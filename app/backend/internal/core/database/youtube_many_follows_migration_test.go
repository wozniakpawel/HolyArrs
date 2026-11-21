package database

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// 0142 lets a YouTube video be in several follows. A video that had a
// follow keeps it as a link; one that had none was added by hand and
// becomes manual; no follow starts out mirrored.
func TestYouTubeManyFollowsMigration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	old, err := Open(dir, Options{MainMigrations: upTo(t, 141)})
	if err != nil {
		t.Fatal(err)
	}
	err = old.Write(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`INSERT INTO users(id, created_at, created_by, updated_at, updated_by, name, role, password_hash) VALUES (1, 't', '{}', 't', '{}', 'owner', 'owner', 'x')`,
			`INSERT INTO youtube_follows(id, user_id, url, kind, yt_id, added_at) VALUES (5, 1, 'u', 'playlist', 'PL1', 't0')`,
			`INSERT INTO youtube_videos(id, user_id, follow_id, video_id, state, added_at) VALUES
				(10, 1, 5, 'followed', 'done', 't1'), (11, 1, NULL, 'one_off', 'done', 't2'), (12, 1, 5, 'skipped', 'skipped', 't3')`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = old.Close()

	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Read().QueryContext(ctx, `SELECT v.id, v.manual, COALESCE(l.follow_id, 0), COALESCE(l.let_go, 0), COALESCE(l.added_at, '')
		FROM youtube_videos v LEFT JOIN youtube_video_follows l ON l.video_row_id = v.id ORDER BY v.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, manual, follow, letGo int
		var at string
		if err := rows.Scan(&id, &manual, &follow, &letGo, &at); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d manual=%d follow=%d let_go=%d at=%s", id, manual, follow, letGo, at))
	}
	want := []string{"10 manual=0 follow=5 let_go=0 at=t1", "11 manual=1 follow=0 let_go=0 at=", "12 manual=0 follow=5 let_go=0 at=t3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("videos after 0142:\n%v\nwant\n%v", got, want)
	}
	var mirror int
	if err := s.Read().QueryRowContext(ctx, "SELECT mirror FROM youtube_follows WHERE id = 5").Scan(&mirror); err != nil || mirror != 0 {
		t.Fatalf("mirror = %d, %v; want off", mirror, err)
	}
	// Removing a follow or a video takes its links along.
	err = s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM youtube_follows WHERE id = 5")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var links int
	if err := s.Read().QueryRowContext(ctx, "SELECT count(*) FROM youtube_video_follows").Scan(&links); err != nil || links != 0 {
		t.Fatalf("%d links left after the follow went, %v", links, err)
	}
}
