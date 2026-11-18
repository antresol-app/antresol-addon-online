// Package store is the addon's own database, data/online.db (WAL). Its
// migrations 0001-0003 are the core's online migrations 0027-0029 replayed
// by content: online_progress is keyed by the profile's uid, with no users
// table and no foreign key, and 0029's backfill from the library's items
// is cut (in_library comes from the import). 0004 is the addon's own state
// (settings, import_state). The one-time import from vault.db is import.go.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// File is the database's name in the data dir.
const File = "online.db"

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB is the addon's database; Dir is the data dir it lives in (the import
// takes its snapshot under it).
type DB struct {
	*sql.DB
	Dir string
}

// Open opens (creating if needed) dir/online.db with the core's pragmas
// (WAL, synchronous NORMAL, busy timeout, immediate transactions) and
// applies the pending migrations in version order.
func Open(dir string) (*DB, error) {
	dsn := "file:" + filepath.Join(dir, File) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(8)
	d.SetMaxIdleConns(8)
	if err := migrate(d, migrationsFS, "migrations"); err != nil {
		d.Close()
		return nil, fmt.Errorf("store: %s: %w", File, err)
	}
	return &DB{DB: d, Dir: dir}, nil
}

type migration struct {
	ver  int
	name string
}

// list returns dir's .sql files by version number; a version used twice is
// an error.
func list(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ver, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad version prefix", e.Name())
		}
		if prev, dup := seen[ver]; dup {
			return nil, fmt.Errorf("migration version %d used twice: %s and %s", ver, prev, e.Name())
		}
		seen[ver] = e.Name()
		out = append(out, migration{ver, e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ver < out[j].ver })
	return out, nil
}

// migrate applies the files of dir not yet in schema_migrations, each in a
// transaction of its own.
func migrate(d *sql.DB, fsys fs.FS, dir string) error {
	all, err := list(fsys, dir)
	if err != nil {
		return err
	}
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	for _, m := range all {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, m.ver).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, m.name))
		if err != nil {
			return err
		}
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, m.ver); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// DropProfilesExcept deletes the online_progress rows of every uid not in
// uids (a deleted profile: the server's GET /users no longer lists it) and
// returns how many went. An empty uids drops nothing: a server always has a
// profile, so an empty list is a bad answer, not "everyone is gone".
func DropProfilesExcept(ctx context.Context, d *sql.DB, uids []string) (int, error) {
	if len(uids) == 0 {
		return 0, nil
	}
	keep := make([]string, 0, len(uids))
	args := make([]any, 0, len(uids))
	for _, u := range uids {
		if u == "" {
			continue
		}
		keep = append(keep, "?")
		args = append(args, u)
	}
	if len(keep) == 0 {
		return 0, nil
	}
	res, err := d.ExecContext(ctx, `DELETE FROM online_progress WHERE user_uid NOT IN (`+strings.Join(keep, ", ")+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
