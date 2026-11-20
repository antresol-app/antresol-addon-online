package boot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol-addon-online/internal/store"
	"github.com/antresol-app/antresol/addonkit"
)

func lampacWatch() *addonkit.HelperWatch {
	return &addonkit.HelperWatch{Helper: addonkit.Helper{Name: "lampac", Image: "img", Port: 9118, Health: "/"},
		Probe: func(context.Context) error { return errors.New("down") }}
}

// The address in force: the server's variable, else «Настройки», else
// the helper's compose name; the saved account replaces the address'
// own account keys, on any of them.
func TestConnect(t *testing.T) {
	w := lampacWatch()
	if c := connect(store.Lampac{}, "", "", w); c.source != addonkit.HelperCompose || c.url != "http://lampac:9118/" {
		t.Fatalf("compose: %+v", c)
	}
	if c := connect(store.Lampac{}, "", "", nil); c.source != addonkit.HelperNone || c.url != "" {
		t.Fatalf("none: %+v", c)
	}
	if c := connect(store.Lampac{}, "", "http://h:1/?uid=x", w); c.source != addonkit.HelperOverride || c.url != "http://h:1/?uid=x" {
		t.Fatalf("env: %+v", c)
	}
	c := connect(store.Lampac{URL: "http://own:2/", AuthKey: "account_email", AuthValue: "a@b.c", Timeout: 30}, "", "", w)
	if c.source != HelperSettings || !strings.HasPrefix(c.url, "http://own:2/?") || !strings.Contains(c.url, "account_email=a%40b.c") || c.timeout.Seconds() != 30 {
		t.Fatalf("settings: %+v", c)
	}
	// an account for the variable's address replaces its uid
	// the variable wins over the saved address; the saved account applies to it
	c = connect(store.Lampac{URL: "http://own:2/", AuthKey: "uid", AuthValue: "new"}, "", "http://h:1/?uid=old&x=1", nil)
	if c.source != addonkit.HelperOverride || c.url != "http://h:1/?uid=new&x=1" {
		t.Fatalf("account on env: %+v", c)
	}
	// the shared password: the addon's own id goes as uid
	c = connect(store.Lampac{URL: "http://own:2/", AuthKey: AuthShared, AuthValue: "pw"}, "antresol-1", "", nil)
	if c.url != "http://own:2/?uid=antresol-1" || strings.Contains(c.url, "pw") {
		t.Fatalf("shared: %q", c.url)
	}
}

