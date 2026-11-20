package youtube

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/jobs"
)

func TestForgetPersonRemovesRowsAndFolder(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	write(t, s, func(tx *sql.Tx) error {
		for _, q := range []string{
			`INSERT INTO users(id, created_at, created_by, updated_at, updated_by, name, role, password_hash) VALUES (7, 't', '{}', 't', '{}', 'Anna', 'member', 'x')`,
			`INSERT INTO users(id, created_at, created_by, updated_at, updated_by, name, role, password_hash) VALUES (8, 't', '{}', 't', '{}', 'Piotr', 'member', 'x')`,
			`INSERT INTO users(id, created_at, created_by, updated_at, updated_by, name, role, password_hash) VALUES (70, 't', '{}', 't', '{}', 'Ola', 'member', 'x')`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	root := t.TempDir()
	write(t, s, func(tx *sql.Tx) error {
		return database.PutSetting(tx, settingsGroup, keyFolder, root, database.System, time.Now())
	})
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	for _, f := range []string{
		filepath.Join(root, "7-Anna", "Watch later", "a.mp4"),
		filepath.Join(root, "8-Piotr", "Watch later", "b.mp4"),
		filepath.Join(root, "70-Ola", "c.mp4"),
		keep,
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A link inside Anna's folder points outside the YouTube folder; only
	// the link goes, never what it points at.
	if err := os.Symlink(outside, filepath.Join(root, "7-Anna", "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "7-Link")); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	fA := addFollow(t, s, Follow{UserID: 7, URL: "https://www.youtube.com/playlist?list=PLa", Kind: KindPlaylist, YTID: "PLa", Enabled: true, AddedAt: now})
	vA := addVideo(t, s, Video{UserID: 7, FollowID: &fA, VideoID: "aaaaaaaaaaa"})
	fB := addFollow(t, s, Follow{UserID: 8, URL: "https://www.youtube.com/playlist?list=PLb", Kind: KindPlaylist, YTID: "PLb", Enabled: true, AddedAt: now})
	vB := addVideo(t, s, Video{UserID: 8, FollowID: &fB, VideoID: "bbbbbbbbbbb"})
	vO := addVideo(t, s, Video{UserID: 70, VideoID: "ooooooooooo"})

	svc := testService(s)
	svc.Queue = jobs.New(s, jobs.Options{})
	enqueue := func(kind, key string) int64 {
		id, _, err := svc.Queue.Enqueue(ctx, kind, nil, map[string]any{}, key, database.System)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	jA := enqueue(KindDownload, "youtube:7:"+itoa(vA))
	jAF := enqueue(KindCheck, "youtube:7:follow:"+itoa(fA))
	jB := enqueue(KindDownload, "youtube:8:"+itoa(vB))
	jO := enqueue(KindDownload, "youtube:70:"+itoa(vO))

	if err := svc.ForgetPerson(ctx, 7); err != nil {
		t.Fatal(err)
	}

	count := func(table string, userID int64) int {
		var n int
		if err := s.Read().QueryRow("SELECT count(*) FROM "+table+" WHERE user_id = ?", userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("youtube_videos", 7) != 0 || count("youtube_follows", 7) != 0 {
		t.Fatal("Anna's rows are still there")
	}
	if count("youtube_videos", 8) != 1 || count("youtube_follows", 8) != 1 || count("youtube_videos", 70) != 1 {
		t.Fatal("another person's rows went too")
	}
	state := func(id int64) string {
		var st string
		if err := s.Read().QueryRow("SELECT state FROM jobs WHERE id = ?", id).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if state(jA) != "cancelled" || state(jAF) != "cancelled" {
		t.Fatalf("Anna's jobs: %s %s", state(jA), state(jAF))
	}
	if state(jB) != "queued" || state(jO) != "queued" {
		t.Fatalf("other jobs: %s %s", state(jB), state(jO))
	}
	for _, gone := range []string{"7-Anna", "7-Link"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", gone, err)
		}
	}
	for _, f := range []string{filepath.Join(root, "8-Piotr", "Watch later", "b.mp4"), filepath.Join(root, "70-Ola", "c.mp4"), keep} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s went: %v", f, err)
		}
	}

	// A second forget, or one for a person with nothing, is fine.
	if err := svc.ForgetPerson(ctx, 7); err != nil {
		t.Fatal(err)
	}
}

func TestForgetPersonWithoutFolder(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	addVideo(t, s, Video{UserID: 2, VideoID: "ccccccccccc"})
	svc := testService(s)
	svc.Queue = jobs.New(s, jobs.Options{})
	if err := svc.ForgetPerson(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.Read().QueryRow("SELECT count(*) FROM youtube_videos WHERE user_id = 2").Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows left", n)
	}
}

func TestForgetPersonKeepsTitlesOutOfErrors(t *testing.T) {
	t.Parallel()
	err := withoutPath(&os.PathError{Op: "unlinkat", Path: "/yt/7-Anna/Watch later/Secret title [abc].mp4", Err: os.ErrPermission})
	if err == nil || strings.Contains(err.Error(), "Secret") || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error %v", err)
	}
}
