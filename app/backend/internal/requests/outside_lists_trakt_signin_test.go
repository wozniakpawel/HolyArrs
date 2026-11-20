package requests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	domain "github.com/HolyArrs/holyarrs/app/backend/internal/core/model"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/secrets"
	"github.com/HolyArrs/holyarrs/app/backend/internal/metadata"
)

// fakePrivateTrakt serves kim's private watchlist only with the access
// token it handed out at sign-in; good says whether Trakt still takes it.
func fakePrivateTrakt(t *testing.T, good *atomic.Bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dev", "user_code": "CODE", "verification_url": "https://trakt.tv/activate", "expires_in": 600, "interval": 1})
			return
		case "/oauth/device/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-ok", "refresh_token": "ref-ok", "expires_in": 86400})
			return
		case "/users/settings":
			_, _ = w.Write([]byte(`{"user":{"username":"kim"}}`))
			return
		}
		if r.Header.Get("trakt-api-key") != fakeTraktClient {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		auth := r.Header.Get("Authorization")
		switch {
		case auth == "":
			w.WriteHeader(http.StatusNotFound) // private: hidden without the sign-in
			return
		case auth != "Bearer tok-ok" || !good.Load():
			w.WriteHeader(http.StatusUnauthorized)
			return
		case r.URL.Path != "/users/kim/watchlist/movies":
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Pagination-Page-Count", "1")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "movie", "movie": map[string]any{"title": "Quiet Orchard", "ids": map[string]any{"tmdb": 900003}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSync_PrivateTraktListWithSignIn: signed in with Trakt, a private
// watchlist is read with the access token. When Trakt refuses the token
// (revoked), the read fails saying to sign in again, the sign-in ends, and
// the next read is a plain public one that cannot see the private list.
func TestSync_PrivateTraktListWithSignIn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	good := &atomic.Bool{}
	good.Store(true)
	srv := fakePrivateTrakt(t, good)
	v, err := secrets.Open(ctx, e.s, make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	by := database.Actor{Kind: "user", ID: 1}
	auth := &metadata.TraktAuth{Secrets: v, Base: srv.URL}
	if err := v.SetSetting(ctx, metadata.TraktKeyGroup, metadata.TraktKeyName, fakeTraktClient, by); err != nil {
		t.Fatal(err)
	}
	if err := auth.SetSecret(ctx, "secret", by); err != nil {
		t.Fatal(err)
	}
	e.svc.TraktBase, e.svc.TraktAuth = srv.URL, auth
	l, _ := AddOutsideList(ctx, e.s, OutsideListInput{Name: "Mine", Kind: ListTrakt, Ref: "kim/watchlist"}, domain.System)
	// Not signed in: the private watchlist is not there.
	if _, err := e.svc.Sync(ctx, l.ID, domain.System); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("public read of a private list: %v", err)
	}
	if _, err := auth.StartSignIn(ctx, 1, by); err != nil {
		t.Fatal(err)
	}
	if s, err := auth.CheckSignIn(ctx); s != metadata.TraktSignedIn || err != nil {
		t.Fatalf("sign-in %q %v", s, err)
	}
	if res, err := e.svc.Sync(ctx, l.ID, domain.System); err != nil || res.Added != 1 {
		t.Fatalf("signed-in read %+v %v", res, err)
	}
	good.Store(false)
	if _, err := e.svc.Sync(ctx, l.ID, domain.System); !errors.Is(err, metadata.ErrTraktSignedOut) {
		t.Fatalf("revoked token: %v", err)
	}
	got, _ := GetOutsideList(ctx, e.s.Read(), l.ID)
	if !strings.Contains(got.LastError, "sign in with Trakt again") || strings.Contains(got.LastError, "tok-ok") {
		t.Fatalf("list error %q", got.LastError)
	}
	if st := auth.Status(ctx); st.SignedIn {
		t.Fatalf("still signed in after a refused token: %+v", st)
	}
	if _, err := e.svc.Sync(ctx, l.ID, domain.System); err == nil || errors.Is(err, metadata.ErrTraktSignedOut) {
		t.Fatalf("read after sign-out: %v", err)
	}
}

// TestTraktSignInRoutes: the sign-in routes answer the status without any
// secret, hand out a PKCE link, and refuse a code with no link or no body.
func TestTraktSignInRoutes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v, err := secrets.Open(ctx, e.s, make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	h := &OutsideListHandlers{Store: e.s, Queue: e.q, Secrets: v, TraktAuth: &metadata.TraktAuth{Secrets: v, AuthBase: "https://auth.example"},
		Actor: func(*http.Request) domain.Actor { return domain.Actor{Kind: "user", ID: 1} }}
	m := http.NewServeMux()
	h.TraktKeyRoutes(m)
	h.TraktSignInRoutes(m)
	call := func(method, path, body string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := call("POST", "/api/v1/settings/trakt-signin/link", ""); code != http.StatusConflict || !strings.Contains(body, "client id") {
		t.Fatalf("link with no client id: %d %s", code, body)
	}
	if code, _ := call("PUT", "/api/v1/settings/trakt-key", `{"key":"cid-123456789"}`); code != http.StatusOK {
		t.Fatalf("key %d", code)
	}
	if code, body := call("PUT", "/api/v1/settings/trakt-secret", `{"secret":"sec-123456789"}`); code != http.StatusOK || strings.Contains(body, "sec-123456789") || !strings.Contains(body, `"client_secret":true`) {
		t.Fatalf("secret %d %s", code, body)
	}
	if code, body := call("PUT", "/api/v1/settings/trakt-secret", `{"secret":""}`); code != http.StatusOK || !strings.Contains(body, `"client_secret":false`) {
		t.Fatalf("secret removed %d %s", code, body)
	}
	if code, body := call("POST", "/api/v1/settings/trakt-signin/code", `{"code":"abc"}`); code != http.StatusConflict {
		t.Fatalf("code with no link: %d %s", code, body)
	}
	code, body := call("POST", "/api/v1/settings/trakt-signin/link", "")
	if code != http.StatusOK || !strings.Contains(body, "https://auth.example/oauth/authorize?") || !strings.Contains(body, "code_challenge_method=S256") {
		t.Fatalf("link %d %s", code, body)
	}
	if code, _ := call("POST", "/api/v1/settings/trakt-signin/code", `{}`); code != http.StatusBadRequest {
		t.Fatalf("empty code %d", code)
	}
	code, body = call("GET", "/api/v1/settings/trakt-signin", "")
	if code != http.StatusOK || !strings.Contains(body, `"waiting":true`) || !strings.Contains(body, `"signed_in":false`) || strings.Contains(body, "cid-123456789") {
		t.Fatalf("status %d %s", code, body)
	}
	if code, _ := call("DELETE", "/api/v1/settings/trakt-signin", ""); code != http.StatusOK {
		t.Fatalf("sign out %d", code)
	}
}
