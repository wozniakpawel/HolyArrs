-- YouTube library: a video can be in several of a person's playlists and
-- channels and is still one row and one file. youtube_video_follows says
-- which follows hold a video; a follow that let a video go (it keeps only
-- its newest) no longer holds it and never takes it back. manual marks a
-- video the person added by its own link: no follow ever deletes it. A
-- mirrored playlist also removes what leaves the playlist: missing marks a
-- video its last complete listing did not have, and the next complete
-- listing without it removes it.
-- youtube_videos.follow_id is no longer read or written.
CREATE TABLE youtube_video_follows (
  video_row_id INTEGER NOT NULL REFERENCES youtube_videos(id) ON DELETE CASCADE,
  follow_id INTEGER NOT NULL REFERENCES youtube_follows(id) ON DELETE CASCADE,
  let_go INTEGER NOT NULL DEFAULT 0,
  missing INTEGER NOT NULL DEFAULT 0,
  added_at TEXT NOT NULL,
  PRIMARY KEY (video_row_id, follow_id)
);
CREATE INDEX youtube_video_follows_follow ON youtube_video_follows(follow_id);

INSERT INTO youtube_video_follows (video_row_id, follow_id, added_at)
  SELECT id, follow_id, added_at FROM youtube_videos WHERE follow_id IS NOT NULL;

ALTER TABLE youtube_videos ADD COLUMN manual INTEGER NOT NULL DEFAULT 0;
UPDATE youtube_videos SET manual = 1 WHERE follow_id IS NULL;

ALTER TABLE youtube_follows ADD COLUMN mirror INTEGER NOT NULL DEFAULT 0;
