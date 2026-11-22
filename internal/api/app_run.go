package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// --- the plays' memory ---

// appSession is what a play of the app remembers beside the proxy's
// session: the title, the source and voice it plays in and the episode the
// source lists after it, so the position the app sends (which names only the
// session) is recorded as the web player records it.
type appSession struct {
	user                    int64
	kind                    string
	tmdbID, season, episode int
	source, voice, quality  string
	nextSeason, nextEpisode int
	last                    bool
	at                      time.Time
}

const (
	maxAppSessions = 2000
	appSessionLife = 12 * time.Hour
)

// appSessions keeps them for a while (a play lasts hours at most; a
// restart forgets them, and the position then keeps the entry's own choices).
type appSessions struct {
	mu  sync.Mutex
	m   map[string]appSession
	now func() time.Time
}

func (s *appSessions) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *appSessions) put(id string, v appSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if s.m == nil {
		s.m = map[string]appSession{}
	}
	if len(s.m) >= maxAppSessions {
		for k, e := range s.m {
			if now.Sub(e.at) > appSessionLife {
				delete(s.m, k)
			}
		}
		if len(s.m) >= maxAppSessions { // all alive: the oldest goes
			oldest, at := "", now
			for k, e := range s.m {
				if e.at.Before(at) || oldest == "" {
					oldest, at = k, e.at
				}
			}
			delete(s.m, oldest)
		}
	}
	v.at = now
	s.m[id] = v
}

// get is user's session id (never another profile's).
func (s *appSessions) get(id string, user int64) (appSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok || v.user != user || s.clock().Sub(v.at) > appSessionLife {
		return appSession{}, false
	}
	return v, true
}

// drop forgets user's session id.
func (s *appSessions) drop(id string, user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[id]; ok && v.user == user {
		delete(s.m, id)
	}
}

// --- the run ---

// appRun: POST /api/app/run — plays the file a final row's ref names (in the
// call's `quality` when it names one); with «quick» and no ref it goes on from
// the last choices of this title (source, voice, season and episode, quality)
// or says {"pick": true}, and the app opens the sheet. The play answer carries
// `ref`, which plays the same stream again.
func (a *API) appRun(w http.ResponseWriter, r *http.Request) {
	in, ref, ok := a.readCall(w, r)
	if !ok {
		return
	}
	ctx, u := r.Context(), userOf(r)
	switch {
	case ref != nil && ref.K == refPlay:
	case ref != nil: // a step's row is not a play
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return
	case in.Quick:
		quick, pick, err := a.quickRef(ctx, u, in)
		if err != nil {
			appErr(w, r, err)
			return
		}
		if pick {
			addonkit.WriteJSON(w, http.StatusOK, map[string]bool{"pick": true})
			return
		}
		ref = &quick
	default:
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return
	}
	svc := a.d.Online
	_, rc, err := svc.Refs.Open(ref.R)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, online.ErrBadRef.Error())
		return
	}
	req := playRequest{Ref: ref.R, Quality: ref.Q, Refresh: in.Refresh}
	if in.Quality != "" {
		req.Quality = in.Quality
	}
	req.Caps.MaxHeight = in.MaxHeight
	if in.StartSec != nil {
		req.StartSec = max(*in.StartSec, 0)
	} else {
		req.StartSec = a.memory(ctx, u, rc.Kind, rc.TMDBID).resumeSec(rc.Season, rc.Episode)
	}
	st, err := a.startPlay(ctx, u, req)
	if err != nil {
		appErr(w, r, err)
		return
	}
	c := st.res.Ctx
	a.sessions.put(st.sid, appSession{user: u.ID, kind: c.Kind, tmdbID: c.TMDBID, season: c.Season, episode: c.Episode,
		source: c.Source, voice: c.Voice, quality: st.res.Quality, nextSeason: ref.NS, nextEpisode: ref.NE, last: ref.L})
	st.body["progress"] = map[string]any{"path": "x/app/progress", "every": progressEvery}
	// the ref that plays this very stream again (a stale link, another quality
	// with `quality`): the file, the quality it plays in and the episode after it
	again := appRef{K: refPlay, R: ref.R, Q: ref.Q, NS: ref.NS, NE: ref.NE, L: ref.L}
	if q := st.res.Quality; q != "" && len(q) <= maxQualityName {
		again.Q = q
	}
	st.body["ref"] = again.encode()
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"play": st.body})
}

