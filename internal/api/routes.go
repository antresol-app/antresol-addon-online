// Package api is the addon's HTTP routes for the app: «Смотреть онлайн»
// (the sources, a source's levels, play), the stream proxy, the online
// watch progress, whether the way is offered, and the admin's status and
// setting. The app reaches them through the core's proxy,
// /api/addons/online/x/<path>, which the addon gets as /api/<path> with
// the profile in X-Antresol-User (addonkit.UserFrom); the shapes are those
// of the in-core /api/online/* they replace.
//
// Every route refuses a kid profile, and a request without a profile,
// with 403 before it reads anything (the core never proxies kids; this is
// defence in depth). The rating limit is the header's maxRating, applied
// per title by online.Allowed. Everything but available and admin/* is
// 404 while «Смотреть онлайн» is off or has no Lampac.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// StreamBase is the client path of the stream proxy's sessions: the
// core's proxy prefix of the addon's /api/media/ (the contract's media
// prefix: the core lets a media key in there for GET and HEAD).
const StreamBase = online.StreamBase

// Messages of the routes (Russian; the catalogs translate them).
const (
	msgKid        = "Недоступно в детском профиле"
	msgBadRequest = "Неверный запрос"
	msgInternal   = "Внутренняя ошибка сервера"
)

// Deps are what the routes work on.
type Deps struct {
	// Online is «Смотреть онлайн» (nil without a usable Lampac URL: every
	// route but available and admin/* is 404). Its Proxy's Base must be
	// StreamBase.
	Online *online.Service
	// Progress is the online watch progress (nil: the progress routes are
	// 404), mirrored into the library through its Lib and Progress.
	Progress *online.ProgressStore
	// CacheImage puts a poster into the core's image cache (POST /images,
	// *addonkit.Client.CacheImage); nil: no posters.
	CacheImage func(ctx context.Context, url string) (string, error)
	// Enabled is the admin setting «Смотреть онлайн» (nil: on), and
	// SetEnabled saves it (nil: PUT admin/settings is 404).
	Enabled    func() bool
	SetEnabled func(ctx context.Context, on bool) error
	// Usable says Lampac can be asked: on the admin's address, or the
	// helper service answers on its compose name (nil: always).
	Usable func() bool
	// Search is the TMDB the app's title search asks (nil: the app has no
	// search from this addon).
	Search TitleSearch
	// Helper is the Lampac helper's line of the status (nil: left out).
	Helper func() addonkit.HelperStatus
	// Lampac is «Настройки»'s Lampac part (nil: its routes are 404).
	Lampac LampacSettings
}

// API is the mounted routes.
type API struct {
	d        Deps
	posters  *posters
	sessions *appSessions
}

// Register mounts the routes on mux (the addon's /api/… paths).
func Register(mux *http.ServeMux, d Deps) *API {
	a := &API{d: d, sessions: &appSessions{}}
	if d.Progress != nil && d.CacheImage != nil {
		a.posters = newPosters(d.CacheImage, d.Progress.SetPoster)
	}
	on := a.whenOn
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, adults(h)) }

	handle("GET /api/sources", on(a.sources))
	handle("GET /api/source", on(a.source))
	handle("GET /api/browse", on(a.browse))
	handle("POST /api/play", on(a.play))
	// GET answers HEAD too (AVPlayer and AirPlay ask HEAD first)
	handle("GET /api/media/{sid}/subs/{file}", on(a.subtitle))
	handle("GET /api/media/{sid}/{blob}", on(a.stream))
	// a session, never a media key: the core lets those in for GET/HEAD only
	handle("DELETE /api/media/{sid}", on(a.closeSession))
	handle("GET /api/progress", on(a.getProgress))
	handle("PUT /api/progress", on(a.saveProgress))
	handle("GET /api/available", a.available)
	// the app protocol (app.go, app_run.go): the way as data for the native apps
	handle("GET /api/app", on(a.appInfo))
	handle("GET /api/app/search", on(a.appSearch))
	handle("POST /api/app/step", on(a.appStep))
	handle("POST /api/app/run", on(a.appRun))
	handle("PUT /api/app/progress", on(a.appProgress))
	handle("DELETE /api/app/play/{session}", on(a.appClose))
	handle("GET /api/app/state", on(a.appState))

	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, adults(addonkit.RequireAdmin(h))) }
	admin("GET /api/admin/status", a.status)
	admin("GET /api/admin/settings", a.getSettings)
	admin("PUT /api/admin/settings", a.putSettings)
	if d.Lampac != nil {
		admin("GET /api/admin/lampac", a.getLampac)
		admin("PUT /api/admin/lampac", a.putLampac)
		admin("POST /api/admin/lampac/check", a.checkLampac)
		admin("GET /api/admin/lampac/sources", a.getSources)
		admin("PUT /api/admin/lampac/sources", a.putSources)
	}
	return a
}

