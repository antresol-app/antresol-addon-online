package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol-addon-online/internal/store"
	"github.com/antresol-app/antresol/addonkit"
)

// HelperSettings: Lampac is on the address saved in «Настройки».
const HelperSettings = addonkit.HelperSettings

// AuthKeys are the ways into Lampac's accounts (accsdb) «Настройки»
// offers: the query keys Lampac reads a user from (token, account_email,
// uid, box_mac; Lampac's RequestInfo.getuid), and its shared password
// (AuthShared).
var AuthKeys = []string{"uid", "account_email", "token", "box_mac", AuthShared}

// queryKeys are the query keys of AuthKeys (the account keys an address
// may carry).
var queryKeys = []string{"uid", "account_email", "token", "box_mac"}

// AuthShared is accsdb's shared password (accsdb.shared_passwd): the
// addon registers its own id with it (GET /testaccsdb?account_email=
// <password>&uid=<id>, as Lampa does) and sends uid=<id>.
const AuthShared = "shared_passwd"

// The bounds of the waits «Настройки» takes, in seconds.
const (
	MinWait = 5
	MaxWait = 120
)

// lampacConn is the Lampac connection in force: its URL (with the
// account's query), where the address comes from, the waits, and its
// health (a new one for every connection).
type lampacConn struct {
	url         string
	source      string // addonkit.HelperOverride | HelperSettings | addonkit.HelperCompose | addonkit.HelperNone
	timeout     time.Duration
	sourcesWait time.Duration
	health      *addonkit.HelperHealth
}

// connect picks the address: the server's variable (it wins), else the
// one saved in «Настройки», else the declared helper's compose address;
// the saved account's pair replaces any account key the address has
// (uid=<the addon's id> for the shared password).
func connect(saved store.Lampac, uid, env string, h *addonkit.HelperWatch) *lampacConn {
	c := &lampacConn{source: addonkit.HelperNone, health: &addonkit.HelperHealth{},
		timeout: time.Duration(saved.Timeout) * time.Second, sourcesWait: time.Duration(saved.SourcesWait) * time.Second}
	switch {
	case strings.TrimSpace(env) != "":
		c.url, c.source = strings.TrimSpace(env), addonkit.HelperOverride
	case strings.TrimSpace(saved.URL) != "":
		c.url, c.source = strings.TrimSpace(saved.URL), HelperSettings
	case h != nil:
		c.url, c.source = h.Helper.Address()+"/", addonkit.HelperCompose
	}
	key, value := saved.AuthKey, saved.AuthValue
	if key == AuthShared {
		key, value = "uid", uid
	}
	if c.url != "" && key != "" && value != "" {
		c.url = withAccount(c.url, key, value)
	}
	return c
}