// softErr: the remembered choice does not hold any more (the source has
// nothing, or does not answer): the person picks again.
func softErr(err error) bool {
	return errors.Is(err, online.ErrNothing) || errors.Is(err, online.ErrSourceFailed)
}

// quickRef is the file the last choices of the title lead to (pick: they
// lead nowhere any more, or there are none). A call that names the season
// and episode plays that episode in the remembered source and voice; else a
// series goes on at the remembered episode, or the next one when that was
// watched to the end.
func (a *API) quickRef(ctx context.Context, u online.User, in appCall) (ref appRef, pick bool, err error) {
	svc := a.d.Online
	mem := a.memory(ctx, u, in.Kind, in.TMDBID)
	last := mem.last
	if last.Source == "" {
		return ref, true, nil
	}
	series := in.Kind == "series"
	season, episode := in.Season, in.Episode
	explicit := series && season > 0 && episode > 0
	if series && !explicit {
		if season, episode = last.Season, last.Episode; season <= 0 || episode <= 0 {
			return ref, true, nil
		}
	}
	src, err := svc.Source(ctx, u, in.Kind, in.TMDBID, last.Source)
	if err != nil {
		return ref, softErr(err), nilIf(err, softErr)
	}
	lv, err := svc.Browse(ctx, u, src.Ref)
	if err != nil {
		return ref, softErr(err), nilIf(err, softErr)
	}
	switched := false
	for range maxHops {
		if lv.Level == "pick" {
			return ref, true, nil // the person picks the match
		}
		if !switched && last.Voice != "" {
			if v, ok := voiceOf(lv, last.Voice); ok {
				switched = true
				if !v.Active {
					if lv, err = svc.Browse(ctx, u, v.Ref); err != nil {
						return ref, softErr(err), nilIf(err, softErr)
					}
					continue
				}
			}
		}
		if lv.Level == "seasons" {
			s, ok := seasonOf(lv, season)
			if !ok {
				return ref, true, nil
			}
			if lv, err = svc.Browse(ctx, u, s.Ref); err != nil {
				return ref, softErr(err), nilIf(err, softErr)
			}
			continue
		}
		break
	}
	switch lv.Level {
	case "episodes":
		idx := -1
		for i, e := range lv.Episodes {
			if e.Episode == episode && (season == 0 || e.Season == season) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return ref, true, nil
		}
		if en, ok := mem.entry(lv.Episodes[idx].Season, lv.Episodes[idx].Episode); !explicit && ok && en.Finished && idx+1 < len(lv.Episodes) {
			idx++ // watched to the end: the next one
		}
		e := lv.Episodes[idx]
		if e.Unsupported {
			return ref, true, nil
		}
		ref = appRef{K: refPlay, R: e.Ref, Q: askedQuality(last.Quality, e.Qualities)}
		if idx+1 < len(lv.Episodes) {
			ref.NS, ref.NE = lv.Episodes[idx+1].Season, lv.Episodes[idx+1].Episode
		} else {
			ref.L = true
		}
		return ref, false, nil
	case "movie":
		idx := -1
		for i, f := range lv.Files {
			if f.Unsupported {
				continue
			}
			if last.Voice != "" && fileVoice(f) == last.Voice {
				idx = i
				break
			}
			if idx < 0 {
				idx = i
			}
		}
		if idx < 0 {
			return ref, true, nil
		}
		f := lv.Files[idx]
		return appRef{K: refPlay, R: f.Ref, Q: askedQuality(last.Quality, f.Qualities)}, false, nil
	}
	return ref, true, nil
}

