-- The addon's own state (not a replay of a core migration):
--   settings      key/value rows: enabled («Смотреть онлайн», 1 or 0; no
--                 row: on), ref_key and proxy_key (hex, made on first use);
--   import_state  the one-time import from vault.db: one row once it has
--                 run (counts_json: its ImportCounts). No row: the next
--                 start with an import source configured runs it.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE import_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    source      TEXT NOT NULL,
    stamp       TEXT NOT NULL,
    counts_json TEXT NOT NULL
);
