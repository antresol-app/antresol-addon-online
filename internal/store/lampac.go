package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

// The Lampac settings of «Онлайн» (rows of the settings table): the
// address and the account of «Настройки», the sources' order and which are
// off, and the waits. All are JSON values under one key each.
const (
	keyLampac        = "lampac"
	keyLampacSources = "lampac_sources"
	keyLampacUID     = "lampac_uid"
	keyKnownSources  = "known_sources"
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

// KnownSources is what the last source lists said of each balancer: listed
// shown and non-rch (true) or not (false). At is when it was saved. Kept
// across restarts so that a resume does not wait for a title's whole list
// to learn which sources exist (online.Service.Source).
type KnownSources struct {
	Shown map[string]bool `json:"shown"`
	At    time.Time       `json:"at"`
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
	v, err := NewLampacUID()
	if err != nil {
		return "", err
	}
	return v, set(ctx, s.DB, keyLampacUID, v)
}

// uidAlphabet and uidRandom make a new id: "antresol" and 8 random [a-z0-9]
// (16 characters). Lampac's Telegram sign-in bot (/start <uid>) takes only
// 6–20 characters of [a-zA-Z0-9_-], so a new id fits it; an id saved
// before (the older "antresol-<32 hex>") is kept as it is.
const (
	uidAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	uidRandom   = 8
)

// NewLampacUID is a new id for Lampac's accounts (see uidAlphabet).
func NewLampacUID() (string, error) {
	out := make([]byte, 0, uidRandom)
	b := make([]byte, 1)
	for len(out) < uidRandom {
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		if int(b[0]) >= 256-256%len(uidAlphabet) {
			continue // no modulo bias
		}
		out = append(out, uidAlphabet[int(b[0])%len(uidAlphabet)])
	}
	return "antresol" + string(out), nil
}

// KnownSources reads the saved balancers (none: the zero value).
func (s Settings) KnownSources(ctx context.Context) (KnownSources, error) {
	var k KnownSources
	err := getJSON(ctx, s.DB, keyKnownSources, &k)
	return k, err
}

// SetKnownSources saves them, stamped now.
func (s Settings) SetKnownSources(ctx context.Context, shown map[string]bool) error {
	return setJSON(ctx, s.DB, keyKnownSources, KnownSources{Shown: shown, At: time.Now().UTC()})
}