// nilIf is err unless soft says it is only a reason to pick again.
func nilIf(err error, soft func(error) bool) error {
	if soft(err) {
		return nil
	}
	return err
}

func voiceOf(lv online.Level, name string) (online.Choice, bool) {
	for _, v := range lv.Voices {
		if v.Name == name {
			return v, true
		}
	}
	return online.Choice{}, false
}

// askedQuality is the remembered quality when the file has it ("": the
// server's default).
func askedQuality(q string, have []string) string {
	for _, h := range have {
		if h == q {
			return q
		}
	}
	return ""
}

// --- the progress and the close ---

// appProgressRequest is the body of PUT /api/app/progress.
type appProgressRequest struct {
	Session    string `json:"session"`
	Way        string `json:"way"`
	Kind       string `json:"kind"`
	TMDBID     int    `json:"tmdbId"`
	Season     int    `json:"season"`
	Episode    int    `json:"episode"`
	PositionMs int64  `json:"positionMs"`
	DurationMs int64  `json:"durationMs"`
	Finished   bool   `json:"finished"`
}

// appProgress: PUT /api/app/progress — the position of a play's session
// (204), recorded as the web player's: with the source, voice and quality
// the session plays in, and the episode after it. A session the addon does
// not know (it restarted) keeps the entry's own choices.
func (a *API) appProgress(w http.ResponseWriter, r *http.Request) {
	if a.d.Progress == nil {
		http.NotFound(w, r)
		return
	}
	var in appProgressRequest
	if !decode(w, r, &in) {
		return
	}
	if in.Way != wayWatch {
		writeErr(w, r, http.StatusNotFound, msgNotFound)
		return
	}
	ctx, u := r.Context(), userOf(r)
	pr := progressRequest{Kind: in.Kind, TMDBID: in.TMDBID, Season: in.Season, Episode: in.Episode,
		PositionMs: in.PositionMs, DurationMs: in.DurationMs}
	if s, ok := a.sessions.get(in.Session, u.ID); ok {
		pr.Kind, pr.TMDBID, pr.Season, pr.Episode = s.kind, s.tmdbID, s.season, s.episode
		pr.Source, pr.Voice, pr.Quality = s.source, s.voice, s.quality
		pr.NextSeason, pr.NextEpisode, pr.LastListed = s.nextSeason, s.nextEpisode, s.last
	} else {
		if (in.Kind != "movie" && in.Kind != "series") || in.TMDBID <= 0 || in.Season < 0 || in.Episode < 0 {
			writeErr(w, r, http.StatusBadRequest, online.ErrUnknownTitle.Error())
			return
		}
		if e, ok := a.memory(ctx, u, in.Kind, in.TMDBID).entry(in.Season, in.Episode); ok {
			pr.Source, pr.Voice, pr.Quality = e.Source, e.Voice, e.Quality
		}
	}
	if in.Finished {
		// watched to the end: the position reaches the duration (which the
		// player may not know: it is then the position)
		if pr.DurationMs <= 0 {
			pr.DurationMs = pr.PositionMs
		}
		pr.PositionMs = max(pr.PositionMs, pr.DurationMs)
	}
	if err := a.saveEntry(ctx, u, pr); err != nil {
		onlineErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appClose: DELETE /api/app/play/{session} — the player's close (204); 404
// for a session that is not the profile's.
func (a *API) appClose(w http.ResponseWriter, r *http.Request) {
	sid, u := r.PathValue("session"), userOf(r)
	a.sessions.drop(sid, u.ID)
	if a.d.Online.Proxy == nil || !a.d.Online.Proxy.Close(u.ID, sid) {
		writeErr(w, r, http.StatusNotFound, online.ErrNoSession.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// appState: GET /api/app/state — the ways' progress; this addon's way is a
// play, which has none.
func (a *API) appState(w http.ResponseWriter, r *http.Request) {
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"ways": map[string]any{}})
}