// withAccount is raw with key=value in place of its account keys.
func withAccount(raw, key, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for _, k := range queryKeys {
		q.Del(k)
	}
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// origin is raw's scheme://host ("" when it does not parse).
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// build makes the service on conn and puts it in force.
func (b *Booted) build(conn *lampacConn) *online.Service {
	svc := newOnline(b.cfg, conn, b.refKey, b.proxyKey, b.serving, b.sourcePrefs)
	b.conn.Store(conn)
	b.svc.Store(svc)
	return svc
}

// Service is «Смотреть онлайн» in force (nil: no Lampac address).
func (b *Booted) Service() *online.Service {
	if b == nil {
		return nil
	}
	return b.svc.Load()
}

func (b *Booted) sourcePrefs() online.SourcePrefs {
	if p := b.prefs.Load(); p != nil {
		return *p
	}
	return online.SourcePrefs{}
}

// LampacView is «Настройки»'s Lampac block: never the account's value
// nor the address' query (it may hold the account).
type LampacView struct {
	// URL is the saved address without its query ("": none saved).
	URL string `json:"url"`
	// ComposeURL is the helper service's compose address (the field's
	// value while none is saved; "": none declared).
	ComposeURL string `json:"composeUrl,omitempty"`
	// Source is where the address in force comes from.
	Source string `json:"source"`
	// Server: the server's variable is set — it wins over the address
	// saved here (the account saved here still applies); never its value.
	Server  bool   `json:"server"`
	AuthKey string `json:"authKey"`
	// AuthSet: an account value is saved.
	AuthSet     bool     `json:"authSet"`
	AuthKeys    []string `json:"authKeys"`
	Timeout     int      `json:"timeout"`     // seconds; 0: the default
	SourcesWait int      `json:"sourcesWait"` // seconds; 0: the default
	// Defaults of the waits, in seconds.
	DefaultTimeout     int `json:"defaultTimeout"`
	DefaultSourcesWait int `json:"defaultSourcesWait"`
	MinWait            int `json:"minWait"`
	MaxWait            int `json:"maxWait"`
	// UID is the addon's own id for Lampac's accounts (made on first
	// read): sent with the shared password, and the one to give Lampac's
	// Telegram sign-in bot (/start <uid>). Not a secret: Lampac shows it.
	UID string `json:"uid"`
}

// LampacView reads the saved settings for «Настройки».
func (b *Booted) LampacView(ctx context.Context) (LampacView, error) {
	saved, err := b.Settings.Lampac(ctx)
	if err != nil {
		return LampacView{}, err
	}
	v := LampacView{URL: withoutQuery(saved.URL), Server: b.cfg.LampacURL != "", AuthKey: saved.AuthKey,
		AuthSet: saved.AuthValue != "", AuthKeys: AuthKeys, Timeout: saved.Timeout, SourcesWait: saved.SourcesWait,
		DefaultTimeout: int(online.DefaultCallTimeout / time.Second), DefaultSourcesWait: int(online.DefaultSourcesBudget / time.Second),
		MinWait: MinWait, MaxWait: MaxWait, Source: addonkit.HelperNone}
	if v.UID, err = b.Settings.LampacUID(ctx, false); err != nil {
		return LampacView{}, err
	}
	if b.Helper != nil {
		v.ComposeURL = b.Helper.Helper.Address()
	}
	if c := b.conn.Load(); c != nil {
		v.Source = c.source
	}
	return v, nil
}

// withoutQuery is raw without its query, fragment and user info.
func withoutQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return ""
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

// LampacSave is a change of «Настройки»'s Lampac block. AuthValue nil
// keeps the saved value; "" clears it.
type LampacSave struct {
	URL         string  `json:"url"`
	AuthKey     string  `json:"authKey"`
	AuthValue   *string `json:"authValue"`
	Timeout     int     `json:"timeout"`
	SourcesWait int     `json:"sourcesWait"`
}

// SettingsError is a value «Настройки» refuses; its text is the admin's
// (Russian).
type SettingsError struct{ Msg string }

func (e *SettingsError) Error() string { return e.Msg }

// next checks in against the saved settings and gives what saving it
// would keep. An address typed with the account in its query keeps it
// (the view never shows a query).
func (b *Booted) next(ctx context.Context, in LampacSave) (store.Lampac, error) {
	saved, err := b.Settings.Lampac(ctx)
	if err != nil {
		return store.Lampac{}, err
	}
	next := store.Lampac{AuthKey: strings.TrimSpace(in.AuthKey), AuthValue: saved.AuthValue, Timeout: in.Timeout, SourcesWait: in.SourcesWait}
	if raw := strings.TrimSpace(in.URL); raw != "" {
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Fragment != "" {
			return next, &SettingsError{Msg: "Укажите адрес Lampac вида http://lampac:9118"}
		}
		if u.Path == "" {
			u.Path = "/"
		}
		if saved.URL != "" && withoutQuery(saved.URL) == u.String() && u.RawQuery == "" {
			if su, err := url.Parse(saved.URL); err == nil {
				u.RawQuery = su.RawQuery
			}
		}
		next.URL = u.String()
	}
	if in.AuthValue != nil {
		next.AuthValue = strings.TrimSpace(*in.AuthValue)
	}
	if next.AuthKey != saved.AuthKey && in.AuthValue == nil {
		next.AuthValue = "" // another way in: the old value is not its
	}
	if next.AuthKey == "" {
		next.AuthValue = ""
	} else if !slices.Contains(AuthKeys, next.AuthKey) {
		return next, &SettingsError{Msg: "Неизвестный способ входа в Lampac"}
	}
	if next.AuthKey != "" && next.AuthValue == "" {
		return next, &SettingsError{Msg: "Укажите значение для входа в Lampac"}
	}
	for _, w := range []int{next.Timeout, next.SourcesWait} {
		if w != 0 && (w < MinWait || w > MaxWait) {
			return next, &SettingsError{Msg: "Время ожидания: от 5 до 120 секунд"}
		}
	}
	return next, nil
}

// SaveLampac checks and saves the Lampac block and rebuilds the service
// on it (OnChange). With the shared password the addon's id is registered
// first; a Lampac that refuses the password refuses the save.
func (b *Booted) SaveLampac(ctx context.Context, in LampacSave) error {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	next, err := b.next(ctx, in)
	if err != nil {
		return err
	}
	uid := ""
	if next.AuthKey == AuthShared {
		if uid, err = b.register(ctx, next, false); err != nil {
			return err
		}
	}
	if err := b.Settings.SetLampac(ctx, next); err != nil {
		return err
	}
	b.apply(ctx, next, uid)
	return nil
}

// apply rebuilds the service on the saved settings and hands it to
// OnChange.
func (b *Booted) apply(ctx context.Context, saved store.Lampac, uid string) {
	conn := connect(saved, uid, b.cfg.LampacURL, b.Helper)
	if conn.source == addonkit.HelperCompose {
		b.Helper.Check(ctx)
	}
	svc := b.build(conn)
	b.cfg.log().Info("online: Lampac settings changed", "from", conn.source)
	if b.onChange != nil {
		b.onChange(svc)
	}
}

// errSharedRefused: Lampac did not take the shared password.
var errSharedRefused = &SettingsError{Msg: "Lampac не принял общий пароль"}

// register registers the addon's id with the shared password of saved on
// the address saved would use (GET /testaccsdb, as Lampa does), a new id
// when renew; the id. A Lampac that does not answer is not an error here
// (the id is registered at the next start or check).
func (b *Booted) register(ctx context.Context, saved store.Lampac, renew bool) (string, error) {
	uid, err := b.Settings.LampacUID(ctx, renew)
	if err != nil {
		return "", err
	}
	base := origin(connect(saved, uid, b.cfg.LampacURL, b.Helper).url)
	if base == "" {
		return uid, nil
	}
	ok, err := registerShared(ctx, base, saved.AuthValue, uid)
	switch {
	case err != nil:
		b.cfg.log().Warn("online: Lampac shared password: not registered yet", "err", err)
	case !ok:
		return "", errSharedRefused
	}
	return uid, nil
}

// registerShared asks base/testaccsdb?account_email=<password>&uid=<uid>:
// ok when Lampac took the id (or has it already); an error when it does
// not answer. The error never names the URL.
func registerShared(ctx context.Context, base, password, uid string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, addonkit.HelperCheckTimeout)
	defer cancel()
	q := url.Values{"account_email": {password}, "uid": {uid}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/testaccsdb?"+q.Encode(), nil)
	if err != nil {
		return false, errors.New("bad address")
	}
	req.Header.Set("User-Agent", online.UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, errors.New("Lampac does not answer")
	}
	defer resp.Body.Close()
	var out struct {
		Accsdb  bool `json:"accsdb"`
		Success bool `json:"success"`
		NewUID  bool `json:"newuid"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out) != nil {
		return false, fmt.Errorf("Lampac answered %d", resp.StatusCode)
	}
	return !out.Accsdb && !out.NewUID, nil
}

// CheckResult is what «Проверить» found: "ok" (Sources: how many Lampac
// lists for the probe title), "auth" (the account was not accepted),
// "down" (Lampac does not answer) or "error" (it answered, but not as
// Lampac).
type CheckResult struct {
	Result  string `json:"result"`
	Sources int    `json:"sources"`
}

// probeCard is the title «Проверить» asks the sources of.
var probeCard = online.Q{TMDBID: 438631, Title: "Дюна", OriginalTitle: "Dune", OriginalLanguage: "en", Year: 2021}

// CheckLampac asks Lampac with the values of in (not saved; the server's
// variable still wins for the address) a real request: /lite/events with
// the account. With the shared password the addon's id is registered
// first, and a new one once if the old one is refused (its time ran out).
func (b *Booted) CheckLampac(ctx context.Context, in LampacSave) (CheckResult, error) {
	next, err := b.next(ctx, in)
	if err != nil {
		return CheckResult{}, err
	}
	try := func(uid string) CheckResult {
		conn := connect(next, uid, b.cfg.LampacURL, b.Helper)
		if conn.url == "" {
			return CheckResult{Result: "down"}
		}
		l, err := online.NewLampac(conn.url)
		if err != nil {
			return CheckResult{Result: "error"}
		}
		l.Timeout = max(conn.timeout, addonkit.HelperCheckTimeout)
		evs, err := l.Events(ctx, probeCard)
		switch {
		case err == nil, errors.Is(err, online.ErrNothing):
			n := 0
			for _, e := range evs {
				if !e.Rch {
					n++
				}
			}
			return CheckResult{Result: "ok", Sources: n}
		case errors.Is(err, online.ErrAuth):
			return CheckResult{Result: "auth"}
		case errors.Is(err, online.ErrUnavailable):
			return CheckResult{Result: "down"}
		}
		return CheckResult{Result: "error"}
	}
	if next.AuthKey != AuthShared {
		return try(""), nil
	}
	uid, err := b.register(ctx, next, false)
	if errors.Is(err, errSharedRefused) {
		return CheckResult{Result: "auth"}, nil
	} else if err != nil {
		return CheckResult{}, err
	}
	r := try(uid)
	if r.Result == "auth" {
		if uid, err = b.register(ctx, next, true); err == nil {
			r = try(uid)
		}
	}
	return r, nil
}

var sourceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SourcesView is «Настройки»'s list of Lampac's sources: each with
// whether it is on, in the order they are offered.
type SourcesView struct {
	Sources []SourceRow `json:"sources"`
}

// SourceRow is one source of SourcesView.
type SourceRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Found: Lampac found the probe title there; Listed false: Lampac
	// does not list it now (a source the admin chose before).
	Found  bool `json:"found"`
	Listed bool `json:"listed"`
}

// ErrNoLampac: no Lampac to list the sources of.
var ErrNoLampac = errors.New("online: no Lampac")

// Sources lists Lampac's sources with the admin's choice, in the order
// they are offered: the chosen order first, then Lampac's.
func (b *Booted) Sources(ctx context.Context) (SourcesView, error) {
	svc := b.Service()
	if svc == nil {
		return SourcesView{}, ErrNoLampac
	}
	all, err := svc.LampacSources(ctx)
	if err != nil {
		return SourcesView{}, err
	}
	p := b.sourcePrefs()
	rows := []SourceRow{}
	seen := map[string]bool{}
	byID := map[string]online.LampacSource{}
	for _, s := range all {
		byID[s.ID] = s
	}
	add := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		s, listed := byID[id]
		name := s.Name
		if name == "" {
			name = id
		}
		rows = append(rows, SourceRow{ID: id, Name: name, Enabled: !slices.Contains(p.Off, id), Found: s.Found, Listed: listed})
	}
	for _, id := range p.Order {
		add(id)
	}
	for _, s := range all {
		add(s.ID)
	}
	for _, id := range p.Off {
		add(id)
	}
	return SourcesView{Sources: rows}, nil
}

// SourcesSave is the admin's choice: the ids in the order to offer them,
// and the ones off.
type SourcesSave struct {
	Order []string `json:"order"`
	Off   []string `json:"off"`
}

// SaveSources saves the choice and applies it at once.
func (b *Booted) SaveSources(ctx context.Context, in SourcesSave) error {
	clean := func(ids []string) ([]string, error) {
		if len(ids) > 500 {
			return nil, &SettingsError{Msg: "Слишком много источников"}
		}
		out := []string{}
		for _, id := range ids {
			if !sourceIDRe.MatchString(id) {
				return nil, &SettingsError{Msg: "Неверный источник"}
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return out, nil
	}
	order, err := clean(in.Order)
	if err != nil {
		return err
	}
	off, err := clean(in.Off)
	if err != nil {
		return err
	}
	if err := b.Settings.SetLampacSources(ctx, store.LampacSources{Order: order, Off: off}); err != nil {
		return err
	}
	b.prefs.Store(&online.SourcePrefs{Order: order, Off: off})
	return nil
}

// HelpersStatus answers helpers.status: Lampac as «Смотреть онлайн» uses
// it, from its own last call when fresh, else a probe of the address in
// force (no query: it may hold the account); with no address, the compose
// name's last probe.
func (b *Booted) HelpersStatus(ctx context.Context) addonkit.HelpersStatus {
	name := "lampac"
	if b.Helper != nil {
		name = b.Helper.Helper.Name
	}
	c := b.conn.Load()
	if c == nil {
		c = &lampacConn{source: addonkit.HelperNone}
	}
	st := addonkit.HelperState{Name: name, Source: c.source}
	if b.Helper != nil && (c.source == addonkit.HelperCompose || c.source == addonkit.HelperNone) {
		st.Address = b.Helper.Helper.Address()
	}
	if c.source == addonkit.HelperNone || c.url == "" {
		if b.Helper != nil {
			return addonkit.HelpersStatus{Helpers: []addonkit.HelperState{b.watchHealth.State(ctx, st, nil)}}
		}
		st.Problem = addonkit.HelperProblemUnset
		return addonkit.HelpersStatus{Helpers: []addonkit.HelperState{st}}
	}
	base := c.url
	if u, err := url.Parse(c.url); err == nil {
		base = u.Scheme + "://" + u.Host
	}
	probe := func(ctx context.Context) error { return addonkit.ProbeAddress(ctx, nil, base, "/", true) }
	return addonkit.HelpersStatus{Helpers: []addonkit.HelperState{c.health.State(ctx, st, probe)}}
}

// renewShared registers the addon's id with the shared password at start
// (a Lampac that was down at the save), and a new id when Lampac refuses
// the old one (its time ran out), rebuilding the service on it.
func (b *Booted) renewShared(ctx context.Context, saved store.Lampac) {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	if _, err := b.register(ctx, saved, false); err != nil {
		b.cfg.log().Warn("online: Lampac shared password", "err", err)
		return
	}
	r, err := b.CheckLampac(ctx, LampacSave{URL: saved.URL, AuthKey: saved.AuthKey, AuthValue: &saved.AuthValue,
		Timeout: saved.Timeout, SourcesWait: saved.SourcesWait})
	if err != nil || r.Result != "ok" {
		return
	}
	uid, err := b.Settings.LampacUID(ctx, false)
	if err != nil || b.closed.Load() {
		return
	}
	if c := b.conn.Load(); c != nil && strings.Contains(c.url, url.QueryEscape(uid)) {
		return // in force already
	}
	b.apply(ctx, saved, uid)
}

// SetOnChange sets what is called with the new service after a change of
// the Lampac settings rebuilt it, and calls it with the one in force.
func (b *Booted) SetOnChange(f func(*online.Service)) {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	b.onChange = f
	f(b.Service())
}
