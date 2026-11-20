package api

// The app protocol (the core's apps reach it as /api/addons/online/x/app…):
// what the addon's web part shows on a title page — the «Онлайн» way of
// «Смотреть ▾» with its source, voice, season, episode and quality choice —
// described as data, so a native app draws the same sheet without knowing
// this addon. The routes are thin wrappers: the sources, the levels, the
// play and the progress are those of the routes above.
//
//	GET    /api/app                  the way and the search this addon offers (404: off)
//	GET    /api/app/search?q=        titles to watch ({titles})
//	POST   /api/app/step             one step of the choice ({title, rows, note, empty})
//	POST   /api/app/run              play: {play}, or {pick: true} when a choice is needed
//	PUT    /api/app/progress         the position of a play's session (204)
//	DELETE /api/app/play/{session}   the player's close (204)
//	GET    /api/app/state            the ways' progress ({ways}): none of this addon's
//
// A row's ref is opaque to the app: it carries the signed ref of the level
// or file it leads to, so every call stands alone. The labels are in the
// profile's language.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-online/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// appVersion is the protocol's version (AppInfo.v).
const appVersion = 1

// wayWatch is the way's id: opaque to the app, which only hands it back.
const wayWatch = "watch"

// progressEvery is how often (seconds) the app sends the position.
const progressEvery = 10

// TitleSearch is the TMDB the app's search asks (*tmdb.Client).
type TitleSearch interface {
	SearchMovie(ctx context.Context, q string, year int) ([]tmdb.Result, error)
	SearchTV(ctx context.Context, q string, year int) ([]tmdb.Result, error)
	ImageURL(size, p string) string
}

// Messages of the app routes (Russian; the catalogs translate them).
const (
	msgNotFound     = "Не найдено"
	msgNoSources    = "Онлайн-источники ничего не нашли"
	msgSourceEmpty  = "Ничего не нашлось в этом источнике"
	msgPickMatch    = "Выберите подходящий вариант"
	msgDefaultAudio = "Озвучка по умолчанию"
)

func tr(r *http.Request, key string, args ...any) string { return i18n.T(r.Context(), key, args...) }

// --- the description ---

type appWayInfo struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Icon     string   `json:"icon"`
	Role     string   `json:"role"`
	Kinds    []string `json:"kinds"`
	Scope    string   `json:"scope"`
	Episodes bool     `json:"episodes"`
}

type appSearchInfo struct {
	Label string `json:"label"`
}

type appInfo struct {
	V      int            `json:"v"`
	Ways   []appWayInfo   `json:"ways"`
	Search *appSearchInfo `json:"search,omitempty"`
}

