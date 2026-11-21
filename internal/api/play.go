package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// titleOf is the kind (movie | series) and TMDB id of the query; false
// (400 written) when they are not one.
func titleOf(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	kind := r.URL.Query().Get("kind")
	id, err := strconv.Atoi(r.URL.Query().Get("tmdbId"))
	if (kind != "movie" && kind != "series") || err != nil || id <= 0 {
		writeErr(w, r, http.StatusBadRequest, online.ErrUnknownTitle.Error())
		return "", 0, false
	}
	return kind, id, true
}

// sources: GET /api/sources?kind=movie|series&tmdbId=N.
func (a *API) sources(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := titleOf(w, r)
	if !ok {
		return
	}
	src, err := a.d.Online.Sources(r.Context(), userOf(r), kind, id)
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"sources": src})
}

// source: GET /api/source?kind=movie|series&tmdbId=N&source=ID — one
// remembered source of a title, without the whole list.
func (a *API) source(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := titleOf(w, r)
	if !ok {
		return
	}
	src, err := a.d.Online.Source(r.Context(), userOf(r), kind, id, r.URL.Query().Get("source"))
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"source": src})
}

// browse: GET /api/browse?ref=R — one level of a source.
func (a *API) browse(w http.ResponseWriter, r *http.Request) {
	lv, err := a.d.Online.Browse(r.Context(), userOf(r), r.URL.Query().Get("ref"))
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, lv)
}

// playRequest is the body of POST /api/play.
type playRequest struct {
	Ref      string  `json:"ref"`
	Quality  string  `json:"quality"` // a source quality ("1080p"); "": the default
	StartSec float64 `json:"startSec"`
	// Caps are the player's; only maxHeight is read: the cap of «Авто»
	// (0: none). The core's per-profile cap does not reach the addon.
	Caps struct {
		MaxHeight int `json:"maxHeight"`
	} `json:"caps"`
	// ReplaceSession is the session this one replaces (another quality,
	// a re-play after a stale link).
	ReplaceSession string `json:"replaceSession"`
	// Refresh: the player's re-play after a stale link (410): everything
	// is resolved fresh, past the caches.
	Refresh bool `json:"refresh"`
}

// subTrack is a subtitle track of the play answer, in the shape of the
// core's /api/play tracks (the players read title, external, url), with
// the source's label kept as given.
type subTrack struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Lang     string `json:"lang"`
	Title    string `json:"title"`
	Forced   bool   `json:"forced"`
	Default  bool   `json:"default"`
	External bool   `json:"external"`
	BurnIn   bool   `json:"burnIn"`
	URL      string `json:"url,omitempty"`
	Label    string `json:"label"`
}

// play: POST /api/play — resolves a file ref and opens a proxy session.
// The answer has the shape of the core's /api/play (mode "online"); its
// url and subtitle URLs are under StreamBase. A POST carries no media key:
// the app appends ?mk= to those URLs itself, and the playlists fetched
// with one carry it on.
func (a *API) play(w http.ResponseWriter, r *http.Request) {
	var in playRequest
	if a.d.Online.Proxy == nil {
		http.NotFound(w, r)
		return
	}
	if !decode(w, r, &in) {
		return
	}
	st, err := a.startPlay(r.Context(), userOf(r), in)
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, st.body)
}

// started is a play that opened a proxy session: the answer for the
// player, the session's id and what the ref resolved to.
type started struct {
	body map[string]any
	sid  string
	res  online.Resolved
}

// startPlay resolves in.Ref for u and opens a session for the stream
// (the answer of play and of the app's run).
func (a *API) startPlay(ctx context.Context, u online.User, in playRequest) (started, error) {
	svc := a.d.Online
	if svc.Proxy == nil {
		return started{}, online.ErrOff
	}
	// the server's cap first: resolving is seconds of upstream work
	if err := svc.Proxy.CanOpen(u.ID, in.ReplaceSession); err != nil {
		return started{}, err
	}
	if h := capHeight(in.Caps.MaxHeight, u.MaxHeight); h > 0 {
		ctx = online.WithMaxHeight(ctx, h)
	}
	began := time.Now()
	res, err := svc.Resolve(ctx, u, in.Ref, in.Quality, in.Refresh)
	if err != nil {
		return started{}, err
	}
	res.Took = time.Since(began)
	var warmSubs func()
	res.Subtitles, warmSubs = svc.PlaySubtitles(ctx, res)
	sid, err := svc.Proxy.Open(u.ID, res, in.ReplaceSession)
	if err != nil {
		return started{}, err // ErrBusy: no subtitle work for a play that is not
	}
	warmSubs()
	subs := make([]subTrack, 0, len(res.Subtitles))
	for i, s := range res.Subtitles {
		subs = append(subs, subTrack{Index: i, Codec: "webvtt", Lang: s.Lang, Title: s.Label, Label: s.Label,
			External: true, URL: svc.Proxy.SubURL(sid, i, "")})
	}
	options := res.Qualities
	if options == nil {
		options = []string{}
	}
	return started{sid: sid, res: res, body: map[string]any{
		"mode": "online", "kind": res.Kind, "url": svc.Proxy.URL(sid, res.URL, ""), "startSec": max(in.StartSec, 0),
		"durationMs": 0, "audio": []any{}, "audioIndex": -1, "subtitles": subs,
		"quality": map[string]any{"options": options, "current": res.Quality}, "session": sid,
	}}, nil
}

// capHeight is the cap of «Авто»: the lower of the request's caps and the
// user header's remote cap (the core's «Качество вне дома» away from
// home), where set; 0: none.
func capHeight(caps, remote int) int {
	switch {
	case caps <= 0:
		return max(remote, 0)
	case remote <= 0:
		return caps
	}
	return min(caps, remote)
}

// keyed is r's context with the media key of its query, if any: the core
// lets a media key through on its media routes and hands the query on, so
// the URIs of a playlist fetched with one carry it on (the core checked
// it; the addon only echoes it to the same client).
func keyed(r *http.Request) *http.Request {
	if mk := r.URL.Query().Get("mk"); mk != "" {
		return r.WithContext(online.WithMediaKey(r.Context(), mk))
	}
	return r
}

// stream: GET (HEAD) /api/media/{sid}/{blob} — the proxy. Its answers (a 410
// for a stale link, 404 for a session gone, 416, 206) go out unchanged.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	if a.d.Online.Proxy == nil {
		writeErr(w, r, http.StatusNotFound, online.ErrNoSession.Error())
		return
	}
	a.d.Online.Proxy.Serve(w, keyed(r), r.PathValue("sid"), r.PathValue("blob"))
}

// subtitle: GET (HEAD) /api/media/{sid}/subs/{n}.vtt.
func (a *API) subtitle(w http.ResponseWriter, r *http.Request) {
	name, isVTT := strings.CutSuffix(r.PathValue("file"), ".vtt")
	n, err := strconv.Atoi(name)
	if a.d.Online.Proxy == nil || !isVTT || err != nil || n < 0 || strconv.Itoa(n) != name {
		writeErr(w, r, http.StatusNotFound, online.ErrNoSession.Error())
		return
	}
	a.d.Online.Proxy.ServeSub(w, r, r.PathValue("sid"), n)
}

// closeSession: DELETE /api/media/{sid} — the player's close (204); 404 for a
// session that is not the profile's.
func (a *API) closeSession(w http.ResponseWriter, r *http.Request) {
	if a.d.Online.Proxy == nil || !a.d.Online.Proxy.Close(userOf(r).ID, r.PathValue("sid")) {
		writeErr(w, r, http.StatusNotFound, online.ErrNoSession.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
