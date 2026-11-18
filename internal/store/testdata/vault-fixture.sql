-- A small vault.db for the one-time import tests: the core's users table
-- with the uid column (core migration 0093), the settings row app_settings
-- with «Смотреть онлайн» off, and online_progress as the core's online
-- migrations 0027-0029 left it (keyed by user_id, with its foreign key).
-- Three profiles have rows; user 9 is gone (an orphan row, written while
-- foreign keys were off). The import tests open it in WAL mode.
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  avatar        TEXT NOT NULL,
  password_hash TEXT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  is_kid        INTEGER NOT NULL DEFAULT 0,
  max_rating    TEXT NULL,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
, uid TEXT);
CREATE UNIQUE INDEX users_uid ON users(uid);

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE online_progress (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
  title TEXT NOT NULL DEFAULT '',
  rating TEXT NOT NULL DEFAULT '',
  year INTEGER NOT NULL DEFAULT 0,
  poster TEXT NOT NULL DEFAULT '',
  next_season INTEGER NOT NULL DEFAULT 0,
  next_episode INTEGER NOT NULL DEFAULT 0,
  last_listed INTEGER NOT NULL DEFAULT 0,
  in_library INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(user_id, kind, tmdb_id, season, episode)
);
CREATE INDEX online_progress_user_updated ON online_progress(user_id, updated_at);

INSERT INTO users (id, name, avatar, is_admin, uid) VALUES
  (1, 'Admin', 'a', 1, 'uid-admin'),
  (2, 'Second', 'b', 0, 'uid-second'),
  (3, 'Third', 'c', 0, 'uid-third'),
  (4, 'NoRows', 'd', 0, 'uid-norows');

INSERT INTO settings (key, value) VALUES
  ('app_settings', '{"remoteQuality":"720","onlineEnabled":false,"noSeeding":true}');

INSERT INTO online_progress (user_id, kind, tmdb_id, season, episode, position_ms, duration_ms, finished,
  source, voice, quality, updated_at, title, rating, year, poster, next_season, next_episode, last_listed, in_library) VALUES
  (1, 'movie', 100, 0, 0, 60000, 6000000, 0, 'src-a', 'Voice A', '1080p', '2026-11-01T10:00:00.000000000Z',
   'Film One', '12+', 2020, 'p1.jpg', 0, 0, 0, 1),
  (1, 'series', 200, 1, 2, 120000, 2400000, 0, 'src-b', 'Voice B', '720p', '2026-11-02T10:00:00.000000000Z',
   'Show Two', '16+', 2021, 'p2.jpg', 1, 3, 0, 0),
  (2, 'movie', 100, 0, 0, 30000, 6000000, 0, 'src-a', '', '', '2026-11-03T10:00:00.000000000Z',
   'Film One', '12+', 2020, 'p1.jpg', 0, 0, 0, 0),
  (3, 'series', 300, 2, 5, 2400000, 2400000, 1, 'src-c', 'Voice C', '480p', '2026-11-04T10:00:00.000000000Z',
   'Show Three', '18+', 2019, 'p3.jpg', 0, 0, 1, 1),
  (9, 'movie', 400, 0, 0, 1000, 5000000, 0, 'src-a', '', '', '2026-11-05T10:00:00.000000000Z',
   'Orphan Film', '', 2018, '', 0, 0, 0, 0);
