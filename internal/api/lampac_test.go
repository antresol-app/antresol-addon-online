package api

import (
	"context"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/boot"
	"github.com/antresol-app/antresol-addon-online/internal/online"
)

type fakeLampacSettings struct {
	saved   boot.LampacSave
	sources boot.SourcesSave
	refuse  bool
	noSvc   bool
}

func (f *fakeLampacSettings) LampacView(context.Context) (boot.LampacView, error) {
	return boot.LampacView{URL: f.saved.URL, AuthKey: f.saved.AuthKey, AuthSet: f.saved.AuthValue != nil, Source: "settings"}, nil
}

func (f *fakeLampacSettings) SaveLampac(_ context.Context, in boot.LampacSave) error {
	if f.refuse {
		return &boot.SettingsError{Msg: "Неверный источник"}
	}
	f.saved = in
	return nil
}

func (f *fakeLampacSettings) CheckLampac(_ context.Context, in boot.LampacSave) (boot.CheckResult, error) {
	if in.AuthValue != nil && *in.AuthValue == "good" {
		return boot.CheckResult{Result: "ok", Sources: 3}, nil
	}
	return boot.CheckResult{Result: "auth"}, nil
}

func (f *fakeLampacSettings) Sources(context.Context) (boot.SourcesView, error) {
	if f.noSvc {
		return boot.SourcesView{}, boot.ErrNoLampac
	}
	return boot.SourcesView{Sources: []boot.SourceRow{{ID: "alpha", Name: "Alpha", Enabled: true, Listed: true}}}, nil
}

func (f *fakeLampacSettings) SaveSources(_ context.Context, in boot.SourcesSave) error {
	f.sources = in
	return nil
}

// «Настройки»'s Lampac routes: admins only; the account's value is
// taken, never given back; a refused value is 422.
func TestLampacSettingsRoutes(t *testing.T) {
	f := &fakeLampacSettings{}
	e := newEnv(t, nil, func(d *Deps) { d.Lampac = f })
	const p = "/api/addons/online/x/admin/lampac"
	if w := e.do("GET", p, "", mama); w.Code != 403 {
		t.Fatalf("non-admin: %d", w.Code)
	}
	w := e.do("PUT", p, `{"url":"http://l:1/","authKey":"uid","authValue":"s3cret","timeout":30}`, admin)
	if w.Code != 200 || strings.Contains(w.Body.String(), "s3cret") || !strings.Contains(w.Body.String(), `"authSet":true`) {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if f.saved.AuthValue == nil || *f.saved.AuthValue != "s3cret" || f.saved.Timeout != 30 {
		t.Fatalf("saved %+v", f.saved)
	}
	if w := e.do("POST", p+"/check", `{"url":"http://l:1/","authKey":"uid","authValue":"good"}`, admin); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"result":"ok","sources":3}` {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", p+"/check", `{"url":"http://l:1/","authKey":"uid","authValue":"bad"}`, admin); !strings.Contains(w.Body.String(), `"auth"`) {
		t.Fatalf("check auth: %s", w.Body)
	}
	f.refuse = true
	if w := e.do("PUT", p, `{"url":"x"}`, admin); w.Code != 422 || !strings.Contains(w.Body.String(), "Неверный источник") {
		t.Fatalf("refused: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", p+"/sources", "", admin); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"alpha"`) {
		t.Fatalf("sources: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", p+"/sources", `{"order":["b","a"],"off":["a"]}`, admin); w.Code != 204 || len(f.sources.Order) != 2 || f.sources.Off[0] != "a" {
		t.Fatalf("save sources: %d %+v", w.Code, f.sources)
	}
	f.noSvc = true
	if w := e.do("GET", p+"/sources", "", admin); w.Code != 503 || !strings.Contains(w.Body.String(), online.ErrUnavailable.Error()) {
		t.Fatalf("no Lampac: %d %s", w.Code, w.Body)
	}
	// without the settings part the routes are not there
	e = newEnv(t, nil)
	if w := e.do("GET", p, "", admin); w.Code != 404 {
		t.Fatalf("no Lampac part: %d", w.Code)
	}
}