// appInfo: GET /api/app. The route is 404 while «Смотреть онлайн» is off or
// not configured (whenOn), so an app shows nothing then.
func (a *API) appInfo(w http.ResponseWriter, r *http.Request) {
	out := appInfo{V: appVersion, Ways: []appWayInfo{{ID: wayWatch, Label: tr(r, "Онлайн"), Icon: "globe", Role: "watch",
		Kinds: []string{"movie", "series"}, Scope: "any", Episodes: true}}}
	if a.d.Search != nil {
		out.Search = &appSearchInfo{Label: tr(r, "Найти и смотреть")}
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}

// --- the search ---

type appTitle struct {
	Kind          string `json:"kind"`
	TMDBID        int    `json:"tmdbId"`
	Title         string `json:"title"`
	Year          int    `json:"year,omitempty"`
	Poster        string `json:"poster,omitempty"`
	Overview      string `json:"overview,omitempty"`
	InLibrary     bool   `json:"inLibrary"`
	LibraryItemID int64  `json:"libraryItemId,omitempty"`
}

// maxSearchTitles bounds the titles one search lists.
const maxSearchTitles = 20

// appSearch: GET /api/app/search?q= — the movies and series TMDB knows by
// that name, in the profile's language, movies and shows alternating (TMDB
// ranks each list by relevance).
func (a *API) appSearch(w http.ResponseWriter, r *http.Request) {
	if a.d.Search == nil {
		http.NotFound(w, r)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := []appTitle{}
	if q == "" {
		addonkit.WriteJSON(w, http.StatusOK, map[string]any{"titles": out})
		return
	}
	ctx := tmdb.WithLanguage(r.Context(), i18n.TMDB(i18n.FromContext(r.Context())))
	var movies, shows []tmdb.Result
	var merr, terr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); movies, merr = a.d.Search.SearchMovie(ctx, q, 0) }()
	go func() { defer wg.Done(); shows, terr = a.d.Search.SearchTV(ctx, q, 0) }()
	wg.Wait()
	if merr != nil && terr != nil {
		if r.Context().Err() == nil {
			internalErr(w, r, "search", merr)
		}
		return
	}
	add := func(kind string, res tmdb.Result) {
		if res.Adult || res.ID <= 0 {
			return // adult content: never offered
		}
		t := appTitle{Kind: kind, TMDBID: res.ID, Title: res.Title, Year: res.Year, Overview: res.Overview}
		if res.Poster != "" {
			t.Poster = a.d.Search.ImageURL("w342", res.Poster)
		}
		if lib := a.library(); lib != nil {
			if it, ok, err := lib.ItemByTMDB(ctx, kind, int64(res.ID)); err == nil && ok {
				t.InLibrary, t.LibraryItemID = true, it.ID
			}
		}
		out = append(out, t)
	}
	for i := 0; i < maxSearchTitles && (i < len(movies) || i < len(shows)); i++ {
		if i < len(movies) {
			add("movie", movies[i])
		}
		if i < len(shows) {
			add("series", shows[i])
		}
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"titles": out})
}

// library is the core's library the search asks whether a title is in it
// (nil: not known).
func (a *API) library() online.LibraryLookup {
	if a.d.Progress == nil || a.d.Progress.Lib == nil {
		return nil
	}
	return a.d.Progress.Lib
}

// --- the refs ---

// What a row's ref leads to.
const (
	refBrowse = "b" // a level of a source (the source, a voice, a season, a candidate): the next step browses it
	refFile   = "f" // a file or episode with several qualities: the next step asks the quality
	refPlay   = "p" // a file or episode (and quality) to play: the run's
)

// appRef is a row's ref. R is the signed ref of online (the level or the
// file); the rest is what a play remembers (the quality asked, the episode
// the source lists after this one).
type appRef struct {
	K  string   `json:"k"`
	R  string   `json:"r"`
	Q  string   `json:"q,omitempty"`
	Qs []string `json:"qs,omitempty"` // the file's qualities (refFile)
	NS int      `json:"ns,omitempty"`
	NE int      `json:"ne,omitempty"`
	L  bool     `json:"l,omitempty"` // no episode is listed after this one
}

// maxAppRef bounds a ref (online's own are at most 8 KiB).
const maxAppRef = 16 << 10

