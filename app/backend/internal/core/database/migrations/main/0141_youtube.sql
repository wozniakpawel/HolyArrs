-- YouTube library: the playlists and channels each person follows, and the
-- videos HolyArrs keeps for them. Every row belongs to one person; the same
-- video followed by two people is two rows and two files. One-off videos
-- have no follow row (follow_id is NULL).
CREATE TABLE youtube_follows (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  url TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('playlist', 'channel')),
  yt_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  quality_max_height INTEGER NOT NULL DEFAULT 1080,
  include_shorts INTEGER NOT NULL DEFAULT 0,
  -- NULL keeps every video.
  keep_last INTEGER,
  -- How many existing videos to take on the first check; NULL takes all.
  backfill INTEGER,
  enabled INTEGER NOT NULL DEFAULT 1,
  added_at TEXT NOT NULL,
  last_checked_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  UNIQUE (user_id, kind, yt_id)
);

CREATE TABLE youtube_videos (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  follow_id INTEGER REFERENCES youtube_follows(id) ON DELETE SET NULL,
  video_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  channel_name TEXT NOT NULL DEFAULT '',
  channel_id TEXT NOT NULL DEFAULT '',
  uploaded_at TEXT,
  duration_seconds INTEGER NOT NULL DEFAULT 0,
  quality_max_height INTEGER NOT NULL DEFAULT 1080,
  -- Relative to the YouTube folder, with forward slashes.
  relative_path TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'downloading', 'done', 'failed', 'skipped', 'removed')),
  error TEXT NOT NULL DEFAULT '',
  position_seconds INTEGER NOT NULL DEFAULT 0,
  watched_at TEXT,
  added_at TEXT NOT NULL,
  UNIQUE (user_id, video_id)
);
CREATE INDEX youtube_videos_follow ON youtube_videos(follow_id);
CREATE INDEX youtube_videos_state ON youtube_videos(state);

INSERT INTO schedules (created_at, created_by, updated_at, updated_by, kind, every_seconds, enabled) VALUES
    (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), '{"kind":"system"}', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), '{"kind":"system"}', 'youtube_check', 21600, 1)
ON CONFLICT (kind) DO NOTHING;
