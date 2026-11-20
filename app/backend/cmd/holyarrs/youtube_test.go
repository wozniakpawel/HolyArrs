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

	// A kid gets nothing, ticked or not. The users screen refuses the tick
	// for a kid, so it goes straight into the row.
	kai := ytJoin(t, r, "kai", `{"max_rating":"PG"}`)
	kai.call("GET", "/api/v1/youtube/status", "", 403)
	if err := r.st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET perms = '["ask","youtube","save_files"]' WHERE id = ?`, kai.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	kai.call("GET", "/api/v1/youtube/status", "", 403)
	var me struct {
		Perms []string `json:"perms"`
	}
	decodeJSON(t, kai.call("GET", "/api/v1/auth/me", "", 200), &me)
	if !slices.Contains(me.Perms, "ask") || slices.Contains(me.Perms, "youtube") || slices.Contains(me.Perms, "save_files") {
		t.Fatalf("kid perms %v", me.Perms)
	}

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

type ytVideo struct {
	ID       int64  `json:"id"`
	VideoID  string `json:"video_id"`
	Title    string `json:"title"`
	State    string `json:"state"`
	FollowID *int64 `json:"follow_id"`
}

type ytFollow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	YTID      string `json:"yt_id"`
	Title     string `json:"title"`
	Backfill  *int   `json:"backfill"`
	KeepLast  *int   `json:"keep_last"`
	MaxHeight int    `json:"quality_max_height"`
	Enabled   bool   `json:"enabled"`
}

// ytRig is an askRig with the fake yt-dlp, a YouTube folder and ana ticked
// for youtube.
func ytRig(t *testing.T) *askRig {
	t.Helper()
	fake := youtubetest.NewFake(t, "ok")
	t.Setenv("HOLYARRS_YTDLP", fake.Bin)
	t.Setenv("HOLYARRS_FFMPEG", fake.Bin)
	r := newAskRig(t)
	r.owner.call("PUT", "/api/v1/settings/youtube", `{"folder":`+jsonString(t.TempDir())+`,"min_free_gb":1}`, 200)
	r.owner.call("PUT", "/api/v1/users/"+itoa(r.ana.id), `{"perms":["ask","youtube"]}`, 200)
	return r
}

// route: POST /api/v1/youtube/probe
// route: POST /api/v1/youtube/videos
// route: GET /api/v1/youtube/videos
// route: GET /api/v1/youtube/videos/{id}
// route: DELETE /api/v1/youtube/videos/{id}
// route: GET /api/v1/youtube/follows
// route: POST /api/v1/youtube/follows
// route: PUT /api/v1/youtube/follows/{id}
// route: DELETE /api/v1/youtube/follows/{id}
func TestYouTubeAddRoutes(t *testing.T) {
	r := ytRig(t)
	var p struct {
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Count   int    `json:"count"`
		Entries []struct {
			ID string `json:"id"`
		} `json:"entries"`
	}
	decodeJSON(t, r.ana.call("POST", "/api/v1/youtube/probe", `{"url":"https://www.youtube.com/@veritasium"}`, 200), &p)
	if p.Kind != "channel" || p.Title != "Veritasium" || p.Count != 0 || len(p.Entries) != 2 {
		t.Fatalf("probe %+v", p)
	}
	body := r.ana.call("POST", "/api/v1/youtube/probe", `{"url":"https://evil.example/watch?v=dQw4w9WgXcQ"}`, 400)
	if !strings.Contains(string(body), "paste a link to a YouTube video, playlist or channel") {
		t.Fatalf("bad link: %s", body)
	}

	var v ytVideo
	decodeJSON(t, r.ana.call("POST", "/api/v1/youtube/videos", `{"url":"https://youtu.be/dQw4w9WgXcQ"}`, 201), &v)
	if v.VideoID != "dQw4w9WgXcQ" || v.State != "queued" || v.Title != "Never Gonna Give You Up" {
		t.Fatalf("video %+v", v)
	}
	r.ana.call("POST", "/api/v1/youtube/videos", `{"url":"https://www.youtube.com/@veritasium"}`, 400)

	var f ytFollow
	decodeJSON(t, r.ana.call("POST", "/api/v1/youtube/follows", `{"url":"https://www.youtube.com/@veritasium","keep_last":5}`, 201), &f)
	if f.Kind != "channel" || f.YTID != "UCHnyfMqiRRG1u-2MsSQLbXA" || f.Backfill == nil || *f.Backfill != 5 || f.MaxHeight != 1080 || !f.Enabled {
		t.Fatalf("follow %+v", f)
	}
	var dup struct {
		Error      string `json:"error"`
		ExistingID int64  `json:"existing_id"`
	}
	decodeJSON(t, r.ana.call("POST", "/api/v1/youtube/follows", `{"url":"https://www.youtube.com/channel/UCHnyfMqiRRG1u-2MsSQLbXA"}`, 409), &dup)
	if dup.Error != "you already follow this" || dup.ExistingID != f.ID {
		t.Fatalf("duplicate %+v", dup)
	}

	var follows []ytFollow
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/follows", "", 200), &follows)
	if len(follows) != 1 || follows[0].ID != f.ID {
		t.Fatalf("follows %+v", follows)
	}
	decodeJSON(t, r.ana.call("PUT", "/api/v1/youtube/follows/"+itoa(f.ID), `{"enabled":false,"keep_last":null,"quality_max_height":720}`, 200), &f)
	if f.Enabled || f.KeepLast != nil || f.MaxHeight != 720 {
		t.Fatalf("updated %+v", f)
	}
	r.ana.call("PUT", "/api/v1/youtube/follows/"+itoa(f.ID), `{"keep_last":0}`, 400)

	var list []ytVideo
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/videos?unwatched=1&limit=10&offset=0", "", 200), &list)
	if len(list) != 1 || list[0].ID != v.ID {
		t.Fatalf("list %+v", list)
	}
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/videos?follow="+itoa(f.ID), "", 200), &list)
	if len(list) != 0 {
		t.Fatalf("follow filter %+v", list)
	}
	r.ana.call("GET", "/api/v1/youtube/videos?limit=x", "", 400)
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/videos/"+itoa(v.ID), "", 200), &v)

	r.ana.call("DELETE", "/api/v1/youtube/follows/"+itoa(f.ID), "", 400)
	r.ana.call("DELETE", "/api/v1/youtube/follows/"+itoa(f.ID)+"?files=keep", "", 204)
	r.ana.call("DELETE", "/api/v1/youtube/follows/"+itoa(f.ID)+"?files=keep", "", 404)
	r.ana.call("DELETE", "/api/v1/youtube/videos/"+itoa(v.ID), "", 204)
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/videos", "", 200), &list)
	if len(list) != 0 {
		t.Fatalf("removed video still listed %+v", list)
	}

	// Without the programs nothing can be looked up.
	t.Setenv("HOLYARRS_FFMPEG", filepath.Join(t.TempDir(), "nowhere"))
	body = r.ana.call("POST", "/api/v1/youtube/probe", `{"url":"https://youtu.be/dQw4w9WgXcQ"}`, 503)
	if !strings.Contains(string(body), "needs yt-dlp and ffmpeg") {
		t.Fatalf("no tools: %s", body)
	}
	// A member without the permission gets nothing.
	r.bo.call("GET", "/api/v1/youtube/videos", "", 403)
}

func TestOtherPersonsRowsAre404(t *testing.T) {
	r := ytRig(t)
	var own, mine ytVideo
	decodeJSON(t, r.owner.call("POST", "/api/v1/youtube/videos", `{"url":"https://youtu.be/dQw4w9WgXcQ"}`, 201), &own)
	decodeJSON(t, r.ana.call("POST", "/api/v1/youtube/videos", `{"url":"https://www.youtube.com/watch?v=xxxxxxxxxx1"}`, 201), &mine)
	var follow ytFollow
	decodeJSON(t, r.owner.call("POST", "/api/v1/youtube/follows", `{"url":"https://www.youtube.com/@veritasium"}`, 201), &follow)

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/youtube/videos/" + itoa(own.ID)},
		{"DELETE", "/api/v1/youtube/videos/" + itoa(own.ID)},
		{"PUT", "/api/v1/youtube/follows/" + itoa(follow.ID)},
		{"DELETE", "/api/v1/youtube/follows/" + itoa(follow.ID) + "?files=delete"},
		{"DELETE", "/api/v1/youtube/follows/" + itoa(follow.ID) + "?files=keep"},
	} {
		body := r.ana.call(c.method, c.path, `{"enabled":false}`, 404)
		for _, secret := range []string{"Never Gonna", "Rick Astley", "dQw4w9WgXcQ", "Veritasium", "UCHnyfMqiRRG1u"} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("%s %s leaks %q: %s", c.method, c.path, secret, body)
			}
		}
	}
	r.owner.call("GET", "/api/v1/youtube/videos/"+itoa(mine.ID), "", 404)
	r.owner.call("DELETE", "/api/v1/youtube/videos/"+itoa(mine.ID), "", 404)

	// Nothing changed for the owner.
	var v ytVideo
	decodeJSON(t, r.owner.call("GET", "/api/v1/youtube/videos/"+itoa(own.ID), "", 200), &v)
	var follows []ytFollow
	decodeJSON(t, r.owner.call("GET", "/api/v1/youtube/follows", "", 200), &follows)
	if v.State != "queued" || len(follows) != 1 || !follows[0].Enabled {
		t.Fatalf("owner rows changed: %+v %+v", v, follows)
	}

	var list []ytVideo
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/videos", "", 200), &list)
	if len(list) != 1 || list[0].ID != mine.ID {
		t.Fatalf("ana sees %+v", list)
	}
	decodeJSON(t, r.ana.call("GET", "/api/v1/youtube/follows", "", 200), &follows)
	if len(follows) != 0 {
		t.Fatalf("ana sees follows %+v", follows)
	}
	// Following the same channel is her own follow, not a duplicate.
	r.ana.call("POST", "/api/v1/youtube/follows", `{"url":"https://www.youtube.com/@veritasium"}`, 201)
}
