-- «Смотреть онлайн» progress keeps a snapshot of its TMDB title (title,
-- rating, year, and the poster's image cache file), taken when it is
-- saved, so «Продолжить» never waits for TMDB. next_season/next_episode
-- are the episode after this one as the source listed it (0: unknown,
-- the next number is offered); last_listed: the source listed nothing
-- after it (a finished series leaves «Продолжить»).
ALTER TABLE online_progress ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE online_progress ADD COLUMN rating TEXT NOT NULL DEFAULT '';
ALTER TABLE online_progress ADD COLUMN year INTEGER NOT NULL DEFAULT 0;
ALTER TABLE online_progress ADD COLUMN poster TEXT NOT NULL DEFAULT '';
ALTER TABLE online_progress ADD COLUMN next_season INTEGER NOT NULL DEFAULT 0;
ALTER TABLE online_progress ADD COLUMN next_episode INTEGER NOT NULL DEFAULT 0;
ALTER TABLE online_progress ADD COLUMN last_listed INTEGER NOT NULL DEFAULT 0;
