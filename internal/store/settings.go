package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// Settings keys (the settings table of 0004).
const (
	keyEnabled  = "enabled"
	keyRefKey   = "ref_key"
	keyProxyKey = "proxy_key"
)

// keyBytes is the length of the ref and proxy keys.
const keyBytes = 32

// Settings reads and writes the addon's settings rows. A stored value that
// does not parse is an error, never a default.
type Settings struct{ DB *sql.DB }

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func get(ctx context.Context, q querier, key string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func set(ctx context.Context, q querier, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func flagValue(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// Enabled is «Смотреть онлайн» (the admin setting); on without a row.
func (s Settings) Enabled(ctx context.Context) (bool, error) {
	v, ok, err := get(ctx, s.DB, keyEnabled)
	if err != nil || !ok {
		return true, err
	}
	switch v {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("store: settings %s = %q, not 1 or 0", keyEnabled, v)
}

// SetEnabled saves it.
func (s Settings) SetEnabled(ctx context.Context, on bool) error {
	return set(ctx, s.DB, keyEnabled, flagValue(on))
}

// Keys are the refs' signing key and the stream proxy's key, each made
// (32 random bytes) on first use and kept. They are the addon's own: the
// core's keys are not imported (refs are never stored, so a player open
// across the move re-resolves).
func (s Settings) Keys(ctx context.Context) (ref, proxy []byte, err error) {
	if ref, err = s.key(ctx, keyRefKey); err != nil {
		return nil, nil, err
	}
	if proxy, err = s.key(ctx, keyProxyKey); err != nil {
		return nil, nil, err
	}
	return ref, proxy, nil
}

func (s Settings) key(ctx context.Context, name string) ([]byte, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	v, ok, err := get(ctx, tx, name)
	if err != nil {
		return nil, err
	}
	if ok {
		b, err := hex.DecodeString(v)
		if err != nil || len(b) != keyBytes {
			return nil, fmt.Errorf("store: settings %s is not a %d-byte hex key", name, keyBytes)
		}
		return b, nil
	}
	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := set(ctx, tx, name, hex.EncodeToString(b)); err != nil {
		return nil, err
	}
	return b, tx.Commit()
}
