// Package boot brings the addon up, in this order: its own database
// (data/online.db), the one-time import from vault.db when one is
// configured and has not run, the clean-up of deleted profiles' rows, the
// keys, «Смотреть онлайн» itself, and then ready. Until ready, /health
// answers ready:false; a failure on the way is logged and leaves the
// addon not ready (the next start tries again).
package boot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol-addon-online/internal/store"
	"github.com/antresol-app/antresol/addonkit"
)

// defaultUsersBudget bounds the boot's GET /users: a core that does not
// answer skips the clean-up, never the start.
const defaultUsersBudget = 10 * time.Second

// Config is what Run needs.
type Config struct {
	DataDir string // the addon's data dir (ANTRESOL_ADDON_DATA_DIR)
	// ImportFrom is the server's vault.db (ANTRESOL_ONLINE_IMPORT_FROM):
	// imported on the first start that has it ("": no import).
	ImportFrom string
	// LampacURL is ANTRESOL_ONLINE_LAMPAC_URL: the admin's own address,
	// which wins. "": the declared helper's compose address (Helper),
	// else no service (available: false).
	LampacURL string
	// Helper watches the helper service the manifest declares (nil:
	// none). Run asks it once; the caller keeps it running
	// (HelperWatch.Run) when LampacURL is "".
	Helper *addonkit.HelperWatch
	FFmpeg string // the core's ffmpeg (subtitles); "": ffmpeg
	// Users lists the server's profiles (*addonkit.Client); nil: no
	// clean-up.
	Users online.UserLister
	// Library and LibraryProgress are the core's library and its watch
	// progress (*addonkit.Client): the online progress is mirrored into
	// them (nil: no mirror).
	Library         online.LibraryLookup
	LibraryProgress online.LibraryProgress
	// Titles reads a TMDB title for «Смотреть онлайн»
	// (coreadapt.Titles.Title); nil: every title is unavailable.
	Titles      func(ctx context.Context, kind string, id int) (online.TitleCtx, error)
	UsersBudget time.Duration // 0: 10 s
	Log         *slog.Logger
	Now         func() time.Time
	// OnReady is called once everything is up (main: addonkit.Ready.Set).
	OnReady func()
}

// Booted is the addon's running state.
type Booted struct {
	DB       *store.DB
	Settings store.Settings
	// Import is the one-time import's counts (Imported: it has run, on
	// this start or an earlier one).
	Import   store.ImportCounts
	Imported bool
	// Online is «Смотреть онлайн» as built at start (nil without a usable
	// Lampac URL); Service is the one in force after a change of
	// «Настройки» (SaveLampac).
	Online *online.Service
	// Progress is the online watch progress, on the addon's database,
	// mirrored into the library's (built with or without Lampac).
	Progress *online.ProgressStore

	// Overridden: the service is on an address of the admin's (the
	// variable ANTRESOL_ONLINE_LAMPAC_URL or «Настройки»), as at start.
	Overridden bool
	// Helper is Config.Helper (nil: none declared).
	Helper *addonkit.HelperWatch
	// onChange (main, SetOnChange) is called with the new service after a
	// change of the Lampac settings rebuilt it (nil when there is none now).
	onChange func(*online.Service)

	enabled atomic.Bool
	closed  atomic.Bool

	cfg      Config
	refKey   []byte
	proxyKey []byte
	lmu      sync.Mutex // one SaveLampac at a time
	conn     atomic.Pointer[lampacConn]
	svc      atomic.Pointer[online.Service]
	prefs    atomic.Pointer[online.SourcePrefs]
	// watchHealth keeps the compose helper's probes (Helper.Health).
	watchHealth *addonkit.HelperHealth
}

// Usable reports whether Lampac can be asked: there is a service, and it
// is on the admin's address or the helper answers on its compose name.
func (b *Booted) Usable() bool {
	return b != nil && b.Service() != nil && b.up()
}