func (a appRef) encode() string {
	b, _ := json.Marshal(a)
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseRef reads a ref; nil for none. ok false: it is not one of ours.
func parseRef(s string) (*appRef, bool) {
	if s == "" {
		return nil, true
	}
	if len(s) > maxAppRef {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	var ref appRef
	if json.Unmarshal(b, &ref) != nil || ref.R == "" || len(ref.Q) > 100 || len(ref.Qs) > 20 || ref.NS < 0 || ref.NE < 0 {
		return nil, false
	}
	switch ref.K {
	case refBrowse, refFile, refPlay:
	default:
		return nil, false
	}
	for _, q := range ref.Qs {
		if len(q) > 100 {
			return nil, false
		}
	}
	return &ref, true
}

// --- the request ---

// appCall is the body of step and run.
type appCall struct {
	Way       string   `json:"way"`
	Kind      string   `json:"kind"`
	TMDBID    int      `json:"tmdbId"`
	Season    int      `json:"season"`
	Episode   int      `json:"episode"`
	Ref       string   `json:"ref"`
	StartSec  *float64 `json:"startSec"`
	MaxHeight int      `json:"maxHeight"`
	Quick     bool     `json:"quick"`
	Refresh   bool     `json:"refresh"`
}

// readCall reads and checks a step or run body; false: the answer is
// written.
func (a *API) readCall(w http.ResponseWriter, r *http.Request) (appCall, *appRef, bool) {
	var in appCall
	if !decode(w, r, &in) {
		return in, nil, false
	}
	if in.Way != wayWatch {
		writeErr(w, r, http.StatusNotFound, msgNotFound) // the way is gone: the app asks again what there is
		return in, nil, false
	}
	if (in.Kind != "movie" && in.Kind != "series") || in.TMDBID <= 0 || in.Season < 0 || in.Episode < 0 {
		writeErr(w, r, http.StatusBadRequest, online.ErrUnknownTitle.Error())
		return in, nil, false
	}
	if in.Kind == "movie" {
		in.Season, in.Episode = 0, 0
	}
	ref, ok := parseRef(in.Ref)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return in, nil, false
	}
	return in, ref, true
}

// appErr answers an error of the service. A source with nothing in it is
// not a 404 here (that would say the way is gone).
func appErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, online.ErrNothing) {
		writeErr(w, r, http.StatusUnprocessableEntity, msgSourceEmpty)
		return
	}
	onlineErr(w, r, err)
}

// memory is what the profile watched of a title: its entries and last
// choices, for the rows' marks and the quick play. A failed read only loses
// them.
type memory struct {
	entries []online.Entry
	last    online.Last
}

func (a *API) memory(ctx context.Context, u online.User, kind string, id int) memory {
	if a.d.Progress == nil {
		return memory{}
	}
	entries, last, err := a.d.Progress.Get(ctx, u.HistoryUser(), kind, id)
	if err != nil {
		return memory{}
	}
	return memory{entries: entries, last: last}
}

func (m memory) entry(season, episode int) (online.Entry, bool) {
	for _, e := range m.entries {
		if e.Season == season && e.Episode == episode {
			return e, true
		}
	}
	return online.Entry{}, false
}

// resumeSec is where an episode (a movie: 0, 0) starts: its saved
// position, else the start.
func (m memory) resumeSec(season, episode int) float64 {
	if e, ok := m.entry(season, episode); ok && !e.Finished {
		return float64(e.PositionMs) / 1000
	}
	return 0
}

// --- the steps ---

type appRow struct {
	Ref     string   `json:"ref"`
	Label   string   `json:"label"`
	Detail  string   `json:"detail,omitempty"`
	Badges  []string `json:"badges,omitempty"`
	Current bool     `json:"current"`
	Final   bool     `json:"final"`
}

type appStep struct {
	Title string   `json:"title"`
	Rows  []appRow `json:"rows"`
	Note  string   `json:"note,omitempty"`
	Empty string   `json:"empty,omitempty"`
}

// maxHops bounds the levels one step goes down on its own.
const maxHops = 6

// appStep: POST /api/app/step — the choice goes source → voice → (season →
// episode) → quality; a step lists what the level it stops at offers, and
// goes down by itself where the call already names the season or episode.
func (a *API) appStep(w http.ResponseWriter, r *http.Request) {
	in, ref, ok := a.readCall(w, r)
	if !ok {
		return
	}
	ctx, u := r.Context(), userOf(r)
	mem := a.memory(ctx, u, in.Kind, in.TMDBID)
	var step appStep
	var err error
	switch {
	case ref == nil:
		step, err = a.sourceStep(ctx, r, u, in, mem)
	case ref.K == refFile:
		step = qualityStep(r, *ref, mem)
	case ref.K == refBrowse:
		step, err = a.browseStep(ctx, r, u, in, ref.R, mem)
	default: // a play is a final row: it has no step
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return
	}
	if errors.Is(err, online.ErrNothing) {
		step, err = appStep{Title: tr(r, "Источник"), Rows: []appRow{}, Empty: tr(r, msgSourceEmpty)}, nil
	}
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, step)
}