// «Настройки»: saving an address rebuilds the service on it at once
// (OnChange), the account's value is never shown, bad values are refused
// and change nothing.
func TestSaveLampac(t *testing.T) {
	b, err := run(t, Config{DataDir: t.TempDir(), Helper: lampacWatch()})
	if err != nil {
		t.Fatal(err)
	}
	var changed *online.Service
	calls := -1 // SetOnChange calls it once with the service in force
	b.SetOnChange(func(s *online.Service) { changed, calls = s, calls+1 })
	ctx := context.Background()
	if v, _ := b.LampacView(ctx); v.Source != addonkit.HelperCompose || v.URL != "" || v.Server {
		t.Fatalf("start: %+v", v)
	}
	if b.Usable() {
		t.Fatal("usable with the helper down")
	}
	secret := "s3cret"
	if err := b.SaveLampac(ctx, LampacSave{URL: "lampac.lan:9118", AuthKey: "uid", AuthValue: &secret, Timeout: 40}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || changed == nil || changed != b.Service() || changed.L.Base.Host != "lampac.lan:9118" ||
		changed.L.Base.Query().Get("uid") != secret || changed.L.Timeout.Seconds() != 40 {
		t.Fatalf("rebuilt: %d %+v", calls, changed)
	}
	if !b.Usable() || b.HelperStatus().Source != HelperSettings || b.HelperStatus().Address != "" {
		t.Fatalf("own address: %v %+v", b.Usable(), b.HelperStatus())
	}
	v, err := b.LampacView(ctx)
	if err != nil || v.URL != "http://lampac.lan:9118/" || !v.AuthSet || v.AuthKey != "uid" || v.Timeout != 40 || v.Source != HelperSettings {
		t.Fatalf("view: %+v %v", v, err)
	}
	// the addon's own id, for Lampac's Telegram bot: short, never the account value
	if uid, _ := b.Settings.LampacUID(ctx, false); v.UID != uid || len(uid) > 20 || uid == secret {
		t.Fatalf("view uid %q, saved %q", v.UID, uid)
	}
	// the value kept when not sent again
	if err := b.SaveLampac(ctx, LampacSave{URL: "http://lampac.lan:9118/", AuthKey: "uid"}); err != nil {
		t.Fatal(err)
	}
	if b.Service().L.Base.Query().Get("uid") != secret {
		t.Fatal("the account was dropped")
	}
	for _, bad := range []LampacSave{{URL: "ftp://x"}, {AuthKey: "password"}, {AuthKey: "token", AuthValue: new(string)}, {Timeout: 2}, {SourcesWait: 500}} {
		err := b.SaveLampac(ctx, bad)
		var se *SettingsError
		if !errors.As(err, &se) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if calls != 2 {
		t.Fatalf("a refused change rebuilt: %d", calls)
	}
	// cleared: back to the helper
	if err := b.SaveLampac(ctx, LampacSave{}); err != nil {
		t.Fatal(err)
	}
	if b.Service().L.Base.Host != "lampac:9118" || b.Usable() {
		t.Fatalf("cleared: %+v usable %v", b.Service().L.Base, b.Usable())
	}
	// a new start reads the saved address
	if err := b.SaveLampac(ctx, LampacSave{URL: "http://other:1/"}); err != nil {
		t.Fatal(err)
	}
	dir := b.cfg.DataDir
	b.Close()
	b2, err := run(t, Config{DataDir: dir, LampacURL: "http://env:3/"})
	if err != nil {
		t.Fatal(err)
	}
	// the server's variable wins over it
	if b2.Online.L.Base.Host != "env:3" {
		t.Fatalf("restart: %+v", b2.Online.L.Base)
	}
	if v, _ := b2.LampacView(ctx); !v.Server || v.Source != addonkit.HelperOverride || v.URL != "http://other:1/" {
		t.Fatalf("restart view: %+v", v)
	}

}

// The sources: Lampac's list merged with the admin's choice, which the
// service applies at once.
func TestLampacSourcesChoice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lite/events":
			w.Write([]byte(`[{"name":"Alpha","url":"/lite/alpha","balanser":"alpha","show":true},
				{"name":"Beta","url":"/lite/beta","balanser":"beta","show":false}]`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	b, err := run(t, Config{DataDir: t.TempDir(), LampacURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := b.Sources(ctx)
	if err != nil || len(v.Sources) != 2 || v.Sources[0].ID != "alpha" || !v.Sources[0].Enabled || !v.Sources[0].Listed || v.Sources[1].Found {
		t.Fatalf("%+v %v", v, err)
	}
	if err := b.SaveSources(ctx, SourcesSave{Order: []string{"beta", "alpha"}, Off: []string{"alpha", "gone"}}); err != nil {
		t.Fatal(err)
	}
	v, _ = b.Sources(ctx)
	got := ""
	for _, s := range v.Sources {
		got += s.ID + ":" + map[bool]string{true: "on", false: "off"}[s.Enabled] + " "
	}
	if got != "beta:on alpha:off gone:off " {
		t.Fatalf("%q", got)
	}
	if p := b.Service().Prefs(); len(p.Off) != 2 || p.Order[0] != "beta" {
		t.Fatalf("service prefs %+v", p)
	}
	if err := b.SaveSources(ctx, SourcesSave{Off: []string{"bad id"}}); err == nil {
		t.Fatal("a bad id saved")
	}
}

// helpers.status: the address in force, never an admin's; the outcome of
// the service's own last call when fresh.
func TestHelpersStatus(t *testing.T) {
	ctx := context.Background()
	b, err := run(t, Config{DataDir: t.TempDir(), Helper: lampacWatch()})
	if err != nil {
		t.Fatal(err)
	}
	st := b.HelpersStatus(ctx).Helpers
	if len(st) != 1 || st[0].Name != "lampac" || st[0].Source != addonkit.HelperCompose || st[0].Address != "http://lampac:9118" || st[0].OK {
		t.Fatalf("compose: %+v", st)
	}
	secret := "s3cret"
	if err := b.SaveLampac(ctx, LampacSave{URL: "http://127.0.0.1:1/", AuthKey: "uid", AuthValue: &secret}); err != nil {
		t.Fatal(err)
	}
	b.conn.Load().health.Record(nil) // a call that went through
	st = b.HelpersStatus(ctx).Helpers
	if st[0].Source != HelperSettings || st[0].Address != "" || !st[0].OK || !st[0].Live {
		t.Fatalf("settings: %+v", st)
	}
	b2, err := run(t, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if st := b2.HelpersStatus(ctx).Helpers; st[0].Source != addonkit.HelperNone || st[0].Problem != addonkit.HelperProblemUnset {
		t.Fatalf("none: %+v", st)
	}
}

// fakeAccsdb is a Lampac with accounts on: /lite/events answers
// {"accsdb": true} unless uid is one it knows; /testaccsdb registers a uid
// with the shared password (Lampac's ApiController.TestAccsdb), once.
func fakeAccsdb(t *testing.T, password string, known ...string) (*httptest.Server, map[string]bool) {
	ids := map[string]bool{}
	for _, k := range known {
		ids[k] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/testaccsdb":
			switch {
			case q.Get("account_email") != password:
				w.Write([]byte(`{"accsdb": false, "success": true}`)) // Lampac's answer to anything else
			case func() bool { _, ok := ids[q.Get("uid")]; return ok }():
				w.Write([]byte(`{"accsdb": false}`)) // known (an expired one too): not registered again
			default:
				ids[q.Get("uid")] = true
				w.Write([]byte(`{"accsdb": false, "success": true, "uid": "` + q.Get("uid") + `"}`))
			}
		case "/lite/events":
			if !ids[q.Get("uid")] {
				w.Write([]byte(`{"accsdb": true, "msg": "no"}`))
				return
			}
			w.Write([]byte(`[{"name":"Alpha","url":"/lite/alpha","balanser":"alpha","show":true},{"name":"R","url":"/lite/r","balanser":"r","rch":true}]`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, ids
}

// «Проверить»: a real request with the values typed (not saved): ok with
// the number of sources, auth when the account is refused, down when
// nothing answers; the server's variable still wins for the address.
func TestCheckLampac(t *testing.T) {
	srv, _ := fakeAccsdb(t, "", "good")
	b, err := run(t, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		in   LampacSave
		want CheckResult
	}{
		{LampacSave{URL: srv.URL, AuthKey: "uid", AuthValue: str("good")}, CheckResult{Result: "ok", Sources: 1}},
		{LampacSave{URL: srv.URL, AuthKey: "uid", AuthValue: str("bad")}, CheckResult{Result: "auth"}},
		{LampacSave{URL: srv.URL}, CheckResult{Result: "auth"}},
		{LampacSave{URL: "http://127.0.0.1:1"}, CheckResult{Result: "down"}},
	} {
		if got, err := b.CheckLampac(ctx, c.in); err != nil || got != c.want {
			t.Errorf("%+v: %+v %v, want %+v", c.in, got, err, c.want)
		}
	}
	if v, _ := b.LampacView(ctx); v.URL != "" {
		t.Fatal("a check saved")
	}
	// the variable wins: the typed address is not asked
	b2, err := run(t, Config{DataDir: t.TempDir(), LampacURL: "http://127.0.0.1:1/"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := b2.CheckLampac(ctx, LampacSave{URL: srv.URL, AuthKey: "uid", AuthValue: str("good")}); got.Result != "down" {
		t.Fatalf("variable: %+v", got)
	}
}

// The shared password: the addon registers its own id with it and sends
// that id; a wrong password is refused; an id whose time ran out is
// replaced by a new one.
func TestSharedPassword(t *testing.T) {
	srv, ids := fakeAccsdb(t, "pw")
	b, err := run(t, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	str := func(s string) *string { return &s }
	if r, err := b.CheckLampac(ctx, LampacSave{URL: srv.URL, AuthKey: AuthShared, AuthValue: str("pw")}); err != nil || r.Result != "ok" {
		t.Fatalf("check: %+v %v", r, err)
	}
	if err := b.SaveLampac(ctx, LampacSave{URL: srv.URL, AuthKey: AuthShared, AuthValue: str("pw")}); err != nil {
		t.Fatal(err)
	}
	uid, _ := b.Settings.LampacUID(ctx, false)
	if !ids[uid] || b.Service().L.Base.Query().Get("uid") != uid || strings.Contains(b.Service().L.Base.String(), "pw") {
		t.Fatalf("registered %v, base %v", ids, b.Service().L.Base)
	}
	if v, _ := b.LampacView(ctx); v.AuthKey != AuthShared || !v.AuthSet {
		t.Fatalf("view %+v", v)
	}
	// its time ran out: Lampac keeps it, refused, and will not register it again
	ids[uid] = false
	if r, _ := b.CheckLampac(ctx, LampacSave{URL: srv.URL, AuthKey: AuthShared}); r.Result != "ok" {
		t.Fatalf("renew: %+v", r)
	}
	if nu, _ := b.Settings.LampacUID(ctx, false); nu == uid || !ids[nu] {
		t.Fatalf("not renewed: %q", nu)
	}
}
