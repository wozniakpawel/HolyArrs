package youtube

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/jobs"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/webserver"
)

// Check timings.
const (
	// updateEvery is how often a check lets yt-dlp update itself.
	updateEvery = 24 * time.Hour
	// lateGrace is how far before the previous check a channel video may be
	// dated and still be taken as new: approximate dates and late uploads
	// to the Live and Shorts tabs fall inside it.
	lateGrace = 7 * 24 * time.Hour
)

// HandleCheck runs a youtube_check job: it lets yt-dlp update itself once a
// day, lists the follows and records what is new, then queues a download
// for every queued video without one. With params {"follow_id"} it lists
// that follow only, else every enabled follow of people who may still use
// YouTube. Job notes and results carry counts and ids only.
func (s *Service) HandleCheck(ctx context.Context, r *jobs.Run) error {
	run := s.runner()
	if len(run.Tools.Missing()) > 0 {
		r.SetResult(map[string]any{"skipped": "tools_missing"})
		return nil
	}
	s.selfUpdate(ctx, run)

	follows, err := EnabledFollows(ctx, s.Store.Read())
	if err != nil {
		return err
	}
	if id, ok := paramID(r.Job.Params, "follow_id"); ok {
		follows = slices.DeleteFunc(follows, func(f Follow) bool { return f.ID != id })
	}
	may := s.mayUse()
	var checked, failed, queued int
	for i, f := range follows {
		ok, err := may(ctx, f.UserID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := r.Progress(ctx, float64(i)/float64(len(follows)), fmt.Sprintf("%d of %d", i+1, len(follows))); err != nil {
			return err
		}
		n, err := s.checkFollow(ctx, run, f)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var listing listingError
		if err != nil && !errors.As(err, &listing) {
			return err
		}
		checked++
		queued += n
		if err != nil {
			failed++
		}
	}
	downloads, err := s.queueDownloads(ctx, may)
	if err != nil {
		return err
	}
	r.SetResult(map[string]any{"follows": checked, "failed": failed, "queued": queued, "downloads": downloads})
	return nil
}

// selfUpdate runs "yt-dlp -U" when the last try is a day old or more, and
// keeps when it ran and how it failed. A failure never stops the check.
func (s *Service) selfUpdate(ctx context.Context, run Runner) {
	set := LoadSettings(ctx, s.Store.Read())
	now := s.now()
	if last, err := time.Parse(database.TimeLayout, set.UpdatedAt); err == nil && now.Sub(last) < updateEvery {
		return
	}
	errText := ""
	if _, err := run.SelfUpdate(ctx); err != nil {
		errText = err.Error()
	}
	err := s.Store.Write(ctx, func(tx *sql.Tx) error {
		if err := database.PutSetting(tx, settingsGroup, keyYTDLPUpdatedAt, database.FormatTime(now), database.System, now); err != nil {
			return err
		}
		return database.PutSetting(tx, settingsGroup, keyYTDLPUpdateErr, errText, database.System, now)
	})
	if err != nil {
		log.Printf("youtube: yt-dlp update not recorded: %v", err)
	}
}

// mayUse answers whether a person may still use YouTube, reading each
// person once: not removed, not a kid and holding the youtube permission.
func (s *Service) mayUse() func(ctx context.Context, userID int64) (bool, error) {
	seen := map[int64]bool{}
	return func(ctx context.Context, userID int64) (bool, error) {
		if ok, done := seen[userID]; done {
			return ok, nil
		}
		p, err := s.Person(ctx, userID)
		if errors.Is(err, sql.ErrNoRows) {
			seen[userID] = false
			return false, nil
		}
		if err != nil {
			return false, err
		}
		ok := !p.Removed && p.MaxRating == "" && slices.Contains(p.Perms, webserver.PermYouTube)
		seen[userID] = ok
		return ok, nil
	}
}

// checkFollow lists one follow and records every entry: a video the person
// does not have yet is queued or skipped, and one they already have is
// linked to the follow instead of downloaded again. It returns how many
// were queued. A listing that fails is kept on the follow with yt-dlp's
// error line and changes nothing else. A mirrored playlist whose listing
// is complete then lets go of the videos that left it.
func (s *Service) checkFollow(ctx context.Context, run Runner, f Follow) (int, error) {
	now := s.now()
	listing, err := run.Listing(ctx, Link{Kind: f.Kind, ID: f.YTID}, f.Kind == KindChannel && f.IncludeShorts)
	if err != nil {
		if ctx.Err() == nil {
			// Only the error: the 7-day window and the first-check backfill
			// count from the last listing that worked.
			if werr := s.Store.Write(ctx, func(tx *sql.Tx) error { return MarkCheckFailed(tx, f.ID, err.Error()) }); werr != nil {
				return 0, werr
			}
		}
		return 0, listingError{err}
	}
	// Everything the listing names is still in the playlist, live or not.
	listed := map[string]bool{}
	for _, e := range listing.Entries {
		listed[e.ID] = true
	}
	entries := slices.DeleteFunc(listing.Entries, func(e Entry) bool {
		return e.LiveStatus == "is_live" || e.LiveStatus == "is_upcoming" || e.LiveStatus == "post_live"
	})
	known, err := KnownVideos(ctx, s.Store.Read(), f.UserID)
	if err != nil {
		return 0, err
	}
	links, err := FollowLinks(ctx, s.Store.Read(), f.UserID, f.ID)
	if err != nil {
		return 0, err
	}
	states := plan(f, entries)
	queued := 0
	err = s.Store.Write(ctx, func(tx *sql.Tx) error {
		queued = 0
		seen := map[string]bool{}
		for i, e := range entries {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			if k, ok := known[e.ID]; ok {
				_, linked := links[k.ID]
				if !linked {
					if err := LinkVideo(tx, f.UserID, k.ID, f.ID, now); err != nil {
						return err
					}
				}
				// A video another follow passed over is taken when this
				// one wants it: on first sight, or always for a mirror.
				if k.State == StateSkipped && states[i] == StateQueued && (f.Mirror || !linked) {
					if err := RequeueVideo(tx, f.UserID, k.ID, f.MaxHeight); err != nil {
						return err
					}
					queued++
				}
				continue
			}
			v := Video{UserID: f.UserID, FollowIDs: []int64{f.ID}, VideoID: e.ID, Title: e.Title, DurationSeconds: e.DurationSeconds,
				MaxHeight: f.MaxHeight, State: states[i], AddedAt: now}
			if f.Kind == KindChannel {
				v.ChannelName, v.ChannelID = f.Title, f.YTID
			}
			if e.Timestamp > 0 {
				t := time.Unix(e.Timestamp, 0).UTC()
				v.UploadedAt = &t
			}
			if _, created, err := InsertVideo(tx, v); err != nil {
				return err
			} else if created && v.State == StateQueued {
				queued++
			}
		}
		return MarkChecked(tx, f.ID, now, "")
	})
	if err != nil || !f.Mirror || f.Kind != KindPlaylist || !listing.Complete {
		return queued, err
	}
	return queued, s.mirror(ctx, f, listed)
}

// mirror lets a mirrored playlist go of every video it holds that is no
// longer in the playlist. Only a complete listing gets here.
func (s *Service) mirror(ctx context.Context, f Follow, listed map[string]bool) error {
	held, err := VideosOfFollow(ctx, s.Store.Read(), f.UserID, f.ID)
	if err != nil {
		return err
	}
	root := LoadSettings(ctx, s.Store.Read()).Folder
	for _, v := range held {
		if listed[v.VideoID] {
			continue
		}
		if err := s.letGo(ctx, root, v, f.ID, true); err != nil {
			return err
		}
	}
	return nil
}

// letGo takes a video away from one of its follows. With unlink the follow
// forgets it (a mirrored playlist it left); without, the follow marks it
// let go (it keeps only its newest). The files go only when the video is
// not manual and no other follow holds it: then the row goes too when
// unlinked, and else stays as removed so no check downloads it again.
func (s *Service) letGo(ctx context.Context, root string, v Video, followID int64, unlink bool) error {
	held, err := Held(ctx, s.Store.Read(), v.UserID, v.ID, followID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !held {
		if err := removeFiles(root, v.RelativePath); err != nil {
			return err
		}
	}
	var gone, again bool
	err = s.Store.Write(ctx, func(tx *sql.Tx) error {
		gone, again = false, false
		var err error
		if unlink {
			err = UnlinkVideo(tx, v.UserID, v.ID, followID)
		} else {
			err = LetGoVideo(tx, v.UserID, v.ID, followID)
		}
		if err != nil || held {
			return err
		}
		if unlink {
			gone, err = DeleteVideoIfUnheld(tx, v.UserID, v.ID)
		} else {
			gone, err = RemoveVideoIfUnheld(tx, v.UserID, v.ID)
		}
		if err != nil || gone || v.State != StateDone {
			return err
		}
		// Something took hold of the video after its files went: fetch
		// them again.
		again = true
		err = RequeueVideo(tx, v.UserID, v.ID, v.MaxHeight)
		if errors.Is(err, sql.ErrNoRows) {
			again, err = false, nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if gone {
		s.publish(v.UserID, LiveVideo{ID: v.ID, State: StateRemoved})
	}
	if again {
		s.queueDownload(ctx, v.UserID, v.ID)
	}
	return nil
}

// listingError is a follow whose listing failed: kept on the follow, and
// the check goes on with the next one.
type listingError struct{ error }

func (e listingError) Unwrap() error { return e.error }

// plan sorts entries in place, newest first, on a follow's first check (the
// caller pairs entries[i] with the returned states[i]) and says, per
// entry, whether a new one is queued or skipped. The first check takes the
// follow's backfill (nil takes all) and skips the rest. Later checks queue
// what is new, except channel videos dated more than lateGrace before the
// previous listing that worked: the back catalogue, such as Shorts once they are turned
// on. Entries with no date are queued. A mirror takes every entry.
func plan(f Follow, entries []Entry) []string {
	states := make([]string, len(entries))
	if f.Mirror {
		for i := range states {
			states[i] = StateQueued
		}
		return states
	}
	if f.LastCheckedAt == nil {
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Timestamp > entries[j].Timestamp })
		for i := range entries {
			states[i] = StateQueued
			if f.Backfill != nil && i >= *f.Backfill {
				states[i] = StateSkipped
			}
		}
		return states
	}
	oldest := f.LastCheckedAt.Add(-lateGrace).Unix()
	for i, e := range entries {
		states[i] = StateQueued
		if f.Kind == KindChannel && e.Timestamp > 0 && e.Timestamp < oldest {
			states[i] = StateSkipped
		}
	}
	return states
}

// queueDownloads queues a download for every queued video of a person who
// may use YouTube. A video that already has a live job keeps it: the job's
// dedupe key is the same.
func (s *Service) queueDownloads(ctx context.Context, may func(context.Context, int64) (bool, error)) (int, error) {
	rows, err := QueuedVideos(ctx, s.Store.Read())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range rows {
		ok, err := may(ctx, v.UserID)
		if err != nil {
			return n, err
		}
		if ok {
			s.queueDownload(ctx, v.UserID, v.ID)
			n++
		}
	}
	return n, nil
}

// paramID reads a whole-number id from job params; JSON gives numbers back
// as float64.
func paramID(params map[string]any, key string) (int64, bool) {
	switch v := params[key].(type) {
	case float64:
		return int64(v), v > 0 && v == float64(int64(v))
	case int64:
		return v, v > 0
	case int:
		return int64(v), v > 0
	}
	return 0, false
}
