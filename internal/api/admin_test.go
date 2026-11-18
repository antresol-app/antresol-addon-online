package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// «Смотреть онлайн» on and off (the admin setting, kept in the addon's own
// database): GET and PUT {enabled}, admins only; a save takes effect at
// once on every route (no cached read).
func TestAdminSettings_Toggle(t *testing.T) {
	e := newEnv(t, fakeOnline(t, false))
	const settings = "/api/addons/online/x/admin/settings"
	if w := e.do("GET", settings, "", admin); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":true}` {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", settings, `{"enabled":false}`, admin); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":false}` {
		t.Fatalf("put off: %d %s", w.Code, w.Body)
	}
	if e.enabled.Load() {
		t.Fatal("not saved")
	}
	if w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", "", admin); w.Code != 404 {
		t.Fatalf("sources while off: %d", w.Code)
	}
	if w := e.do("GET", "/api/addons/online/x/available", "", mama); !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("available while off: %s", w.Body)
	}
	if w := e.do("GET", settings, "", admin); strings.TrimSpace(w.Body.String()) != `{"enabled":false}` {
		t.Fatalf("get off: %s", w.Body)
	}
	// a body without the field, or not JSON, changes nothing
	for _, bad := range []string{`{}`, `{"enabled":"yes"}`, `{"enabled":`, `{"enabled":null}`} {
		if w := e.do("PUT", settings, bad, admin); w.Code != 400 {
			t.Fatalf("put %s: %d %s", bad, w.Code, w.Body)
		}
	}
	// adults who are not admins: neither read nor write
	for _, m := range []string{"GET", "PUT"} {
		if w := e.do(m, settings, `{"enabled":true}`, mama); w.Code != 403 {
			t.Fatalf("%s by a non-admin: %d", m, w.Code)
		}
	}
	if e.enabled.Load() {
		t.Fatal("a refused put saved")
	}
	if w := e.do("PUT", settings, `{"enabled":true}`, admin); w.Code != 200 {
		t.Fatalf("put on: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", "", admin); w.Code != 200 {
		t.Fatalf("sources on again: %d %s", w.Code, w.Body)
	}
	// without Lampac the setting is kept all the same (available stays false)
	e = newEnv(t, nil)
	if w := e.do("PUT", settings, `{"enabled":false}`, admin); w.Code != 200 || e.enabled.Load() {
		t.Fatalf("no Lampac put: %d %s", w.Code, w.Body)
	}
	// a failed save answers 500 and keeps the state
	e = newEnv(t, fakeOnline(t, false), func(d *Deps) {
		d.SetEnabled = func(context.Context, bool) error { return errors.New("disk full") }
	})
	if w := e.do("PUT", settings, `{"enabled":false}`, admin); w.Code != 500 || strings.Contains(w.Body.String(), "disk full") {
		t.Fatalf("failed save: %d %s", w.Code, w.Body)
	}
	if !e.enabled.Load() {
		t.Fatal("a failed save changed the state")
	}
}