// up: Lampac is on the admin's address, or the helper answers.
func (b *Booted) up() bool {
	c := b.conn.Load()
	return c != nil && (c.source == addonkit.HelperOverride || c.source == HelperSettings) || b.Helper.Found()
}

// serving is what the service takes as its on: the admin setting, and
// Lampac there to ask (so the scanner and the channels wait for a helper
// that does not answer yet, as the routes do).
func (b *Booted) serving() bool { return b.Enabled() && b.up() }

// HelperStatus is the Lampac line of the settings page.
func (b *Booted) HelperStatus() addonkit.HelperStatus {
	src := addonkit.HelperNone
	if c := b.conn.Load(); c != nil {
		src = c.source
	}
	own := src == addonkit.HelperOverride || src == HelperSettings
	if b.Helper == nil {
		if own {
			return addonkit.HelperStatus{Source: src}
		}
		return addonkit.HelperStatus{Source: addonkit.HelperNone}
	}
	st := b.Helper.Status(own)
	if own {
		st.Source = src
	}
	return st
}

// Enabled is the admin setting «Смотреть онлайн» as last read or saved.
func (b *Booted) Enabled() bool { return b.enabled.Load() }

// SetEnabled saves the setting and applies it at once.
func (b *Booted) SetEnabled(ctx context.Context, on bool) error {
	if err := b.Settings.SetEnabled(ctx, on); err != nil {
		return err
	}
	b.enabled.Store(on)
	return nil
}

// Close closes the database (once).
func (b *Booted) Close() error {
	if b == nil || b.closed.Swap(true) {
		return nil
	}
	return b.DB.Close()
}

func (c Config) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Run brings the addon up (see the package comment) and calls OnReady at
// the end. Any error is logged and returned, and OnReady is not called.
func Run(ctx context.Context, c Config) (*Booted, error) {
	b, err := bringUp(ctx, c)
	if err != nil {
		c.log().Error("online: not ready", "err", err)
		return nil, err
	}
	if c.OnReady != nil {
		c.OnReady()
	}
	return b, nil
}

func bringUp(ctx context.Context, c Config) (*Booted, error) {
	if c.DataDir == "" {
		return nil, errors.New("boot: no data dir")
	}
	lg := c.log()
	d, err := store.Open(c.DataDir)
	if err != nil {
		return nil, err
	}
	b := &Booted{DB: d, Settings: store.Settings{DB: d.DB}}
	fail := func(err error) (*Booted, error) {
		b.Close()
		return nil, err
	}

	// the one-time import, before anything reads the progress
	if c.ImportFrom != "" {
		counts, err := store.Import(ctx, d, c.ImportFrom, c.Now)
		if err != nil {
			return fail(fmt.Errorf("boot: import from vault.db: %w", err))
		}
		b.Import, b.Imported = counts, true
		lg.Info("online: import from vault.db", "rows", counts.Rows, "profiles", counts.Users,
			"orphans", counts.Orphans, "enabled", counts.Enabled)
	} else {
		// an earlier start's snapshot goes even with the source unset now
		if err := store.RemoveImportLeftovers(c.DataDir); err != nil {
			return fail(err)
		}
		counts, done, err := store.ImportState(ctx, d)
		if err != nil {
			return fail(err)
		}
		b.Import, b.Imported = counts, done
	}

	dropDeletedProfiles(ctx, c, d)

	on, err := b.Settings.Enabled(ctx)
	if err != nil {
		return fail(err)
	}
	b.enabled.Store(on)
	refKey, proxyKey, err := b.Settings.Keys(ctx)
	if err != nil {
		return fail(fmt.Errorf("boot: keys: %w", err))
	}
	b.Helper = c.Helper
	b.cfg, b.refKey, b.proxyKey = c, refKey, proxyKey
	saved, err := b.Settings.Lampac(ctx)
	if err != nil {
		lg.Warn("online: the saved Lampac settings do not read; the server's are used", "err", err)
		saved = store.Lampac{}
	}
	sp, err := b.Settings.LampacSources(ctx)
	if err != nil {
		lg.Warn("online: the saved choice of sources does not read; all are offered", "err", err)
	}
	b.prefs.Store(&online.SourcePrefs{Order: sp.Order, Off: sp.Off})
	if c.Helper != nil {
		b.watchHealth = &addonkit.HelperHealth{}
		c.Helper.Health = b.watchHealth
	}
	uid := ""
	if saved.AuthKey == AuthShared {
		// the addon's id, registered again in the background (Lampac may
		// be down at start, or the id's time ran out)
		if uid, err = b.Settings.LampacUID(ctx, false); err != nil {
			return fail(err)
		}
		go b.renewShared(context.WithoutCancel(ctx), saved)
	}
	conn := connect(saved, uid, c.LampacURL, c.Helper)
	b.Overridden = conn.source == addonkit.HelperOverride || conn.source == HelperSettings
	if conn.source == addonkit.HelperCompose && !c.Helper.Check(ctx) {
		lg.Warn("online: no Lampac at its compose address yet; asked again every 30 s", "address", c.Helper.Helper.Address())
	}
	b.Online = b.build(conn)
	b.Progress = &online.ProgressStore{DB: d.DB, Lib: c.Library, Progress: c.LibraryProgress, Users: c.Users, Now: c.Now}
	return b, nil
}

