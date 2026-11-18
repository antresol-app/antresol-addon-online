-- in_library: this online position reached the library's progress (the
-- mirror at save time, or a seeding after a scan). A scan seeds only rows
-- that never did, so «Не просмотрено» (which deletes the library row) is
-- never undone by an old online position. Rows imported from the server's
-- database carry the flag they had there.
ALTER TABLE online_progress ADD COLUMN in_library INTEGER NOT NULL DEFAULT 0;
