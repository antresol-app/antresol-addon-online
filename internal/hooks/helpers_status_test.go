package hooks

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// helpers.status answers what Deps.Helpers says; without it, 204.
func TestHelpersStatusHook(t *testing.T) {
	m := &addonkit.HookMux{}
	Register(m, Deps{Helpers: func(context.Context) addonkit.HelpersStatus {
		return addonkit.HelpersStatus{Helpers: []addonkit.HelperState{{Name: "lampac", Source: "settings", OK: true, Live: true}}}
	}})
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/helpers.status", strings.NewReader(`{}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"source":"settings"`) || !strings.Contains(w.Body.String(), `"live":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	m = &addonkit.HookMux{}
	Register(m, Deps{})
	w = httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/helpers.status", strings.NewReader(`{}`)))
	if w.Code != 204 {
		t.Fatalf("no helpers: %d", w.Code)
	}
}
