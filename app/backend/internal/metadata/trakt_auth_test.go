package metadata

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/secrets"
)

// fakeTrakt stands in for Trakt's sign-in, token and user routes.
type fakeTrakt struct {
	mu         sync.Mutex
	approved   bool   // the person approved the device code
	deny       bool   // the person said no
	refreshBad bool   // the refresh token is no longer good
	gen        int    // tokens handed out
	valid      string // the access token Trakt still takes
	revoked    []string
	challenge  string // the PKCE challenge of the link the person opened
	sawSecret  bool   // a request carried a client secret
	calls      map[string]int
	srv        *httptest.Server
}

func newFakeTrakt(t *testing.T) *fakeTrakt {
	f := &fakeTrakt{calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[r.URL.Path]++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_secret"] != "" {
			f.sawSecret = true
		}
		tokens := func() {
			f.gen++
			f.valid = "access-" + string(rune('0'+f.gen))
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.valid, "refresh_token": "refresh-" + string(rune('0'+f.gen)),
				"expires_in": 86400})
		}
		switch r.URL.Path {
		case "/oauth/device/code":
			if body["client_id"] != "cid" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dev-1", "user_code": "ABCD1234", "verification_url": "https://trakt.tv/activate", "expires_in": 600, "interval": 5})
		case "/oauth/device/token":
			switch {
			case body["code"] != "dev-1" || body["client_secret"] != "" && body["client_secret"] != "csecret":
				w.WriteHeader(http.StatusUnauthorized)
			case f.deny:
				w.WriteHeader(http.StatusTeapot)
			case !f.approved:
				w.WriteHeader(http.StatusBadRequest)
			default:
				tokens()
			}
		case "/oauth/token":
			if body["grant_type"] == "authorization_code" {
				sum := sha256.Sum256([]byte(body["code_verifier"]))
				if body["code"] != "the-code" || f.challenge == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge ||
					body["redirect_uri"] != "urn:ietf:wg:oauth:2.0:oob" || body["client_id"] != "cid" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"invalid code"}`))
					return
				}
				tokens()
				return
			}
			if f.refreshBad || body["grant_type"] != "refresh_token" || !strings.HasPrefix(body["refresh_token"], "refresh-") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			tokens()
		case "/oauth/revoke":
			f.revoked = append(f.revoked, body["token"])
		case "/users/settings", "/sync/watched/movies":
			if r.Header.Get("Authorization") != "Bearer "+f.valid || r.Header.Get("trakt-api-key") != "cid" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/users/settings" {
				_, _ = w.Write([]byte(`{"user":{"username":"pawel"}}`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func traktAuthRig(t *testing.T, f *fakeTrakt, now *time.Time) *TraktAuth {
	t.Helper()
	st, err := database.Open(t.TempDir(), database.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	v, err := secrets.Open(context.Background(), st, make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return &TraktAuth{Secrets: v, Base: f.srv.URL, Now: func() time.Time { return *now }}
}

// TestTraktDeviceSignIn: the device sign-in needs the app's id and secret,
// asks Trakt no faster than its interval, saves the tokens and username
// once approved, and never shows a token in the status.
func TestTraktDeviceSignIn(t *testing.T) {
	ctx := context.Background()
	f := newFakeTrakt(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a := traktAuthRig(t, f, &now)
	by := database.Actor{Kind: "user", ID: 1}
	if _, err := a.StartSignIn(ctx, 1, by); !errors.Is(err, ErrTraktNoApp) {
		t.Fatalf("no app: %v", err)
	}
	if _, err := a.CheckSignIn(ctx); !errors.Is(err, ErrTraktNoSignIn) {
		t.Fatalf("check before start: %v", err)
	}
	if err := a.Secrets.SetSetting(ctx, TraktKeyGroup, TraktKeyName, "cid", by); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSecret(ctx, "csecret", by); err != nil {
		t.Fatal(err)
	}
	code, err := a.StartSignIn(ctx, 1, by)
	if err != nil || code.UserCode != "ABCD1234" || code.VerificationURL != "https://trakt.tv/activate" || code.Interval != 5 {
		t.Fatalf("start %+v %v", code, err)
	}
	if st := a.Status(ctx); !st.Waiting || st.SignedIn || !st.ClientID || !st.ClientSecret {
		t.Fatalf("status while waiting %+v", st)
	}
	if s, err := a.CheckSignIn(ctx); s != TraktWaiting || err != nil {
		t.Fatalf("first check %q %v", s, err)
	}
	// Asked again within the interval: Trakt is not asked.
	if s, _ := a.CheckSignIn(ctx); s != TraktWaiting || f.calls["/oauth/device/token"] != 1 {
		t.Fatalf("second check %q, Trakt asked %d times", s, f.calls["/oauth/device/token"])
	}
	f.approved = true
	now = now.Add(6 * time.Second)
	if s, err := a.CheckSignIn(ctx); s != TraktSignedIn || err != nil {
		t.Fatalf("approved %q %v", s, err)
	}
	st := a.Status(ctx)
	if !st.SignedIn || st.Username != "pawel" || st.PersonID != 1 || st.Waiting || st.ExpiresAt == nil || !st.ExpiresAt.Equal(time.Date(2026, 11, 21, 12, 0, 6, 0, time.UTC)) {
		t.Fatalf("signed-in status %+v", st)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "access-") || strings.Contains(string(raw), "refresh-") || strings.Contains(string(raw), "csecret") {
		t.Fatalf("status shows a secret: %s", raw)
	}
	if a.Person(ctx) != 1 {
		t.Fatal("person not kept")
	}
	// Denied and expired codes end the wait.
	if _, err := a.StartSignIn(ctx, 1, by); err != nil {
		t.Fatal(err)
	}
	f.deny = true
	now = now.Add(6 * time.Second)
	if s, _ := a.CheckSignIn(ctx); s != TraktDenied {
		t.Fatalf("denied %q", s)
	}
	if _, err := a.StartSignIn(ctx, 1, by); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute)
	if s, _ := a.CheckSignIn(ctx); s != TraktExpired {
		t.Fatalf("expired %q", s)
	}
}

// signedIn signs a in through the fake device flow.
func signedIn(t *testing.T, a *TraktAuth, f *fakeTrakt, now *time.Time) {
	t.Helper()
	ctx := context.Background()
	by := database.Actor{Kind: "user", ID: 1}
	_ = a.Secrets.SetSetting(ctx, TraktKeyGroup, TraktKeyName, "cid", by)
	_ = a.SetSecret(ctx, "csecret", by)
	f.approved = true
	if _, err := a.StartSignIn(ctx, 1, by); err != nil {
		t.Fatal(err)
	}
	if s, err := a.CheckSignIn(ctx); s != TraktSignedIn || err != nil {
		t.Fatalf("sign-in %q %v", s, err)
	}
}

// TestTraktTokenRefresh: a token that ends within the hour is refreshed
// first; a refresh Trakt refuses ends the sign-in with a reason, and the
// next read does not ask Trakt again.
func TestTraktTokenRefresh(t *testing.T) {
	ctx := context.Background()
	f := newFakeTrakt(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a := traktAuthRig(t, f, &now)
	signedIn(t, a, f, &now)
	if id, tok, err := a.Access(ctx); id != "cid" || tok != "access-1" || err != nil {
		t.Fatalf("fresh token %q %q %v", id, tok, err)
	}
	now = now.Add(23*time.Hour + 30*time.Minute)
	if _, tok, err := a.Access(ctx); tok != "access-2" || err != nil || f.calls["/oauth/token"] != 1 {
		t.Fatalf("refreshed token %q %v (%d refreshes)", tok, err, f.calls["/oauth/token"])
	}
	if err := a.Read(ctx, "/sync/watched/movies", &[]any{}); err != nil {
		t.Fatalf("read with the new token: %v", err)
	}
	f.refreshBad = true
	now = now.Add(24 * time.Hour)
	if _, _, err := a.Access(ctx); !errors.Is(err, ErrTraktSignedOut) {
		t.Fatalf("refused refresh: %v", err)
	}
	st := a.Status(ctx)
	if st.SignedIn || st.Problem == "" || a.Person(ctx) != 0 {
		t.Fatalf("after a refused refresh %+v", st)
	}
	if _, tok, err := a.Access(ctx); tok != "" || err != nil || f.calls["/oauth/token"] != 2 {
		t.Fatalf("signed out: token %q %v, %d refreshes", tok, err, f.calls["/oauth/token"])
	}
	if err := a.Read(ctx, "/sync/watched/movies", &[]any{}); !errors.Is(err, ErrTraktSignedOut) {
		t.Fatalf("read signed out: %v", err)
	}
}

// TestTraktRevokedAndSignOut: a token Trakt refuses (revoked at Trakt) ends
// the sign-in; signing out asks Trakt to forget the token and clears it.
func TestTraktRevokedAndSignOut(t *testing.T) {
	ctx := context.Background()
	f := newFakeTrakt(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a := traktAuthRig(t, f, &now)
	signedIn(t, a, f, &now)
	f.valid = "revoked"
	if err := a.Read(ctx, "/sync/watched/movies", &[]any{}); !errors.Is(err, ErrTraktSignedOut) {
		t.Fatalf("revoked token: %v", err)
	}
	if st := a.Status(ctx); st.SignedIn || !strings.Contains(st.Problem, "sign in with Trakt again") {
		t.Fatalf("after revoke %+v", st)
	}
	// Refused for an older token leaves a newer sign-in alone.
	signedIn(t, a, f, &now)
	a.Refused(ctx, "access-1")
	if !a.Status(ctx).SignedIn {
		t.Fatal("an old token's refusal ended a newer sign-in")
	}
	if err := a.SignOut(ctx, database.Actor{Kind: "user", ID: 1}); err != nil {
		t.Fatal(err)
	}
	if len(f.revoked) != 1 || f.revoked[0] != f.valid {
		t.Fatalf("revoked %v", f.revoked)
	}
	if st := a.Status(ctx); st.SignedIn || st.Problem != "" || st.Username != "" {
		t.Fatalf("after sign-out %+v", st)
	}
}

// TestTraktPKCESignIn: an app with only a client id signs in with a PKCE
// link: the link carries an S256 challenge and the out-of-band address,
// the pasted code is traded with the verifier and no client secret, a
// wrong code or an old link is refused, and refreshes send no secret.
func TestTraktPKCESignIn(t *testing.T) {
	ctx := context.Background()
	f := newFakeTrakt(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a := traktAuthRig(t, f, &now)
	a.AuthBase = "https://auth.example"
	by := database.Actor{Kind: "user", ID: 1}
	if _, err := a.StartLink(ctx, 1, by); !errors.Is(err, ErrTraktNoApp) {
		t.Fatalf("no client id: %v", err)
	}
	if err := a.FinishLink(ctx, "the-code"); !errors.Is(err, ErrTraktNoSignIn) {
		t.Fatalf("code with no link: %v", err)
	}
	if err := a.Secrets.SetSetting(ctx, TraktKeyGroup, TraktKeyName, "cid", by); err != nil {
		t.Fatal(err)
	}
	link, err := a.StartLink(ctx, 1, by)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link.URL)
	if err != nil || u.Host != "auth.example" || u.Path != "/oauth/authorize" {
		t.Fatalf("link %q %v", link.URL, err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "cid" || q.Get("redirect_uri") != "urn:ietf:wg:oauth:2.0:oob" ||
		q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("state") == "" {
		t.Fatalf("link query %v", q)
	}
	if again, _ := a.StartLink(ctx, 1, by); again.URL == link.URL {
		t.Fatal("two links share a verifier")
	}
	// The newest link counts; its challenge is the one Trakt saw.
	link, _ = a.StartLink(ctx, 1, by)
	u, _ = url.Parse(link.URL)
	f.challenge = u.Query().Get("code_challenge")
	if !a.Status(ctx).Waiting {
		t.Fatal("not waiting for the code")
	}
	if err := a.FinishLink(ctx, "wrong"); !errors.Is(err, ErrTraktBadCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if err := a.FinishLink(ctx, "  the-code \n"); err != nil {
		t.Fatalf("good code: %v", err)
	}
	st := a.Status(ctx)
	if !st.SignedIn || st.Username != "pawel" || st.ClientSecret || st.Waiting {
		t.Fatalf("status %+v", st)
	}
	if err := a.FinishLink(ctx, "the-code"); !errors.Is(err, ErrTraktNoSignIn) {
		t.Fatalf("a link used twice: %v", err)
	}
	now = now.Add(24 * time.Hour)
	if _, tok, err := a.Access(ctx); tok != "access-2" || err != nil {
		t.Fatalf("refresh without a secret: %q %v", tok, err)
	}
	if f.sawSecret {
		t.Fatal("a client secret was sent though none is saved")
	}
	// A link is good for a while only.
	_, _ = a.StartLink(ctx, 1, by)
	now = now.Add(16 * time.Minute)
	if err := a.FinishLink(ctx, "the-code"); !errors.Is(err, ErrTraktNoSignIn) {
		t.Fatalf("old link: %v", err)
	}
}

// TestTraktDeviceSignInWithoutSecret: the device sign-in works for an app
// with only a client id, and sends no secret.
func TestTraktDeviceSignInWithoutSecret(t *testing.T) {
	ctx := context.Background()
	f := newFakeTrakt(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a := traktAuthRig(t, f, &now)
	if err := a.Secrets.SetSetting(ctx, TraktKeyGroup, TraktKeyName, "cid", database.Actor{Kind: "user", ID: 1}); err != nil {
		t.Fatal(err)
	}
	f.approved = true
	if _, err := a.StartSignIn(ctx, 1, database.Actor{Kind: "user", ID: 1}); err != nil {
		t.Fatal(err)
	}
	if s, err := a.CheckSignIn(ctx); s != TraktSignedIn || err != nil || f.sawSecret {
		t.Fatalf("device sign-in %q %v (secret sent %v)", s, err, f.sawSecret)
	}
}

// The kept Trakt rating fills the title page as a percent, as Trakt shows
// it, only with enough votes and only when the answer has no Trakt score.
func TestWithKeptTraktRating(t *testing.T) {
	r := 8.3347
	got := withKept(TitleExtras{Scores: []OutsideScore{}}, KeptScores{TraktRating: &r, TraktVotes: 52731})
	if len(got.Scores) != 1 || got.Scores[0] != (OutsideScore{Source: "trakt", Value: 83, Max: 100, Votes: 52731}) {
		t.Fatalf("scores %+v", got.Scores)
	}
	if got := withKept(TitleExtras{Scores: []OutsideScore{}}, KeptScores{TraktRating: &r, TraktVotes: 12}); len(got.Scores) != 0 {
		t.Fatalf("too few votes %+v", got.Scores)
	}
	had := OutsideScore{Source: "trakt", Value: 80, Max: 100, Votes: 900}
	if got := withKept(TitleExtras{Scores: []OutsideScore{had}}, KeptScores{TraktRating: &r, TraktVotes: 52731}); len(got.Scores) != 1 || got.Scores[0] != had {
		t.Fatalf("kept over the answer %+v", got.Scores)
	}
}

// TraktRatings reads a rating with the client id alone, never a sign-in.
func TestTraktRatingsClientIDOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("trakt-api-key") != "cid" || r.Header.Get("Authorization") != "" || r.URL.Path != "/shows/tt0903747/ratings" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"rating":9.1,"votes":120000}`))
	}))
	defer srv.Close()
	tr := &TraktRatings{Base: srv.URL, Key: func(context.Context) (string, error) { return "cid", nil }}
	sc, err := tr.Rating(context.Background(), "show", "tt0903747")
	if err != nil || sc != (TraktScore{Rating: 9.1, Votes: 120000}) {
		t.Fatalf("%+v %v", sc, err)
	}
	if _, err := (&TraktRatings{}).Rating(context.Background(), "movie", "tt1"); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("no key: %v", err)
	}
}
