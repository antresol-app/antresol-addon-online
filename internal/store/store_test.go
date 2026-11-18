package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var fixedNow = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }

func openDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newSource writes testdata/vault-fixture.sql into a fresh vault.db in WAL
// mode, runs edit on it, and closes it (so its WAL is checkpointed).
func newSource(t *testing.T, edit ...string) string {
	t.Helper()
	from := filepath.Join(t.TempDir(), "vault.db")
	body, err := os.ReadFile("testdata/vault-fixture.sql")
	if err != nil {
		t.Fatal(err)
	}
	v, err := sql.Open("sqlite", "file:"+from+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Exec(string(body)); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, e := range edit {
		if _, err := v.Exec(e); err != nil {
			t.Fatalf("fixture edit %q: %v", e, err)
		}
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	return from
}

func count(t *testing.T, d *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func mustImport(t *testing.T, d *DB, from string) ImportCounts {
	t.Helper()
	c, err := Import(context.Background(), d, from, fixedNow)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return c
}

// TestImport_CopiesRowsByColumnName: every column both sides have is
// copied by its name; a column only the source has is skipped, and one
// only the addon has keeps its default.
func TestImport_CopiesRowsByColumnName(t *testing.T) {
	from := newSource(t,
		`ALTER TABLE online_progress ADD COLUMN only_in_source TEXT NOT NULL DEFAULT 'x'`,
		`ALTER TABLE online_progress DROP COLUMN next_episode`)
	d := openDB(t)
	c := mustImport(t, d, from)
	if c.Rows != 4 {
		t.Fatalf("rows %d, want 4 (%+v)", c.Rows, c)
	}
	var (
		pos, dur, fin, year, nextS, nextE, last, inLib      int
		src, voice, quality, updated, title, rating, poster string
	)
	if err := d.QueryRow(`SELECT position_ms, duration_ms, finished, source, voice, quality, updated_at, title, rating,
		year, poster, next_season, next_episode, last_listed, in_library FROM online_progress
		WHERE user_uid='uid-admin' AND kind='series' AND tmdb_id=200 AND season=1 AND episode=2`).
		Scan(&pos, &dur, &fin, &src, &voice, &quality, &updated, &title, &rating, &year, &poster, &nextS, &nextE, &last, &inLib); err != nil {
		t.Fatal(err)
	}
	if pos != 120000 || dur != 2400000 || fin != 0 || src != "src-b" || voice != "Voice B" || quality != "720p" ||
		updated != "2026-11-02T10:00:00.000000000Z" || title != "Show Two" || rating != "16+" || year != 2021 ||
		poster != "p2.jpg" || nextS != 1 || last != 0 || inLib != 0 {
		t.Fatalf("row copied wrong: %d %d %d %q %q %q %q %q %q %d %q %d %d %d", pos, dur, fin, src, voice, quality, updated,
			title, rating, year, poster, nextS, last, inLib)
	}
	if nextE != 0 {
		t.Fatalf("next_episode %d: a column missing from the source keeps its default", nextE)
	}
	// the flag each row had travels with it
	if n := count(t, d.DB, `SELECT count(*) FROM online_progress WHERE in_library=1`); n != 2 {
		t.Fatalf("in_library rows %d, want 2", n)
	}
}

// TestImport_NoTable: a source without online_progress is 0 rows and a
// success (and the import is recorded).
func TestImport_NoTable(t *testing.T) {
	from := newSource(t, `DROP TABLE online_progress`)
	d := openDB(t)
	c := mustImport(t, d, from)
	if c.Rows != 0 || c.Users != 0 || c.Orphans != 0 {
		t.Fatalf("counts %+v, want zero", c)
	}
	if _, done, err := ImportState(context.Background(), d); err != nil || !done {
		t.Fatalf("import_state: done %v, %v", done, err)
	}
}

// TestImport_OrphanUsersSkipped: a row whose user_id the snapshot's users
// do not list is skipped and counted.
func TestImport_OrphanUsersSkipped(t *testing.T) {
	d := openDB(t)
	c := mustImport(t, d, newSource(t))
	if c.Rows != 4 || c.Users != 3 || c.Orphans != 1 {
		t.Fatalf("counts %+v, want 4 rows of 3 profiles, 1 orphan", c)
	}
	if n := count(t, d.DB, `SELECT count(*) FROM online_progress WHERE tmdb_id=400`); n != 0 {
		t.Fatalf("the orphan row was imported")
	}
	if n := count(t, d.DB, `SELECT count(*) FROM online_progress`); n != 4 {
		t.Fatalf("%d rows, want 4", n)
	}
}

// walSource is a source with a row only in its WAL: w (returned, still
// open, auto-checkpoint off) wrote it. The test closes w.
func walSource(t *testing.T) (string, *sql.DB) {
	t.Helper()
	from := newSource(t)
	w, err := sql.Open("sqlite", "file:"+from+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(`INSERT INTO online_progress (user_id, kind, tmdb_id, position_ms, duration_ms, updated_at, title)
		VALUES (2, 'movie', 500, 7000, 9000000, '2026-11-06T10:00:00.000000000Z', 'WAL only')`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(from + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("no WAL content: %v", err)
	}
	return from, w
}

// TestImport_ReadsWAL: a row still only in the source's WAL is imported.
func TestImport_ReadsWAL(t *testing.T) {
	from, w := walSource(t)
	defer w.Close()
	d := openDB(t)
	c := mustImport(t, d, from)
	if c.Rows != 5 {
		t.Fatalf("rows %d, want 5 (the WAL row included)", c.Rows)
	}
	var title string
	if err := d.QueryRow(`SELECT title FROM online_progress WHERE user_uid='uid-second' AND tmdb_id=500`).Scan(&title); err != nil || title != "WAL only" {
		t.Fatalf("WAL row: %q %v", title, err)
	}
}

func sum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// TestImport_NeverWritesSource: the source and its WAL are byte-equal
// after an import, and nothing is created beside them.
func TestImport_NeverWritesSource(t *testing.T) {
	from, w := walSource(t)
	defer w.Close()
	before := [2][32]byte{sum(t, from), sum(t, from+"-wal")}
	entries := func() []string {
		es, err := os.ReadDir(filepath.Dir(from))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}
	names := entries()
	d := openDB(t)
	mustImport(t, d, from)
	if after := [2][32]byte{sum(t, from), sum(t, from+"-wal")}; after != before {
		t.Fatal("vault.db or its WAL changed")
	}
	if got := entries(); !slices.Equal(got, names) {
		t.Fatalf("the source folder changed: %v, was %v", got, names)
	}
	// and no snapshot is left in the addon's folder
	if _, err := os.Stat(filepath.Join(d.Dir, "import")); !os.IsNotExist(err) {
		t.Fatalf("import work folder left: %v", err)
	}
}

// TestImport_EnabledFromAppSettings: «Смотреть онлайн» takes the server's
// app_settings.onlineEnabled; without it (no key, no row, no settings
// table) it is on.
func TestImport_EnabledFromAppSettings(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		edit []string
		want bool
	}{
		{"off", nil, false},
		{"on", []string{`UPDATE settings SET value='{"onlineEnabled":true}' WHERE key='app_settings'`}, true},
		{"no key", []string{`UPDATE settings SET value='{"remoteQuality":"720"}' WHERE key='app_settings'`}, true},
		{"no row", []string{`DELETE FROM settings`}, true},
		{"no table", []string{`DROP TABLE settings`}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDB(t)
			c := mustImport(t, d, newSource(t, tc.edit...))
			if c.Enabled != tc.want {
				t.Fatalf("counts.Enabled %v, want %v", c.Enabled, tc.want)
			}
			on, err := Settings{DB: d.DB}.Enabled(ctx)
			if err != nil || on != tc.want {
				t.Fatalf("setting %v %v, want %v", on, err, tc.want)
			}
		})
	}
	// an app_settings the core never writes fails the import
	d := openDB(t)
	if _, err := Import(ctx, d, newSource(t, `UPDATE settings SET value='{"onlineEnabled":"no"}' WHERE key='app_settings'`), fixedNow); err == nil {
		t.Fatal("a non-boolean onlineEnabled was accepted")
	}
	if _, done, _ := ImportState(ctx, d); done {
		t.Fatal("a failed import was recorded")
	}
}

// TestImport_RunsOnce: once recorded, an import never reads its source
// again: the counts are the recorded ones, even with the source gone.
func TestImport_RunsOnce(t *testing.T) {
	ctx := context.Background()
	from := newSource(t)
	d := openDB(t)
	first := mustImport(t, d, from)
	st, done, err := ImportState(ctx, d)
	if err != nil || !done || st != first {
		t.Fatalf("import_state %+v %v %v, want %+v", st, done, err, first)
	}
	var stamp string
	if err := d.QueryRow(`SELECT stamp FROM import_state WHERE id=1`).Scan(&stamp); err != nil || stamp != "2026-11-17T12:00:00Z" {
		t.Fatalf("stamp %q %v", stamp, err)
	}
	// a row the profile saved in the addon since, and the source changed
	if _, err := d.Exec(`DELETE FROM online_progress WHERE tmdb_id=300`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(from); err != nil {
		t.Fatal(err)
	}
	again, err := Import(ctx, d, from, fixedNow)
	if err != nil || again != first {
		t.Fatalf("second import %+v %v, want the recorded %+v", again, err, first)
	}
	if n := count(t, d.DB, `SELECT count(*) FROM online_progress`); n != 3 {
		t.Fatalf("%d rows: the second import copied again", n)
	}
}

// TestImport_FailureLeavesNothing: a failed import records nothing and
// copies nothing, so the next start runs it again.
func TestImport_FailureLeavesNothing(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	if _, err := Import(ctx, d, filepath.Join(t.TempDir(), "missing.db"), fixedNow); err == nil {
		t.Fatal("a missing source imported")
	}
	if _, done, _ := ImportState(ctx, d); done {
		t.Fatal("a failed import was recorded")
	}
	c := mustImport(t, d, newSource(t))
	if c.Rows != 4 {
		t.Fatalf("retry rows %d, want 4", c.Rows)
	}
}

// TestImport_KeysByUID: rows land under the snapshot's users.uid; a users
// table without uid (a core before migration 0093) fails the import.
func TestImport_KeysByUID(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	mustImport(t, d, newSource(t))
	rows, err := d.Query(`SELECT user_uid, count(*) FROM online_progress GROUP BY user_uid ORDER BY user_uid`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var uid string
		var n int
		if err := rows.Scan(&uid, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, uid+":"+string(rune('0'+n)))
	}
	rows.Close()
	if want := []string{"uid-admin:2", "uid-second:1", "uid-third:1"}; !slices.Equal(got, want) {
		t.Fatalf("rows by uid %v, want %v", got, want)
	}
	cols, err := d.Query(`SELECT name FROM pragma_table_info('online_progress')`)
	if err != nil {
		t.Fatal(err)
	}
	for cols.Next() {
		var n string
		cols.Scan(&n)
		if n == "user_id" {
			t.Fatal("the addon's online_progress has a user_id column")
		}
	}
	cols.Close()

	noUID := newSource(t, `DROP INDEX users_uid`, `ALTER TABLE users DROP COLUMN uid`)
	d2 := openDB(t)
	_, err = Import(ctx, d2, noUID, fixedNow)
	if err == nil || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("a users table without uid: %v, want an error naming uid", err)
	}
	if _, done, _ := ImportState(ctx, d2); done {
		t.Fatal("the failed import was recorded")
	}
	if n := count(t, d2.DB, `SELECT count(*) FROM online_progress`); n != 0 {
		t.Fatalf("%d rows copied by a failed import", n)
	}
	// a profile whose uid is empty is not a profile the addon can key by:
	// its rows are orphans
	d3 := openDB(t)
	c := mustImport(t, d3, newSource(t, `DROP INDEX users_uid`, `UPDATE users SET uid=NULL WHERE id=3`))
	if c.Rows != 3 || c.Orphans != 2 || c.Users != 2 {
		t.Fatalf("counts %+v, want 3 rows of 2 profiles, 2 orphans", c)
	}
}

// TestImport_KeepsAddonRows: a row the addon already has (same key) is
// kept as it is, never overwritten by the older one of the source.
func TestImport_KeepsAddonRows(t *testing.T) {
	d := openDB(t)
	if _, err := d.Exec(`INSERT INTO online_progress (user_uid, kind, tmdb_id, position_ms, updated_at, title)
		VALUES ('uid-second', 'movie', 100, 99000, '2026-11-10T10:00:00.000000000Z', 'Newer')`); err != nil {
		t.Fatal(err)
	}
	c := mustImport(t, d, newSource(t))
	if c.Rows != 3 {
		t.Fatalf("rows %d, want 3 (the addon's own row kept)", c.Rows)
	}
	var title string
	var pos int
	if err := d.QueryRow(`SELECT title, position_ms FROM online_progress WHERE user_uid='uid-second' AND tmdb_id=100`).Scan(&title, &pos); err != nil || title != "Newer" || pos != 99000 {
		t.Fatalf("the addon's row became %q %d %v", title, pos, err)
	}
}

// TestMigrations_0003WithoutCoreTables: the migrations apply on an empty
// database (no items, no users) and 0003 adds in_library.
func TestMigrations_0003WithoutCoreTables(t *testing.T) {
	d := openDB(t)
	for _, tbl := range []string{"items", "users", "progress"} {
		if n := count(t, d.DB, `SELECT count(*) FROM sqlite_master WHERE name=?`, tbl); n != 0 {
			t.Fatalf("core table %s in online.db", tbl)
		}
	}
	if n := count(t, d.DB, `SELECT count(*) FROM pragma_table_info('online_progress') WHERE name='in_library'`); n != 1 {
		t.Fatal("no in_library column")
	}
	var vs []int
	rows, err := d.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int
		rows.Scan(&v)
		vs = append(vs, v)
	}
	rows.Close()
	if want := []int{1, 2, 3, 4}; !slices.Equal(vs, want) {
		t.Fatalf("versions %v, want %v", vs, want)
	}
	// reopening applies nothing twice
	dir := d.Dir
	d.Close()
	d2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	d2.Close()
}

// TestMigrations_NoUsersTableNoFK: online_progress is keyed by
// (user_uid, kind, tmdb_id, season, episode), with no foreign key.
func TestMigrations_NoUsersTableNoFK(t *testing.T) {
	d := openDB(t)
	rows, err := d.Query(`SELECT name FROM pragma_table_info('online_progress') WHERE pk > 0 ORDER BY pk`)
	if err != nil {
		t.Fatal(err)
	}
	var pk []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		pk = append(pk, n)
	}
	rows.Close()
	if want := []string{"user_uid", "kind", "tmdb_id", "season", "episode"}; !slices.Equal(pk, want) {
		t.Fatalf("primary key %v, want %v", pk, want)
	}
	for _, tbl := range []string{"online_progress", "settings", "import_state"} {
		if n := count(t, d.DB, `SELECT count(*) FROM pragma_foreign_key_list(?)`, tbl); n != 0 {
			t.Fatalf("%s has %d foreign keys", tbl, n)
		}
	}
}

// TestDropProfilesExcept: the rows of a uid not listed go; an empty list
// drops nothing (no server has no profile: it is a bad answer).
func TestDropProfilesExcept(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	mustImport(t, d, newSource(t))
	n, err := DropProfilesExcept(ctx, d.DB, nil)
	if err != nil || n != 0 {
		t.Fatalf("empty list: dropped %d %v", n, err)
	}
	n, err = DropProfilesExcept(ctx, d.DB, []string{"uid-admin", "uid-third", "uid-new"})
	if err != nil || n != 1 {
		t.Fatalf("dropped %d %v, want 1", n, err)
	}
	if c := count(t, d.DB, `SELECT count(*) FROM online_progress WHERE user_uid='uid-second'`); c != 0 {
		t.Fatal("the deleted profile's row is still there")
	}
	if c := count(t, d.DB, `SELECT count(*) FROM online_progress`); c != 3 {
		t.Fatalf("%d rows left, want 3", c)
	}
}

// leftover is a killed run's snapshot in dir/import.
func leftover(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "import", "vault.db-0123abcd")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("a copy of vault.db"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestImport_RemovesLeftoverAfterRecordedImport: a run killed between its
// commit and the clean-up leaves its snapshot (password hashes included);
// the next Import deletes it, though the import is recorded and does not
// run again.
func TestImport_RemovesLeftoverAfterRecordedImport(t *testing.T) {
	d := openDB(t)
	first := mustImport(t, d, newSource(t))
	leftover(t, d.Dir)
	again, err := Import(context.Background(), d, filepath.Join(t.TempDir(), "gone.db"), fixedNow)
	if err != nil || again != first {
		t.Fatalf("recorded import: %+v %v", again, err)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "import")); !os.IsNotExist(err) {
		t.Fatalf("the leftover snapshot is still there: %v", err)
	}
}
