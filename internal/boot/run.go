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
	"sync/atomic"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol-addon-online/internal/store"
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
	// LampacURL is ANTRESOL_ONLINE_LAMPAC_URL ("": no service, the addon
	// runs with available: false).
	LampacURL string
	FFmpeg    string // the core's ffmpeg (subtitles); "": ffmpeg
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
	// Online is «Смотреть онлайн» (nil without a usable Lampac URL).
	Online *online.Service
	// Progress is the online watch progress, on the addon's database,
	// mirrored into the library's (built with or without Lampac).
	Progress *online.ProgressStore

	enabled atomic.Bool
	closed  atomic.Bool
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
	b.Online = newOnline(c, refKey, proxyKey, b.Enabled)
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

// newOnline builds «Смотреть онлайн» on the Lampac URL (nil without one, or
// with one that does not parse: available false). Only the origin is
// logged, never the URL's uid. The proxy's URLs are under
// online.StreamBase, the routes' path through the core's proxy.
func newOnline(c Config, refKey, proxyKey []byte, enabled func() bool) *online.Service {
	if c.LampacURL == "" {
		return nil
	}
	lg := c.log()
	l, err := online.NewLampac(c.LampacURL)
	if err != nil {
		lg.Error("online: ANTRESOL_ONLINE_LAMPAC_URL does not parse; streaming is unavailable", "err", err)
		return nil
	}
	lg.Info("online: Lampac", "origin", l.Base.Scheme+"://"+l.Base.Host)
	slg := lg.With("component", "online")
	up := online.NewUpstreamClient(l)
	return &online.Service{L: l, Refs: online.NewRefs(refKey, l.Base), Log: slg, Upstream: up, Enabled: enabled, Titles: c.Titles,
		Proxy: &online.Proxy{Key: proxyKey, HTTP: up, Lampac: l, Log: slg, Base: online.StreamBase},
		Subs:  &online.Subs{Dir: filepath.Join(c.DataDir, "subs"), FFmpeg: c.FFmpeg, HTTP: up, Lampac: l}}
}
