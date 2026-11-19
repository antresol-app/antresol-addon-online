package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// The Lampac settings of «Онлайн» (rows of the settings table): the
// address and the account of «Настройки», the sources' order and which are
// off, and the waits. All are JSON values under one key each.
const (
	keyLampac        = "lampac"
	keyLampacSources = "lampac_sources"
	keyLampacUID     = "lampac_uid"
)

// Lampac is the Lampac connection the admin saved. An empty URL means
// none: the server's ANTRESOL_ONLINE_LAMPAC_URL, else the helper service.
// AuthKey and AuthValue are the account query pair Lampac's accsdb asks
// for (uid, account_email, token or box_mac); added to whichever address
// is in force. AuthKey "shared_passwd" is accsdb's shared password: the
// addon registers its own id with it (LampacUID) and sends that. Timeout (seconds, 0: the default) bounds one Lampac call,
// SourcesWait (seconds, 0: the default) a title's source list.
type Lampac struct {
	URL         string `json:"url,omitempty"`
	AuthKey     string `json:"authKey,omitempty"`
	AuthValue   string `json:"authValue,omitempty"`
	Timeout     int    `json:"timeout,omitempty"`
	SourcesWait int    `json:"sourcesWait,omitempty"`
}

// LampacSources is the admin's choice of Lampac's sources (balancers):
// Order lists the ids to show first, in that order (the rest follow in
// Lampac's own order); Off the ids never offered.
type LampacSources struct {
	Order []string `json:"order,omitempty"`
	Off   []string `json:"off,omitempty"`
}

func getJSON(ctx context.Context, q querier, key string, v any) error {
	s, ok, err := get(ctx, q, key)
	if err != nil || !ok {
		return err
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("store: settings %s: %w", key, err)
	}
	return nil
}

func setJSON(ctx context.Context, q querier, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return set(ctx, q, key, string(b))
}

// Lampac reads the saved connection (none: the zero value).
func (s Settings) Lampac(ctx context.Context) (Lampac, error) {
	var l Lampac
	err := getJSON(ctx, s.DB, keyLampac, &l)
	return l, err
}

// SetLampac saves it.
func (s Settings) SetLampac(ctx context.Context, l Lampac) error {
	return setJSON(ctx, s.DB, keyLampac, l)
}

// LampacSources reads the sources' choice (none: the zero value).
func (s Settings) LampacSources(ctx context.Context) (LampacSources, error) {
	var l LampacSources
	err := getJSON(ctx, s.DB, keyLampacSources, &l)
	return l, err
}

// SetLampacSources saves it.
func (s Settings) SetLampacSources(ctx context.Context, l LampacSources) error {
	return setJSON(ctx, s.DB, keyLampacSources, l)
}

// LampacUID is the addon's own Lampac id (for accsdb's shared password),
// made (16 random bytes, hex) on first use; renew makes a new one (an id
// whose shared-password time ran out cannot be registered again).
func (s Settings) LampacUID(ctx context.Context, renew bool) (string, error) {
	if !renew {
		v, ok, err := get(ctx, s.DB, keyLampacUID)
		if err != nil || ok {
			return v, err
		}
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	v := "antresol-" + hex.EncodeToString(b)
	return v, set(ctx, s.DB, keyLampacUID, v)
}
