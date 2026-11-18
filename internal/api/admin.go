package api

import (
	"net/http"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// status: GET /api/admin/status — whether Lampac is configured and
// answers, whatever the setting, so «Настройки» can tell «off» from «not
// configured».
func (a *API) status(w http.ResponseWriter, r *http.Request) {
	if a.d.Online == nil {
		addonkit.WriteJSON(w, http.StatusOK, online.StatusDTO{})
		return
	}
	st, err := a.d.Online.Status(r.Context())
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, st)
}

// settingsDTO is the admin setting «Смотреть онлайн».
type settingsDTO struct {
	Enabled *bool `json:"enabled"`
}

func (a *API) enabled() bool { return a.d.Enabled == nil || a.d.Enabled() }

// getSettings: GET /api/admin/settings — {enabled}.
func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	on := a.enabled()
	addonkit.WriteJSON(w, http.StatusOK, settingsDTO{Enabled: &on})
}

// putSettings: PUT /api/admin/settings {enabled} — saved in the addon's
// database and applied at once; the answer is the setting as saved. A
// body without a boolean "enabled" is 400 and changes nothing.
func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	if a.d.SetEnabled == nil {
		http.NotFound(w, r)
		return
	}
	var in settingsDTO
	if !decode(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return
	}
	if err := a.d.SetEnabled(r.Context(), *in.Enabled); err != nil {
		internalErr(w, r, "settings", err)
		return
	}
	a.getSettings(w, r)
}
