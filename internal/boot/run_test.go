package boot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/coretest"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
	_ "modernc.org/sqlite"
)

// logBuf is a slog text log the test reads.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type users struct {
	list []online.User
	err  error
}

func (u users) Users(context.Context) ([]online.User, error) { return u.list, u.err }

// source is a vault.db from the store's fixture.
func source(t *testing.T, edit ...string) string {
	t.Helper()
	from := filepath.Join(t.TempDir(), "vault.db")
	body, err := os.ReadFile("../store/testdata/vault-fixture.sql")
	if err != nil {
		t.Fatal(err)
	}
	v, err := sql.Open("sqlite", "file:"+from+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	for _, e := range edit {
		if _, err := v.Exec(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	return from
}

func health(t *testing.T, r *addonkit.Ready) bool {
	t.Helper()
	w := httptest.NewRecorder()
	r.Health(w, httptest.NewRequest("GET", "/health", nil))
	var h struct{ OK, Ready bool }
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil || !h.OK {
		t.Fatalf("health %s %v", w.Body, err)
	}
	return h.Ready
}

func all() users {
	return users{list: []online.User{{ID: 1, UID: "uid-admin"}, {ID: 2, UID: "uid-second"}, {ID: 3, UID: "uid-third"}}}
}

func run(t *testing.T, c Config) (*Booted, error) {
	t.Helper()
	b, err := Run(context.Background(), c)
	if b != nil {
		t.Cleanup(func() { b.Close() })
	}
	return b, err
}

// TestImport_FailureNotReady: a failed import leaves the addon not ready
// (the health check says ready:false), the error is in the log, and the
// next start runs the import again.
func TestImport_FailureNotReady(t *testing.T) {
	data := t.TempDir()
	var log logBuf
	ready := &addonkit.Ready{}
	bad := source(t, `UPDATE settings SET value='{"onlineEnabled":"no"}' WHERE key='app_settings'`)
	c := Config{DataDir: data, ImportFrom: bad, Users: all(), Log: slog.New(slog.NewTextHandler(&log, nil)), OnReady: ready.Set}
	if _, err := run(t, c); err == nil {
		t.Fatal("boot passed a failed import")
	}
	if health(t, ready) {
		t.Fatal("ready after a failed import")
	}
	if !strings.Contains(log.String(), "import") || !strings.Contains(log.String(), "onlineEnabled") {
		t.Fatalf("the log does not have the error: %s", log.String())
	}
	// the second start (the source fixed) imports and becomes ready
	good := source(t)
	c.ImportFrom = good
	b, err := run(t, c)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if !health(t, ready) {
		t.Fatal("not ready after the retried import")
	}
	if !b.Imported || b.Import.Rows != 4 || b.Import.Enabled {
		t.Fatalf("import %v %+v", b.Imported, b.Import)
	}
	if b.Enabled() {
		t.Fatal("«Смотреть онлайн» on: the imported setting was off")
	}
}

// TestBoot_ImportOnlyWhenConfigured: without ANTRESOL_ONLINE_IMPORT_FROM
// nothing is imported and the setting defaults to on; a later start with
// it set imports once, and a third start does not import again.
func TestBoot_ImportOnlyWhenConfigured(t *testing.T) {
	data := t.TempDir()
	b, err := run(t, Config{DataDir: data, Users: all()})
	if err != nil {
		t.Fatal(err)
	}
	if b.Imported || !b.Enabled() {
		t.Fatalf("imported %v enabled %v", b.Imported, b.Enabled())
	}
	b.Close()
	from := source(t)
	b, err = run(t, Config{DataDir: data, ImportFrom: from, Users: all()})
	if err != nil || !b.Imported || b.Import.Rows != 4 {
		t.Fatalf("import start: %v %+v", err, b)
	}
	b.Close()
	os.Remove(from)
	b, err = run(t, Config{DataDir: data, ImportFrom: from, Users: all()})
	if err != nil || !b.Imported || b.Import.Rows != 4 {
		t.Fatalf("third start: %v (the recorded import must answer)", err)
	}
}

// TestBoot_DropsRowsOfDeletedProfiles: after the import, the rows of a
// uid /users no longer lists go; a /users error keeps every row and the
// addon still becomes ready.
func TestBoot_DropsRowsOfDeletedProfiles(t *testing.T) {
	rows := func(b *Booted) map[string]int {
		r, err := b.DB.Query(`SELECT user_uid, count(*) FROM online_progress GROUP BY user_uid`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string]int{}
		for r.Next() {
			var u string
			var n int
			r.Scan(&u, &n)
			out[u] = n
		}
		return out
	}
	// /users fails: nothing dropped, ready
	data := t.TempDir()
	ready := &addonkit.Ready{}
	b, err := run(t, Config{DataDir: data, ImportFrom: source(t), Users: users{err: errors.New("core down")}, OnReady: ready.Set})
	if err != nil || !health(t, ready) {
		t.Fatalf("a /users error blocked the start: %v", err)
	}
	if got := rows(b); len(got) != 3 {
		t.Fatalf("rows %v: a /users error dropped rows", got)
	}
	b.Close()
	// the next start: uid-second is deleted
	ready = &addonkit.Ready{}
	gone := users{list: []online.User{{ID: 1, UID: "uid-admin"}, {ID: 3, UID: "uid-third"}}}
	b, err = run(t, Config{DataDir: data, Users: gone, OnReady: ready.Set})
	if err != nil || !health(t, ready) {
		t.Fatalf("start: %v", err)
	}
	if got := rows(b); got["uid-second"] != 0 || got["uid-admin"] != 2 || got["uid-third"] != 1 {
		t.Fatalf("rows %v, want uid-second's gone", got)
	}
}

// TestBoot_UsersHungDoesNotBlock: a /users call that never answers is
// given up on, and the addon becomes ready.
func TestBoot_UsersHungDoesNotBlock(t *testing.T) {
	ready := &addonkit.Ready{}
	start := time.Now()
	_, err := run(t, Config{DataDir: t.TempDir(), Users: hung{}, UsersBudget: 50 * time.Millisecond, OnReady: ready.Set})
	if err != nil || !health(t, ready) {
		t.Fatalf("hung /users: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the hung /users held the start")
	}
}

type hung struct{}

func (hung) Users(ctx context.Context) ([]online.User, error) { <-ctx.Done(); return nil, ctx.Err() }

// TestBoot_BuildsOnline: with a Lampac URL the service is built with the
// stored keys and the setting; without one (or with one that does not
// parse) there is none (available: false), and the addon is still ready.
func TestBoot_BuildsOnline(t *testing.T) {
	data := t.TempDir()
	var log logBuf
	b, err := run(t, Config{DataDir: data, LampacURL: "http://127.0.0.1:9118", Log: slog.New(slog.NewTextHandler(&log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if b.Online == nil || b.Online.Refs == nil || b.Online.Proxy == nil || len(b.Online.Proxy.Key) != 32 || b.Online.Enabled == nil {
		t.Fatalf("service %+v", b.Online)
	}
	if !b.Online.Enabled() {
		t.Fatal("the service is off by default")
	}
	if err := b.SetEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if b.Online.Enabled() {
		t.Fatal("SetEnabled did not reach the service")
	}
	if on, _ := b.Settings.Enabled(context.Background()); on {
		t.Fatal("SetEnabled was not saved")
	}
	b.Close()
	for _, u := range []string{"", "::not a url"} {
		ready := &addonkit.Ready{}
		b, err := run(t, Config{DataDir: data, LampacURL: u, Log: slog.New(slog.NewTextHandler(&log, nil)), OnReady: ready.Set})
		if err != nil || b.Online != nil || !health(t, ready) {
			t.Fatalf("lampac %q: %v %+v", u, err, b)
		}
		if b.Enabled() {
			t.Fatal("the saved off was lost")
		}
		b.Close()
	}
	if !strings.Contains(log.String(), "LAMPAC_URL") {
		t.Fatalf("the URL that does not parse is not in the log: %s", log.String())
	}
}

// TestBoot_KnownSourcesSurviveRestart: the service saves the balancers its
// lists named in the addon's own store, and the next start loads them.
func TestBoot_KnownSourcesSurviveRestart(t *testing.T) {
	data := t.TempDir()
	ctx := context.Background()
	b, err := run(t, Config{DataDir: data, LampacURL: "http://127.0.0.1:9118"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Online.LoadKnown == nil || b.Online.SaveKnown == nil {
		t.Fatal("the service keeps no balancers across a restart")
	}
	if shown, _, err := b.Online.LoadKnown(ctx); err != nil || len(shown) != 0 {
		t.Fatalf("a fresh store: %v %v", shown, err)
	}
	if err := b.Online.SaveKnown(ctx, map[string]bool{"alpha": true, "beta": false}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	b2, err := run(t, Config{DataDir: data, LampacURL: "http://127.0.0.1:9118"})
	if err != nil {
		t.Fatal(err)
	}
	shown, at, err := b2.Online.LoadKnown(ctx)
	if err != nil || len(shown) != 2 || !shown["alpha"] || shown["beta"] || time.Since(at) > time.Minute {
		t.Fatalf("after a restart: %v %v %v", shown, at, err)
	}
}

// TestBoot_RemovesImportLeftoverWithoutSource: a start with no import
// source set (any more) still deletes an earlier run's snapshot.
func TestBoot_RemovesImportLeftoverWithoutSource(t *testing.T) {
	data := t.TempDir()
	p := filepath.Join(data, "import", "vault.db-0123abcd")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("a copy of vault.db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, Config{DataDir: data}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "import")); !os.IsNotExist(err) {
		t.Fatalf("the leftover snapshot is still there: %v", err)
	}
}

// TestBoot_WiresRoutesDeps: the service's titles, the subtitles' ffmpeg
// and the proxy's base are set from Config, and the progress store is
// built on the addon's database with the core's library and progress
// (also without Lampac: the store is the addon's own data).
func TestBoot_WiresRoutesDeps(t *testing.T) {
	core := &coretest.Core{}
	titles := func(context.Context, string, int) (online.TitleCtx, error) { return online.TitleCtx{Title: "Т"}, nil }
	b, err := run(t, Config{DataDir: t.TempDir(), LampacURL: "http://127.0.0.1:9118", FFmpeg: "/opt/ff/ffmpeg",
		Titles: titles, Library: core, LibraryProgress: core, Users: core})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Online.Titles == nil || b.Online.Subs.FFmpeg != "/opt/ff/ffmpeg" || b.Online.Proxy.Base != online.StreamBase {
		t.Fatalf("service %+v", b.Online)
	}
	if tc, _ := b.Online.Titles(context.Background(), "movie", 1); tc.Title != "Т" {
		t.Fatalf("titles %+v", tc)
	}
	p := b.Progress
	if p == nil || p.DB != b.DB.DB || p.Lib != online.LibraryLookup(core) || p.Progress != online.LibraryProgress(core) ||
		p.Users != online.UserLister(core) {
		t.Fatalf("progress %+v", p)
	}
	b2, err := run(t, Config{DataDir: t.TempDir(), Library: core, LibraryProgress: core})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if b2.Online != nil || b2.Progress == nil || b2.Progress.Lib == nil {
		t.Fatalf("no Lampac: %+v %+v", b2.Online, b2.Progress)
	}
}

// TestBoot_LampacByComposeName: without ANTRESOL_ONLINE_LAMPAC_URL the
// declared helper's compose address is used, and the service is usable
// only while the helper answers; the variable, when set, wins and the
// helper is not asked.
func TestBoot_LampacByComposeName(t *testing.T) {
	up := false
	asked := 0
	w := &addonkit.HelperWatch{Helper: addonkit.Helper{Name: "lampac", Image: "img", Port: 9118, Health: "/"},
		Probe: func(context.Context) error {
			asked++
			if up {
				return nil
			}
			return errors.New("down")
		}}
	b, err := run(t, Config{DataDir: t.TempDir(), Helper: w})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Online == nil || b.Online.L.Base.String() != "http://lampac:9118/" {
		t.Fatalf("service on %+v", b.Online)
	}
	if asked != 1 || b.Usable() || b.Online.Enabled() {
		t.Fatalf("asked %d, usable %v, on %v: want 1, false, false", asked, b.Usable(), b.Online.Enabled())
	}
	if st := b.HelperStatus(); st.Source != addonkit.HelperNone || !strings.Contains(st.Compose, "lampac:") {
		t.Fatalf("status %+v", st)
	}
	up = true
	w.Check(context.Background())
	if !b.Usable() || !b.Online.Enabled() || b.HelperStatus().Source != addonkit.HelperCompose {
		t.Fatalf("found: usable %v %+v", b.Usable(), b.HelperStatus())
	}

	asked = 0
	b2, err := run(t, Config{DataDir: t.TempDir(), Helper: w, LampacURL: "http://192.168.50.10:9118/?uid=x"})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if asked != 0 || b2.Online.L.Base.Host != "192.168.50.10:9118" || !b2.Usable() || b2.HelperStatus().Source != addonkit.HelperOverride {
		t.Fatalf("override: asked %d %+v %+v", asked, b2.Online.L.Base, b2.HelperStatus())
	}
	if b2.HelperStatus().Address != "" {
		t.Fatal("the admin's address is never shown")
	}

	// no variable, no helper: no service, as before
	b3, err := run(t, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b3.Close()
	if b3.Online != nil || b3.Usable() || b3.HelperStatus().Source != addonkit.HelperNone {
		t.Fatalf("none: %+v %+v", b3.Online, b3.HelperStatus())
	}
}
