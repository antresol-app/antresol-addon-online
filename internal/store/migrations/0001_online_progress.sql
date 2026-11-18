-- «Смотреть онлайн» watch progress, by TMDB title (kind 'movie' or
-- 'series'; season and episode are 0 for a movie), per profile by its uid
-- (a profile's id may be given to a new one once it is deleted, a uid
-- never; the addon's database has no users table, so no foreign key: a
-- deleted profile's rows go when the profiles no longer list it).
-- source, voice and quality are the last choices of the picker (plain
-- hints, never refs). A title that is in the library also gets its
-- progress row mirrored.
CREATE TABLE online_progress (
  user_uid TEXT NOT NULL,
  kind TEXT NOT NULL,
  tmdb_id INTEGER NOT NULL,
  season INTEGER NOT NULL DEFAULT 0,
  episode INTEGER NOT NULL DEFAULT 0,
  position_ms INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  finished INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL DEFAULT '',
  voice TEXT NOT NULL DEFAULT '',
  quality TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(user_uid, kind, tmdb_id, season, episode)
);
CREATE INDEX online_progress_user_updated ON online_progress(user_uid, updated_at);
