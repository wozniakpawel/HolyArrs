package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HolyArrs/holyarrs/app/backend/internal/youtube/youtubetest"
)

// ytJoin signs a new person in through an invite made with invite (the
// invite's JSON body).
func ytJoin(t *testing.T, r *askRig, name, invite string) *signedIn {
	t.Helper()
	var inv struct {
		Path string `json:"path"`
	}
	decodeJSON(t, r.owner.call("POST", "/api/v1/users/invites", invite, 201), &inv)
	jar, _ := cookiejar.New(nil)
	b := &signedIn{t: t, base: r.srv.URL, c: &http.Client{Jar: jar}}
	var x struct {
		CSRF string `json:"csrf"`
	}
	decodeJSON(t, b.call("POST", "/api/v1/auth/invites/"+strings.TrimPrefix(inv.Path, "/invite/"), `{"name":"`+name+`","password":"a long test passphrase"}`, 201), &x)
	b.csrf = x.CSRF
	var me struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, b.call("GET", "/api/v1/auth/me", "", 200), &me)
	b.id = me.ID
	return b
}

type ytStatus struct {
	Missing    []string `json:"missing"`
	FolderSet  bool     `json:"folder_set"`
	FloorBytes int64    `json:"floor_bytes"`
}

// route: GET /api/v1/youtube/status
// route: GET /api/v1/settings/youtube
// route: PUT /api/v1/settings/youtube
func TestYouTubeSettingsAndStatusRoutes(t *testing.T) {
	fake := youtubetest.NewFake(t, "ok")
	t.Setenv("HOLYARRS_YTDLP", fake.Bin)
	t.Setenv("HOLYARRS_FFMPEG", fake.Bin)
	r := newAskRig(t)
	folder := t.TempDir()

	r.owner.call("PUT", "/api/v1/settings/youtube", `{"folder":`+jsonString(folder+"/")+`,"min_free_gb":5}`, 200)
	var got struct {
		Folder    string `json:"folder"`
		MinFreeGB int    `json:"min_free_gb"`
		Totals    struct {
			Videos int `json:"videos"`
		} `json:"totals"`
		Version string `json:"version"`
	}
	decodeJSON(t, r.owner.call("GET", "/api/v1/settings/youtube", "", 200), &got)
	if got.Folder != filepath.Clean(folder) || got.MinFreeGB != 5 || got.Totals.Videos != 0 || got.Version != "2026.10.26" {
		t.Fatalf("settings %+v", got)
	}
	// A field left out keeps its value; a bad folder or floor is refused.
	r.owner.call("PUT", "/api/v1/settings/youtube", `{"min_free_gb":7}`, 200)
	decodeJSON(t, r.owner.call("GET", "/api/v1/settings/youtube", "", 200), &got)
	if got.Folder != filepath.Clean(folder) || got.MinFreeGB != 7 {
		t.Fatalf("partial save %+v", got)
	}
	r.owner.call("PUT", "/api/v1/settings/youtube", `{"folder":"relative/path"}`, 400)
	r.owner.call("PUT", "/api/v1/settings/youtube", `{"folder":`+jsonString(filepath.Join(folder, "missing"))+`}`, 400)
	r.owner.call("PUT", "/api/v1/settings/youtube", `{"min_free_gb":0}`, 400)

	// Admins see the card but may not change it.
	r.owner.call("PUT", "/api/v1/users/"+itoa(r.bo.id), `{"role":"admin"}`, 200)
	r.bo.call("GET", "/api/v1/settings/youtube", "", 200)
	r.bo.call("PUT", "/api/v1/settings/youtube", `{"min_free_gb":9}`, 403)
	r.bo.call("GET", "/api/v1/youtube/status", "", 403) // not ticked

	// A member needs the youtube permission.
	r.ana.call("GET", "/api/v1/youtube/status", "", 403)
	r.ana.call("GET", "/api/v1/settings/youtube", "", 403)
	r.owner.call("PUT", "/api/v1/users/"+itoa(r.ana.id), `{"perms":["ask","youtube"]}`, 200)
	var st ytStatus
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/status", "", 200), &st)
	if st.Missing == nil || len(st.Missing) != 0 || !st.FolderSet || st.FloorBytes != 7<<30 {
		t.Fatalf("status %+v", st)
	}

	// A kid gets nothing, even when ticked. The users screen refuses the
	// tick for a kid, so it goes straight into the row.
	kai := ytJoin(t, r, "kai", `{"max_rating":"PG"}`)
	if err := r.st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET perms = '["ask","youtube"]' WHERE id = ?`, kai.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	kai.call("GET", "/api/v1/youtube/status", "", 404)

	// A program path pointing nowhere counts as missing.
	t.Setenv("HOLYARRS_YTDLP", filepath.Join(t.TempDir(), "nowhere", "yt-dlp"))
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/status", "", 200), &st)
	if !slices.Equal(st.Missing, []string{"yt-dlp"}) {
		t.Fatalf("missing %v", st.Missing)
	}
	decodeJSON(t, r.owner.call("GET", "/api/v1/settings/youtube", "", 200), &got)
	if got.Version != "" {
		t.Fatalf("version without yt-dlp: %q", got.Version)
	}
}

func TestYouTubeRoutesAbsentWhenFlagOff(t *testing.T) {
	t.Setenv("HOLYARRS_FLAGS", "")
	addr, _, _, stop := runApp(t, config{ConfigDir: t.TempDir()})
	defer func() { _ = stop() }()
	jar, _ := cookiejar.New(nil)
	own := &signedIn{t: t, base: "http://" + addr, c: &http.Client{Jar: jar}}
	var x struct {
		CSRF string `json:"csrf"`
	}
	decodeJSON(t, own.call("POST", "/api/v1/auth/setup", `{"name":"luis","password":"a long test passphrase"}`, 201), &x)
	own.csrf = x.CSRF
	own.call("GET", "/api/v1/youtube/status", "", 404)
	own.call("GET", "/api/v1/settings/youtube", "", 404)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