// dropDeletedProfiles deletes the rows of every uid GET /users no longer
// lists (a deleted profile). A failing or hung call skips it; the start
// goes on.
func dropDeletedProfiles(ctx context.Context, c Config, d *store.DB) {
	if c.Users == nil {
		return
	}
	budget := c.UsersBudget
	if budget <= 0 {
		budget = defaultUsersBudget
	}
	uctx, cancel := context.WithTimeout(ctx, budget)
	us, err := c.Users.Users(uctx)
	cancel()
	if err != nil {
		c.log().Warn("online: profiles not read, deleted profiles' rows kept until the next scan", "err", err)
		return
	}
	uids := make([]string, 0, len(us))
	for _, u := range us {
		uids = append(uids, u.UID)
	}
	n, err := store.DropProfilesExcept(ctx, d.DB, uids)
	if err != nil {
		c.log().Warn("online: deleted profiles' rows not dropped", "err", err)
		return
	}
	if n > 0 {
		c.log().Info("online: rows of deleted profiles dropped", "rows", n)
	}
}

// newOnline builds «Смотреть онлайн» on the Lampac URL of conn (nil
// without one, or with one that does not parse: available false). Only
// the origin is logged, never the URL's uid. The proxy's URLs are under
// online.StreamBase, the routes' path through the core's proxy.
func newOnline(c Config, conn *lampacConn, refKey, proxyKey []byte, enabled func() bool, prefs func() online.SourcePrefs) *online.Service {
	if conn.url == "" {
		return nil
	}
	lg := c.log()
	l, err := online.NewLampac(conn.url)
	if err != nil {
		lg.Error("online: the Lampac address does not parse; streaming is unavailable", "from", conn.source, "err", err)
		return nil
	}
	l.Timeout, l.Health = conn.timeout, conn.health
	lg.Info("online: Lampac", "origin", l.Base.Scheme+"://"+l.Base.Host)
	slg := lg.With("component", "online")
	up := online.NewUpstreamClient(l)
	return &online.Service{L: l, Refs: online.NewRefs(refKey, l.Base), Log: slg, Upstream: up, Enabled: enabled, Titles: c.Titles,
		Prefs: prefs, SourcesBudget: conn.sourcesWait,
		Proxy: &online.Proxy{Key: proxyKey, HTTP: up, Lampac: l, Log: slg, Base: online.StreamBase},
		Subs:  &online.Subs{Dir: filepath.Join(c.DataDir, "subs"), FFmpeg: c.FFmpeg, HTTP: up, Lampac: l}}
}