// Wait waits for the poster fetches in flight (tests, shutdown).
func (a *API) Wait() {
	if a.posters != nil {
		a.posters.wg.Wait()
	}
}

// adults lets adult profiles through, with the profile in the context
// (online.WithUser) and its language for the messages; a kid, a missing
// or a broken header, and one naming no profile (null, {}, an id <= 0 or
// no uid) is 403.
func adults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := addonkit.UserFrom(r)
		if !ok || u.IsKid || u.ID <= 0 || u.UID == "" {
			writeErr(w, r, http.StatusForbidden, msgKid)
			return
		}
		ctx := online.WithUser(r.Context(), *u)
		if u.Lang != "" {
			ctx = i18n.WithLanguage(ctx, u.Lang)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// userOf is the request's profile (adults put it there).
func userOf(r *http.Request) online.User {
	u, _ := online.UserFrom(r.Context())
	return u
}

// on: Lampac is configured and the admin setting is on.
func (a *API) on() bool {
	return a.usable() && (a.d.Enabled == nil || a.d.Enabled())
}

// usable: there is a service and its Lampac can be asked.
func (a *API) usable() bool {
	return a.d.Online != nil && (a.d.Usable == nil || a.d.Usable())
}

// whenOn answers 404 while «Смотреть онлайн» is off or not configured.
func (a *API) whenOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.on() {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// available: GET /api/available — whether the online way is offered
// (on, and Lampac configured); the rating is per title.
func (a *API) available(w http.ResponseWriter, r *http.Request) {
	addonkit.WriteJSON(w, http.StatusOK, map[string]bool{"available": a.on()})
}

// writeErr writes {"error": msg} (Russian) in the request's language.
func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	addonkit.WriteError(w, code, i18n.Localize(r.Context(), msg))
}

// internalErr logs err and answers a generic 500.
func internalErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	log.Printf("online: %s %s: %s: %v", r.Method, r.URL.Path, what, err)
	writeErr(w, r, http.StatusInternalServerError, msgInternal)
}

// decode reads the JSON body into v; a bad one is 400.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return false
	}
	return true
}

// onlineErr answers an error of the online service.
func onlineErr(w http.ResponseWriter, r *http.Request, err error) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{online.ErrKid, http.StatusForbidden},
		{online.ErrRating, http.StatusForbidden},
		{online.ErrBadRef, http.StatusBadRequest},
		{online.ErrUnknownTitle, http.StatusBadRequest},
		{online.ErrNoProfile, http.StatusBadRequest},
		{online.ErrTitleUnavailable, http.StatusServiceUnavailable},
		{online.ErrAuth, http.StatusBadGateway},
		{online.ErrUnavailable, http.StatusServiceUnavailable},
		{online.ErrSourceFailed, http.StatusBadGateway},
		{online.ErrUnsupported, http.StatusUnsupportedMediaType},
		{online.ErrBusy, http.StatusTooManyRequests},
		{online.ErrNothing, http.StatusNotFound},
	} {
		if errors.Is(err, c.err) {
			writeErr(w, r, c.code, c.err.Error())
			return
		}
	}
	switch {
	case errors.Is(err, online.ErrOff):
		http.NotFound(w, r)
	case errors.Is(err, context.Canceled):
		// the client went away
	default:
		internalErr(w, r, "online", err)
	}
}
