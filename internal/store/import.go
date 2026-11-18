package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/sqlitesnap"
)

// ImportCounts are what the one-time import did: Rows copied, the
// profiles (Users) they belong to, the Orphans skipped (rows of a user_id
// the snapshot's users do not list, or list without a uid), and the server's last
// «Смотреть онлайн» setting it took (Enabled).
type ImportCounts struct {
	Rows    int  `json:"rows"`
	Users   int  `json:"users"`
	Orphans int  `json:"orphans"`
	Enabled bool `json:"enabled"`
}

// The snapshot's tables the import reads (the core's, in vault.db).
const (
	srcProgress = "online_progress"
	// srcUsers maps a row's user_id to the profile's uid (core migration
	// 0093). Read from the snapshot only, never from a live core database.
	srcUsers = `SELECT id, uid FROM users`
	// srcAppSettings is the core's settings row whose JSON holds the
	// onlineEnabled the server saved last.
	srcAppSettings = `SELECT value FROM settings WHERE key = 'app_settings'`
)

// importDir is the import's work folder in the data dir (its snapshot).
const importDir = "import"

// RemoveImportLeftovers deletes dataDir/import: the snapshot of a run
// that was killed before it could delete it. Every start calls it (Import
// does, and boot without an import source), so a copy of vault.db never
// stays in the addon's data dir.
func RemoveImportLeftovers(dataDir string) error {
	if err := os.RemoveAll(filepath.Join(dataDir, importDir)); err != nil {
		return fmt.Errorf("store: import leftovers: %w", err)
	}
	return nil
}

// ImportState is the recorded import (done false when none has run).
func ImportState(ctx context.Context, d *DB) (ImportCounts, bool, error) {
	var raw string
	err := d.QueryRowContext(ctx, `SELECT counts_json FROM import_state WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportCounts{}, false, nil
	}
	if err != nil {
		return ImportCounts{}, false, err
	}
	var c ImportCounts
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return ImportCounts{}, false, fmt.Errorf("store: import_state counts: %w", err)
	}
	return c, true, nil
}

// Import copies the «Смотреть онлайн» progress and setting of the server's
// vault.db at from into d, once: with an import recorded it returns the
// recorded counts and reads nothing. It reads a sqlitesnap snapshot (the
// database and its -wal, copied under d.Dir/import and deleted after),
// never from itself. online_progress rows are copied by column name over
// the columns both sides have, each row's user_id becoming the snapshot's
// users.uid; a row the addon already has (same key) is kept. A source
// without online_progress is 0 rows. The rows, the setting and
// import_state are written in one transaction: a failure records nothing,
// and the next start runs the import again.
func Import(ctx context.Context, d *DB, from string, now func() time.Time) (ImportCounts, error) {
	// a run killed after its commit leaves its snapshot (a whole vault.db,
	// password hashes included): it goes first, whether or not the import
	// has run
	if err := RemoveImportLeftovers(d.Dir); err != nil {
		return ImportCounts{}, err
	}
	if c, done, err := ImportState(ctx, d); err != nil || done {
		return c, err
	}
	if from == "" {
		return ImportCounts{}, errors.New("store: import: no source")
	}
	if now == nil {
		now = time.Now
	}
	work := filepath.Join(d.Dir, importDir)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return ImportCounts{}, err
	}
	defer os.RemoveAll(work)
	snap, err := sqlitesnap.Snapshot(from, work)
	if err != nil {
		return ImportCounts{}, fmt.Errorf("store: import: snapshot of %s: %w", from, err)
	}
	src, err := sqlitesnap.OpenRO(snap)
	if err != nil {
		return ImportCounts{}, fmt.Errorf("store: import: open snapshot: %w", err)
	}
	defer src.Close()

	var c ImportCounts
	if c.Enabled, err = sourceEnabled(ctx, src); err != nil {
		return ImportCounts{}, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return ImportCounts{}, err
	}
	defer tx.Rollback()
	if c.Rows, c.Users, c.Orphans, err = copyProgress(ctx, src, tx); err != nil {
		return ImportCounts{}, err
	}
	if err := set(ctx, tx, keyEnabled, flagValue(c.Enabled)); err != nil {
		return ImportCounts{}, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return ImportCounts{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO import_state (id, source, stamp, counts_json) VALUES (1, ?, ?, ?)`,
		from, now().UTC().Format(time.RFC3339), string(raw)); err != nil {
		return ImportCounts{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImportCounts{}, err
	}
	return c, nil
}

// sourceEnabled is app_settings.onlineEnabled of the snapshot: true
// without a settings table, row or key (the core's default); a value the
// core never writes is an error, never a default.
func sourceEnabled(ctx context.Context, src *sql.DB) (bool, error) {
	ok, err := sqlitesnap.HasTable(ctx, src, "settings")
	if err != nil || !ok {
		return true, err
	}
	var raw string
	err = src.QueryRowContext(ctx, srcAppSettings).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var m map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&m); err != nil || dec.More() {
		return false, fmt.Errorf("store: import: settings app_settings is not a JSON object")
	}
	v, ok := m["onlineEnabled"]
	if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return true, nil
	}
	var on bool
	if err := json.Unmarshal(v, &on); err != nil {
		return false, fmt.Errorf("store: import: settings app_settings onlineEnabled = %s, not true or false", v)
	}
	return on, nil
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// columns lists a table's columns in table order, lower-cased.
func columns(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(n))
	}
	return out, rows.Err()
}

