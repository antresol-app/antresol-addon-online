package main

import (
	"net/http"
	"sync/atomic"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
	"github.com/antresol-app/antresol/addonkit"
)

// newHandler serves /health and the hooks. The hooks are registered on the
// returned HookMux once the services are built (until then they answer 404:
// the core fails the operation closed).
func newHandler(env addonkit.Env, core *addonkit.Client, ready *addonkit.Ready) (http.Handler, *addonkit.HookMux) {
	mux := http.NewServeMux()
	// the core's health check: {"ok": true, "ready": …}
	mux.HandleFunc("GET /health", ready.Health)
	// the hooks the core calls (POST /hooks/<name>): exactly those the
	// manifest declares in "hooks"
	hooks := &addonkit.HookMux{}
	mux.Handle("POST /hooks/", hooks)
	return mux, hooks
}

// msgStarting: the app's routes before the services are built;
// msgNotReady: after building them failed (until the next start).
const (
	msgStarting = "Расширение запускается"
	msgNotReady = "Расширение не готово"
)

// lateHandler is the app's routes (/api/…), set once the services are
// built; until then 503 «Расширение запускается» with Retry-After, and
// after a failed build 503 «Расширение не готово» without one.
type lateHandler struct {
	h      atomic.Pointer[http.Handler]
	failed atomic.Bool
}

func (l *lateHandler) set(h http.Handler) { l.h.Store(&h) }

// fail: the services could not be built; the routes stay unset.
func (l *lateHandler) fail() { l.failed.Store(true) }

func (l *lateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	lang := i18n.RU
	if u, ok := addonkit.UserFrom(r); ok && u.Lang != "" {
		lang = u.Lang
	}
	if l.failed.Load() {
		addonkit.WriteError(w, http.StatusServiceUnavailable, i18n.TL(lang, msgNotReady))
		return
	}
	w.Header().Set("Retry-After", "5")
	addonkit.WriteError(w, http.StatusServiceUnavailable, i18n.TL(lang, msgStarting))
}