func (a *API) sourceStep(ctx context.Context, r *http.Request, u online.User, in appCall, mem memory) (appStep, error) {
	srcs, err := a.d.Online.Sources(ctx, u, in.Kind, in.TMDBID)
	if err != nil {
		return appStep{}, err
	}
	rows := make([]appRow, 0, len(srcs))
	for _, s := range srcs {
		rows = append(rows, appRow{Ref: appRef{K: refBrowse, R: s.Ref}.encode(), Label: s.Name, Detail: s.Quality,
			Current: mem.last.Source != "" && s.ID == mem.last.Source})
	}
	return appStep{Title: tr(r, "Источник"), Rows: rows, Empty: tr(r, msgNoSources)}, nil
}

// browseStep opens the level ref points to and lists its choice: the
// source's candidates, the voices (where it lists several and none was
// picked), the seasons (unless the call names one), the episodes, or a
// film's files.
func (a *API) browseStep(ctx context.Context, r *http.Request, u online.User, in appCall, ref string, mem memory) (appStep, error) {
	svc := a.d.Online
	lv, err := svc.Browse(ctx, u, ref)
	if err != nil {
		return appStep{}, err
	}
	for range maxHops {
		_, rc, err := svc.Refs.Open(ref)
		if err != nil {
			return appStep{}, online.ErrBadRef
		}
		switch {
		case lv.Level == "pick":
			return candidateStep(r, lv), nil
		case len(lv.Voices) > 1 && rc.Voice == "":
			return voiceStep(r, lv, mem), nil
		case lv.Level == "seasons":
			if in.Season <= 0 {
				return seasonStep(r, lv, mem), nil
			}
			s, ok := seasonOf(lv, in.Season)
			if !ok {
				return appStep{}, online.ErrNothing
			}
			ref = s.Ref
			if lv, err = svc.Browse(ctx, u, ref); err != nil {
				return appStep{}, err
			}
		case lv.Level == "episodes":
			return episodeStep(r, lv, in, mem), nil
		case lv.Level == "movie":
			return fileStep(r, lv, mem), nil
		default:
			return appStep{}, online.ErrNothing
		}
	}
	return appStep{}, online.ErrNothing
}

func seasonOf(lv online.Level, n int) (online.Season, bool) {
	for _, s := range lv.Seasons {
		if s.Number == n {
			return s, true
		}
	}
	return online.Season{}, false
}

func candidateStep(r *http.Request, lv online.Level) appStep {
	rows := make([]appRow, 0, len(lv.Candidates))
	for _, c := range lv.Candidates {
		label := c.Title
		if c.Year > 0 {
			label += " (" + strconv.Itoa(c.Year) + ")"
		}
		rows = append(rows, appRow{Ref: appRef{K: refBrowse, R: c.Ref}.encode(), Label: label, Detail: c.Details})
	}
	return appStep{Title: tr(r, "Вариант"), Rows: rows, Note: tr(r, msgPickMatch), Empty: tr(r, msgSourceEmpty)}
}

func voiceStep(r *http.Request, lv online.Level, mem memory) appStep {
	rows := make([]appRow, 0, len(lv.Voices))
	for _, v := range lv.Voices {
		cur := v.Active
		if mem.last.Voice != "" {
			cur = v.Name == mem.last.Voice
		}
		rows = append(rows, appRow{Ref: appRef{K: refBrowse, R: v.Ref}.encode(), Label: v.Name, Current: cur})
	}
	return appStep{Title: tr(r, "Озвучка"), Rows: rows, Empty: tr(r, msgSourceEmpty)}
}

