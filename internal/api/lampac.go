package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/antresol-app/antresol-addon-online/internal/boot"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// LampacSettings is «Настройки»'s Lampac part (*boot.Booted): the
// address and account, the waits, and the choice of sources.
type LampacSettings interface {
	LampacView(ctx context.Context) (boot.LampacView, error)
	SaveLampac(ctx context.Context, in boot.LampacSave) error
	CheckLampac(ctx context.Context, in boot.LampacSave) (boot.CheckResult, error)
	Sources(ctx context.Context) (boot.SourcesView, error)
	SaveSources(ctx context.Context, in boot.SourcesSave) error
}

// getLampac: GET /api/admin/lampac — the saved address (no query), where
// the one in force comes from, the account's key (never its value), the
// waits.
func (a *API) getLampac(w http.ResponseWriter, r *http.Request) {
	v, err := a.d.Lampac.LampacView(r.Context())
	if err != nil {
		internalErr(w, r, "lampac settings", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, v)
}

// putLampac: PUT /api/admin/lampac {url, authKey, authValue?, timeout,
// sourcesWait} — checked, saved and applied at once (the service is
// rebuilt on a new address); 422 with a message for a value refused.
func (a *API) putLampac(w http.ResponseWriter, r *http.Request) {
	var in boot.LampacSave
	if !decode(w, r, &in) {
		return
	}
	if err := a.d.Lampac.SaveLampac(r.Context(), in); err != nil {
		settingsErr(w, r, "lampac settings", err)
		return
	}
	a.getLampac(w, r)
}

// checkLampac: POST /api/admin/lampac/check {url, authKey, authValue?,
// timeout, sourcesWait} — a real request to Lampac with these values (not
// saved): {result: ok|auth|down|error, sources}; 422 for a value refused.
func (a *API) checkLampac(w http.ResponseWriter, r *http.Request) {
	var in boot.LampacSave
	if !decode(w, r, &in) {
		return
	}
	res, err := a.d.Lampac.CheckLampac(r.Context(), in)
	if err != nil {
		settingsErr(w, r, "lampac check", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, res)
}

// getSources: GET /api/admin/lampac/sources — Lampac's sources with the
// admin's choice; 503 when there is no Lampac or it does not answer.
func (a *API) getSources(w http.ResponseWriter, r *http.Request) {
	v, err := a.d.Lampac.Sources(r.Context())
	switch {
	case errors.Is(err, boot.ErrNoLampac):
		writeErr(w, r, http.StatusServiceUnavailable, online.ErrUnavailable.Error())
	case err != nil:
		onlineErr(w, r, err)
	default:
		addonkit.WriteJSON(w, http.StatusOK, v)
	}
}

// putSources: PUT /api/admin/lampac/sources {order, off} — saved and
// applied to the next list.
func (a *API) putSources(w http.ResponseWriter, r *http.Request) {
	var in boot.SourcesSave
	if !decode(w, r, &in) {
		return
	}
	if err := a.d.Lampac.SaveSources(r.Context(), in); err != nil {
		settingsErr(w, r, "lampac sources", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func settingsErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *boot.SettingsError
	if errors.As(err, &se) {
		writeErr(w, r, http.StatusUnprocessableEntity, se.Msg)
		return
	}
	internalErr(w, r, what, err)
}
