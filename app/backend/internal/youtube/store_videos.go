package youtube

import (
	"context"
	"database/sql"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
)

// InsertVideo adds a video for a person. When the person already has that
// video it changes nothing and returns created false.
func InsertVideo(tx *sql.Tx, v Video) (id int64, created bool, err error) {
	state := v.State
	if state == "" {
		state = StateQueued
	}
	var uploaded any
	if v.UploadedAt != nil {
		uploaded = database.FormatTime(*v.UploadedAt)
	}
	res, err := tx.Exec(`INSERT INTO youtube_videos
		(user_id, follow_id, video_id, title, channel_name, channel_id, uploaded_at, duration_seconds, quality_max_height, state, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, video_id) DO NOTHING`,
		v.UserID, v.FollowID, v.VideoID, v.Title, v.ChannelName, v.ChannelID, uploaded, v.DurationSeconds, maxHeight(v.MaxHeight), state, database.FormatTime(v.AddedAt))
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	id, err = res.LastInsertId()
	return id, err == nil, err
}

// VideosOf reads a person's videos, newest added first, leaving out removed
// and skipped ones.
func VideosOf(ctx context.Context, q database.Lister, userID int64, f VideoFilter) ([]Video, error) {
	where := "user_id = ? AND state NOT IN ('removed', 'skipped')"
	args := []any{userID}
	if f.FollowID != 0 {
		where += " AND follow_id = ?"
		args = append(args, f.FollowID)
	}
	if f.ChannelID != "" {
		where += " AND channel_id = ?"
		args = append(args, f.ChannelID)
	}
	if f.Unwatched {
		where += " AND watched_at IS NULL"
	}
	where += " ORDER BY added_at DESC, id DESC"
	if f.Limit > 0 || f.Offset > 0 {
		limit := f.Limit
		if limit <= 0 {
			limit = -1
		}
		where += " LIMIT ? OFFSET ?"
		args = append(args, limit, max(f.Offset, 0))
	}
	return database.Select[Video](ctx, q, "youtube_videos", where, args...)
}

// VideoOf reads one of a person's videos; sql.ErrNoRows when it is not
// theirs.
func VideoOf(ctx context.Context, q database.Querier, userID, id int64) (Video, error) {
	return videoWhere(ctx, q, "id = ? AND user_id = ?", id, userID)
}

// VideoByID reads any video row, for the jobs only.
func VideoByID(ctx context.Context, q database.Querier, id int64) (Video, error) {
	return videoWhere(ctx, q, "id = ?", id)
}

func videoWhere(ctx context.Context, q database.Querier, where string, args ...any) (Video, error) {
	var v Video
	err := q.QueryRowContext(ctx, "SELECT "+database.SelectList(&v)+" FROM youtube_videos WHERE "+where, args...).
		Scan(database.ScanInto(&v)...)
	return v, err
}

// KnownVideoIDs is every YouTube video id a person has a row for, whatever
// its state, so a check never adds the same video twice.
func KnownVideoIDs(ctx context.Context, q database.Lister, userID int64) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT video_id FROM youtube_videos WHERE user_id = ?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetVideoState moves a video to state with its error text.
func SetVideoState(tx *sql.Tx, id int64, state, errText string) error {
	_, err := tx.Exec("UPDATE youtube_videos SET state = ?, error = ? WHERE id = ?", state, errText, id)
	return err
}

// FinishVideo records a finished download: its file, size and what yt-dlp
// told us about it.
func FinishVideo(tx *sql.Tx, id int64, rel string, size int64, m Meta) error {
	var uploaded any
	if m.UploadedAt != nil {
		uploaded = database.FormatTime(*m.UploadedAt)
	}
	_, err := tx.Exec(`UPDATE youtube_videos SET state = 'done', error = '', relative_path = ?, size_bytes = ?,
		title = ?, channel_name = ?, channel_id = ?, uploaded_at = ?, duration_seconds = ? WHERE id = ?`,
		rel, size, m.Title, m.ChannelName, m.ChannelID, uploaded, m.DurationSeconds, id)
	return err
}

// SavePosition keeps where a person stopped in one of their videos. Once a
// video is watched it stays watched, keeping the first time.
// sql.ErrNoRows when the video is not theirs.
func SavePosition(tx *sql.Tx, userID, id int64, seconds int, watched bool, at time.Time) error {
	var res sql.Result
	var err error
	if watched {
		res, err = tx.Exec("UPDATE youtube_videos SET position_seconds = ?, watched_at = COALESCE(watched_at, ?) WHERE id = ? AND user_id = ?",
			max(seconds, 0), database.FormatTime(at), id, userID)
	} else {
		res, err = tx.Exec("UPDATE youtube_videos SET position_seconds = ? WHERE id = ? AND user_id = ?", max(seconds, 0), id, userID)
	}
	return oneRow(res, err)
}

// QueuedVideos reads every queued video of every person, oldest first, for
// the jobs only.
func QueuedVideos(ctx context.Context, q database.Lister) ([]Video, error) {
	return database.Select[Video](ctx, q, "youtube_videos", "state = 'queued' ORDER BY id")
}

// KeepLastExcess reads a follow's downloaded videos beyond its n newest by
// upload date, newest of them first. Videos with no upload date count as
// oldest.
func KeepLastExcess(ctx context.Context, q database.Lister, followID int64, n int) ([]Video, error) {
	return database.Select[Video](ctx, q, "youtube_videos",
		"follow_id = ? AND state = 'done' ORDER BY uploaded_at IS NULL, uploaded_at DESC, id DESC LIMIT -1 OFFSET ?", followID, max(n, 0))
}

// VideosOfFollow reads every video row of one of a person's follows,
// whatever its state.
func VideosOfFollow(ctx context.Context, q database.Lister, userID, followID int64) ([]Video, error) {
	return database.Select[Video](ctx, q, "youtube_videos", "user_id = ? AND follow_id = ? ORDER BY id", userID, followID)
}

// DeleteVideosOfFollow removes every video row of one of a person's
// follows, for an unfollow that also deletes the files.
func DeleteVideosOfFollow(tx *sql.Tx, userID, followID int64) error {
	_, err := tx.Exec("DELETE FROM youtube_videos WHERE user_id = ? AND follow_id = ?", userID, followID)
	return err
}

// ReadTotals counts downloaded videos, their bytes and the people who have
// any, across everyone. No titles leave this function.
func ReadTotals(ctx context.Context, q database.Querier) (Totals, error) {
	var t Totals
	err := q.QueryRowContext(ctx, "SELECT count(*), COALESCE(sum(size_bytes), 0), count(DISTINCT user_id) FROM youtube_videos WHERE state = 'done'").
		Scan(&t.Videos, &t.Bytes, &t.People)
	return t, err
}