func seasonStep(r *http.Request, lv online.Level, mem memory) appStep {
	rows := make([]appRow, 0, len(lv.Seasons))
	for _, s := range lv.Seasons {
		rows = append(rows, appRow{Ref: appRef{K: refBrowse, R: s.Ref}.encode(), Label: tr(r, "Сезон %d", s.Number),
			Current: mem.last.Season == s.Number})
	}
	return appStep{Title: tr(r, "Сезон"), Rows: rows, Empty: tr(r, msgSourceEmpty)}
}

// episodeStep lists a season's episodes; a call that names the episode
// lists only it (the row says what will play and asks its quality).
func episodeStep(r *http.Request, lv online.Level, in appCall, mem memory) appStep {
	rows := []appRow{}
	for i, e := range lv.Episodes {
		if e.Unsupported {
			continue
		}
		if in.Episode > 0 && (e.Episode != in.Episode || (in.Season > 0 && e.Season != in.Season)) {
			continue
		}
		ref := appRef{K: refPlay, R: e.Ref}
		if i+1 < len(lv.Episodes) {
			ref.NS, ref.NE = lv.Episodes[i+1].Season, lv.Episodes[i+1].Episode
		} else {
			ref.L = true
		}
		rows = append(rows, itemRow(r, ref, e.Name, e.Qualities, e.Subtitles, e.SubtitleTracks,
			mem.last.Season == e.Season && mem.last.Episode == e.Episode))
	}
	return appStep{Title: tr(r, "Серия"), Rows: rows, Empty: tr(r, msgSourceEmpty)}
}

// fileStep lists a film's files: one per voice-over.
func fileStep(r *http.Request, lv online.Level, mem memory) appStep {
	rows := []appRow{}
	for _, f := range lv.Files {
		if f.Unsupported {
			continue
		}
		voice := fileVoice(f)
		label := voice
		if label == "" {
			label = tr(r, msgDefaultAudio)
		}
		rows = append(rows, itemRow(r, appRef{K: refPlay, R: f.Ref}, label, f.Qualities, f.Subtitles, f.SubtitleTracks,
			mem.last.Voice != "" && voice == mem.last.Voice))
	}
	return appStep{Title: tr(r, "Озвучка"), Rows: rows, Empty: tr(r, msgSourceEmpty)}
}

// fileVoice is the voice-over a film file plays in.
func fileVoice(f online.File) string {
	if f.Voice != "" {
		return f.Voice
	}
	return f.Name
}

// itemRow is a file or episode as a row: its qualities as the second line,
// its subtitles as a badge. One quality (or none) is played at once; several
// are asked next.
func itemRow(r *http.Request, ref appRef, label string, qualities []string, subs int, tracks []online.SubInfo, current bool) appRow {
	row := appRow{Label: label, Detail: strings.Join(qualities, " · "), Current: current, Final: len(qualities) <= 1}
	if subs > 0 {
		row.Badges = []string{subsBadge(r, tracks)}
	}
	if !row.Final {
		ref.K, ref.Qs = refFile, qualities
	}
	row.Ref = ref.encode()
	return row
}

// subsBadge is «Субтитры: RU, EN» («Субтитры» when no track names its
// language).
func subsBadge(r *http.Request, tracks []online.SubInfo) string {
	var langs []string
	seen := map[string]bool{}
	for _, t := range tracks {
		l := strings.ToUpper(strings.TrimSpace(t.Lang))
		if l != "" && !seen[l] {
			seen[l] = true
			langs = append(langs, l)
		}
	}
	if len(langs) == 0 {
		return tr(r, "Субтитры")
	}
	return tr(r, "Субтитры: %s", strings.Join(langs, ", "))
}

func qualityStep(r *http.Request, ref appRef, mem memory) appStep {
	rows := make([]appRow, 0, len(ref.Qs))
	for _, q := range ref.Qs {
		play := ref
		play.K, play.Q, play.Qs = refPlay, q, nil
		rows = append(rows, appRow{Ref: play.encode(), Label: q, Current: q == mem.last.Quality, Final: true})
	}
	return appStep{Title: tr(r, "Качество"), Rows: rows, Empty: tr(r, msgSourceEmpty)}
}