// uids maps the snapshot's users.id to users.uid (profiles without one are
// left out). A users table without uid is a core before migration 0093:
// its rows cannot be keyed, so the import fails.
func uids(ctx context.Context, src *sql.DB) (map[int64]string, error) {
	cols, err := sqlitesnap.Columns(ctx, src, "users")
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, errors.New("store: import: the source has online_progress but no users table")
	}
	if !cols["uid"] {
		return nil, errors.New("store: import: the source's users have no uid column (a server before migration 0093)")
	}
	rows, err := src.QueryContext(ctx, srcUsers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var uid sql.NullString
		if err := rows.Scan(&id, &uid); err != nil {
			return nil, err
		}
		if uid.Valid && uid.String != "" {
			out[id] = uid.String
		}
	}
	return out, rows.Err()
}

// copyProgress copies the snapshot's online_progress into tx (see Import)
// and returns the rows copied, their distinct profiles and the orphans.
func copyProgress(ctx context.Context, src *sql.DB, tx *sql.Tx) (rows, users, orphans int, err error) {
	ok, err := sqlitesnap.HasTable(ctx, src, srcProgress)
	if err != nil || !ok {
		return 0, 0, 0, err
	}
	byID, err := uids(ctx, src)
	if err != nil {
		return 0, 0, 0, err
	}
	srcCols, err := columns(ctx, src, srcProgress)
	if err != nil {
		return 0, 0, 0, err
	}
	dstCols, err := columns(ctx, tx, "online_progress")
	if err != nil {
		return 0, 0, 0, err
	}
	have := map[string]bool{}
	for _, c := range dstCols {
		have[c] = true
	}
	hasUserID := false
	var cols []string
	for _, c := range srcCols {
		switch {
		case c == "user_id":
			hasUserID = true
		case c == "user_uid": // the key is the snapshot's users.uid, never a column of the source
		case have[c]:
			cols = append(cols, quote(c))
		}
	}
	if !hasUserID {
		return 0, 0, 0, errors.New("store: import: the source's online_progress has no user_id column")
	}
	if len(cols) == 0 {
		return 0, 0, 0, errors.New("store: import: the source's online_progress has no column in common")
	}
	list := strings.Join(cols, ", ")
	ins, err := tx.PrepareContext(ctx, `INSERT INTO online_progress (user_uid, `+list+`) VALUES (?`+
		strings.Repeat(", ?", len(cols))+`) ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, 0, 0, err
	}
	defer ins.Close()
	q, err := src.QueryContext(ctx, `SELECT user_id, `+list+` FROM `+quote(srcProgress))
	if err != nil {
		return 0, 0, 0, err
	}
	defer q.Close()
	vals := make([]any, len(cols)+1)
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	seen := map[string]bool{}
	total := 0
	for q.Next() {
		if err := q.Scan(ptrs...); err != nil {
			return 0, 0, 0, err
		}
		total++
		id, ok := vals[0].(int64)
		uid := byID[id]
		if !ok || uid == "" {
			orphans++
			continue
		}
		args := append([]any{uid}, vals[1:]...)
		res, err := ins.ExecContext(ctx, args...)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("store: import: online_progress row %d: %w", total, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rows++
			seen[uid] = true
		}
	}
	if err := q.Err(); err != nil {
		return 0, 0, 0, err
	}
	return rows, len(seen), orphans, nil
}
