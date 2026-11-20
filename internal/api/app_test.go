package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// --- the fakes ---

const app = "/api/addons/online/x/app"

// episode is a Lampac episode of beta: qualities by name ("" url: none).
func episode(s, e int, name string, qualities map[string]string, extra string) string {
	q, _ := json.Marshal(qualities)
	return `{"method":"play","url":"http://cdn.example/v720/index.m3u8","quality":` + string(q) +
		`,"headers":{"Referer":"https://player.example/"},"s":` + itoa(s) + `,"e":` + itoa(e) + `,"name":"` + name + `"` + extra + `}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

var (
	q1080and720 = map[string]string{"1080p": "http://cdn.example/v/master.m3u8", "720p": "http://cdn.example/v720/index.m3u8"}
	q720        = map[string]string{"720p": "http://cdn.example/v720/index.m3u8"}
)

const betaVoices = `[{"method":"link","url":"http://lampac.test:9118/lite/beta?rjson=True%s&t=1","active":%a,"name":"Дубляж"},` +
	`{"method":"link","url":"http://lampac.test:9118/lite/beta?rjson=True%s&t=2","active":%b,"name":"Многоголосый"}]`

// seriesLampac makes the play fake's Lampac serve a series: source beta
// (seasons with two voice-overs, season 1 with three episodes, the third
// DASH only) and source delta, which fails.
func seriesLampac(f *playFake, hideFirst *atomic.Bool) {
	h := func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		// a level's voice links keep its season
		voices := func(t string) string {
			a, b := "true", "false"
			if t == "2" {
				a, b = "false", "true"
			}
			season := ""
			if s := q.Get("s"); s != "" {
				season = "&s=" + s
			}
			return strings.NewReplacer("%a", a, "%b", b, "%s", season).Replace(betaVoices)
		}
		switch r.URL.Path {
		case "/lite/events":
			if q.Get("id") == "2" { // the film whose source offers titles to pick from
				w.Write([]byte(`[{"name":"gamma","url":"http://lampac.test:9118/lite/gamma","show":true,"balanser":"gamma"}]`))
				return true
			}
			if q.Get("serial") != "1" {
				return false
			}
			w.Write([]byte(`[{"name":"beta - 720p","url":"http://lampac.test:9118/lite/beta","show":true,"balanser":"beta"},` +
				`{"name":"delta","url":"http://lampac.test:9118/lite/delta","show":true,"balanser":"delta"}]`))
		case "/lite/gamma":
			if h := q.Get("href"); h != "" {
				w.Write([]byte(`{"type":"movie","data":[{"method":"play","translate":"Дубляж","url":"http://cdn.example/v720/index.m3u8",` +
					`"quality":{"720p":"http://cdn.example/v720/index.m3u8"},"headers":{"Referer":"https://player.example/"}}]}`))
				return true
			}
			w.Write([]byte(`{"type":"similar","data":[` +
				`{"method":"link","url":"http://lampac.test:9118/lite/gamma?href=a","title":"Фильм A","year":2021,"details":"драма"},` +
				`{"method":"link","url":"http://lampac.test:9118/lite/gamma?href=b","title":"Фильм B","year":2020}]}`))
		case "/lite/delta":
			w.WriteHeader(503)
		case "/lite/beta":
			t := q.Get("t")
			switch q.Get("s") {
			case "":
				w.Write([]byte(`{"type":"season","voice":` + voices(t) + `,"data":[` +
					`{"method":"link","id":1,"url":"http://lampac.test:9118/lite/beta?rjson=False&s=1&t=` + t + `","name":"1 сезон"},` +
					`{"method":"link","id":2,"url":"http://lampac.test:9118/lite/beta?rjson=False&s=2&t=` + t + `","name":"2 сезон"}]}`))
			case "1":
				sub := `,"subtitles":[{"label":"Русские","url":"http://cdn.example/ru.srt"},{"label":"English","url":"http://cdn.example/en.srt"}]`
				eps := episode(1, 1, "Серия 1", q1080and720, sub) + "," + episode(1, 2, "Пилот", q720, "") + "," +
					`{"method":"play","url":"http://cdn.example/v.mpd","quality":{"1080p":"http://cdn.example/v.mpd"},"s":1,"e":3,"name":"Серия 3"}`
				if t == "2" {
					eps = episode(1, 1, "Серия 1", q720, "")
				}
				if hideFirst.Load() { // a source that dropped the first episode
					eps = episode(1, 2, "Пилот", q720, "")
				}
				w.Write([]byte(`{"type":"episode","voice":` + voices(t) + `,"data":[` + eps + `]}`))
			case "2":
				w.Write([]byte(`{"type":"episode","voice":` + voices(t) + `,"data":[` + episode(2, 1, "Серия 1", q720, "") + `]}`))
			default:
				w.WriteHeader(404)
			}
		default:
			return false
		}
		return true
	}
	f.extra.Store(&h)
	f.svc.Titles = func(ctx context.Context, kind string, id int) (online.TitleCtx, error) {
		if id == 1396 {
			return online.TitleCtx{Title: "Бета", Rating: "12+", Year: 2020, Serial: kind == "series"}, nil
		}
		return fakeTitles(ctx, kind, id)
	}
}

// appEnv is the app routes over the play fake (a film with three files
// under source alpha; the series 1396 under beta).
type appEnv struct {
	*env
	f *playFake
	// hideFirst makes beta drop the first episode of season 1
	hideFirst atomic.Bool
}

func newAppEnv(t *testing.T, opts ...envOpt) *appEnv {
	t.Helper()
	f := newPlayFake(t)
	e := &appEnv{f: f}
	seriesLampac(f, &e.hideFirst)
	e.env = newEnv(t, f.svc, opts...)
	return e
}

type appStepAnswer struct {
	Title string
	Rows  []struct {
		Ref, Label, Detail string
		Badges             []string
		Current, Final     bool
	}
	Note, Empty string
}

func (e *appEnv) get(path string, u *addonkit.User) *httptest.ResponseRecorder {
	return e.do("GET", app+path, "", u)
}

func (e *appEnv) post(path, body string, u *addonkit.User) *httptest.ResponseRecorder {
	return e.do("POST", app+path, body, u)
}

// step posts a step and decodes it.
func (e *appEnv) step(t *testing.T, body string) appStepAnswer {
	t.Helper()
	w := e.post("/step", body, admin)
	var s appStepAnswer
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatalf("step %s: %d %s", body, w.Code, w.Body)
	}
	return s
}

func call(kind string, id int, ref, extra string) string {
	b, _ := json.Marshal(ref)
	r := `"ref":` + string(b)
	if ref == "" {
		r = `"ref":""`
	}
	return `{"way":"watch","kind":"` + kind + `","tmdbId":` + itoa(id) + `,` + r + extra + `}`
}

func labels(s appStepAnswer) string {
	var out []string
	for _, r := range s.Rows {
		out = append(out, r.Label)
	}
	return strings.Join(out, "|")
}

type runAnswer struct {
	Pick bool
	Play *struct {
		Mode, Kind, URL, Session string
		StartSec                 float64
		Quality                  struct {
			Options []string
			Current string
		}
		Progress struct {
			Path  string
			Every int
		}
		Subtitles []struct{ URL string }
	}
}

func (e *appEnv) run(t *testing.T, body string, u *addonkit.User) (runAnswer, *httptest.ResponseRecorder) {
	t.Helper()
	w := e.post("/run", body, u)
	var a runAnswer
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
	}
	return a, w
}

// --- the description and who may ask ---

func TestApp_Info(t *testing.T) {
	e := newAppEnv(t, func(d *Deps) { d.Search = &fakeSearch{} })
	w := e.get("", mama)
	const want = `{"v":1,"ways":[{"id":"watch","label":"Онлайн","icon":"globe","role":"watch","kinds":["movie","series"],"scope":"any","episodes":true}],` +
		`"search":{"label":"Найти и смотреть"}}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	en := e.get("", &addonkit.User{ID: 3, UID: "uid-mama", Name: "Mama", Lang: "en"})
	if !strings.Contains(en.Body.String(), `"label":"Online"`) || !strings.Contains(en.Body.String(), `"label":"Find and Watch"`) {
		t.Fatalf("en: %s", en.Body)
	}
	// without a TMDB the way has no search
	if w := newAppEnv(t).get("", mama); w.Code != 200 || strings.Contains(w.Body.String(), "search") {
		t.Fatalf("no search: %d %s", w.Code, w.Body)
	}
}

// While «Смотреть онлайн» is off or has no Lampac every app route is 404:
// the app shows nothing.
func TestApp_NotOffered(t *testing.T) {
	routes := [][3]string{
		{"GET", app, ""},
		{"GET", app + "/search?q=x", ""},
		{"POST", app + "/step", call("movie", 438631, "", "")},
		{"POST", app + "/run", call("movie", 438631, "", `,"quick":true`)},
		{"PUT", app + "/progress", `{"way":"watch","session":"x","kind":"movie","tmdbId":438631,"positionMs":1,"durationMs":10}`},
		{"DELETE", app + "/play/x", ""},
		{"GET", app + "/state", ""},
	}
	f := newPlayFake(t)
	e := newEnv(t, f.svc, func(d *Deps) { d.Search = &fakeSearch{} })
	for _, rt := range routes {
		// a session that is not there is a 404 with its own words, not the missing route's
		if w := e.do(rt[0], rt[1], rt[2], mama); w.Code == 404 && errOf(w) != "Сеанс не найден" {
			t.Fatalf("on: %s %s is 404: %s", rt[0], rt[1], w.Body)
		}
	}
	e.enabled.Store(false)
	for _, rt := range routes {
		if w := e.do(rt[0], rt[1], rt[2], mama); w.Code != 404 {
			t.Errorf("off: %s %s: %d %s", rt[0], rt[1], w.Code, w.Body)
		}
	}
	none := newEnv(t, nil, func(d *Deps) { d.Search = &fakeSearch{} })
	for _, rt := range routes {
		if w := none.do(rt[0], rt[1], rt[2], mama); w.Code != 404 {
			t.Errorf("no Lampac: %s %s: %d %s", rt[0], rt[1], w.Code, w.Body)
		}
	}
	if f.cdn.Load() != 0 {
		t.Fatal("an off addon reached the source")
	}
}

func TestApp_KidRefused(t *testing.T) {
	e := newAppEnv(t, func(d *Deps) { d.Search = &fakeSearch{} })
	for _, rt := range [][3]string{
		{"GET", app, ""},
		{"GET", app + "/search?q=x", ""},
		{"POST", app + "/step", call("movie", 438631, "", "")},
		{"POST", app + "/run", call("movie", 438631, "", `,"quick":true`)},
		{"PUT", app + "/progress", `{"way":"watch","session":"x","kind":"movie","tmdbId":438631,"positionMs":1,"durationMs":10}`},
		{"DELETE", app + "/play/x", ""},
		{"GET", app + "/state", ""},
	} {
		for name, u := range map[string]*addonkit.User{"kid": kid, "no header": nil} {
			w := e.do(rt[0], rt[1], rt[2], u)
			if w.Code != 403 || errOf(w) != "Недоступно в детском профиле" {
				t.Errorf("%s %s %s: %d %s", name, rt[0], rt[1], w.Code, w.Body)
			}
		}
	}
	if e.f.cdn.Load() != 0 {
		t.Fatal("a kid reached the source")
	}
}

// --- the search ---

type fakeSearch struct {
	err    error
	movies []tmdb.Result
	shows  []tmdb.Result
	langs  []string
}

func (s *fakeSearch) SearchMovie(ctx context.Context, q string, _ int) ([]tmdb.Result, error) {
	s.langs = append(s.langs, tmdb.ContextLanguage(ctx))
	if s.err != nil {
		return nil, s.err
	}
	if s.movies != nil {
		return s.movies, nil
	}
	return []tmdb.Result{
		{ID: 603, Title: "Матрица", Year: 1999, Overview: "Хакер", Poster: "/m.jpg"},
		{ID: 666, Title: "Для взрослых", Adult: true},
	}, nil
}

func (s *fakeSearch) SearchTV(_ context.Context, q string, _ int) ([]tmdb.Result, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.shows != nil {
		return s.shows, nil
	}
	return []tmdb.Result{{ID: 1396, Title: "Бета"}}, nil
}

func (s *fakeSearch) ImageURL(size, p string) string { return "https://img.example/" + size + p }

// lib answers the library lookup of the search: tmdb id 603 is item 77.
type lib struct{ online.LibraryLookup }

func (lib) ItemByTMDB(_ context.Context, kind string, id int64) (addonkit.Item, bool, error) {
	if kind == "movie" && id == 603 {
		return addonkit.Item{ID: 77}, true, nil
	}
	return addonkit.Item{}, false, nil
}

func TestApp_Search(t *testing.T) {
	fs := &fakeSearch{}
	e := newAppEnv(t, func(d *Deps) {
		d.Search = fs
		d.Progress.Lib = lib{}
	})
	w := e.get("/search?q=matrix", &addonkit.User{ID: 3, UID: "uid-mama", Name: "Mama", Lang: "en"})
	const want = `{"titles":[` +
		`{"kind":"movie","tmdbId":603,"title":"Матрица","year":1999,"poster":"https://img.example/w342/m.jpg","overview":"Хакер","inLibrary":true,"libraryItemId":77},` +
		`{"kind":"series","tmdbId":1396,"title":"Бета","inLibrary":false}]}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(fs.langs) != 1 || fs.langs[0] != "en-US" {
		t.Fatalf("TMDB asked in %v", fs.langs)
	}
	// nothing typed: nothing asked, an empty list
	n := len(fs.langs)
	if w := e.get("/search?q=%20", mama); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"titles":[]}` || len(fs.langs) != n {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	// one list failing still gives the other; both failing is an error
	fs.err = nil
	fs.shows = []tmdb.Result{}
	if w := e.get("/search?q=matrix", mama); w.Code != 200 || strings.Contains(w.Body.String(), `"series"`) {
		t.Fatalf("movies only: %d %s", w.Code, w.Body)
	}
	fs.err = errors.New("tmdb down")
	if w := e.get("/search?q=matrix", mama); w.Code != 500 || errOf(w) != "Внутренняя ошибка сервера" {
		t.Fatalf("both failed: %d %s", w.Code, w.Body)
	}
	// without a TMDB there is no search
	if w := newAppEnv(t).get("/search?q=matrix", mama); w.Code != 404 {
		t.Fatalf("no search: %d", w.Code)
	}
}

// --- a film ---

// The film: source → voice-over (a file each) → quality → play.
func TestApp_Step_Film(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("movie", 438631, "", ""))
	if src.Title != "Источник" || len(src.Rows) != 1 || src.Rows[0].Label != "alpha" || src.Rows[0].Final || src.Rows[0].Current || src.Empty == "" {
		t.Fatalf("sources: %+v", src)
	}
	files := e.step(t, call("movie", 438631, src.Rows[0].Ref, ""))
	// the DASH-only file is not offered; the call entry has no qualities named
	if files.Title != "Озвучка" || labels(files) != "Дубляж|Многоголосый" {
		t.Fatalf("files: %+v", files)
	}
	dub, multi := files.Rows[0], files.Rows[1]
	if dub.Detail != "1080p · 720p" || dub.Final || len(dub.Badges) != 1 || !strings.HasPrefix(dub.Badges[0], "Субтитры") || !multi.Final || multi.Detail != "" {
		t.Fatalf("rows: %+v %+v", dub, multi)
	}
	quality := e.step(t, call("movie", 438631, dub.Ref, ""))
	if quality.Title != "Качество" || labels(quality) != "1080p|720p" || !quality.Rows[0].Final || !quality.Rows[1].Final {
		t.Fatalf("qualities: %+v", quality)
	}
	// a quality is played; the answer is the play of the web with the app's extras
	a, w := e.run(t, call("movie", 438631, quality.Rows[1].Ref, `,"startSec":12`), admin)
	if w.Code != 200 || a.Play == nil || a.Play.Mode != "online" || a.Play.Kind != "hls" || a.Play.Quality.Current != "720p" ||
		strings.Join(a.Play.Quality.Options, ",") != "1080p,720p" || a.Play.StartSec != 12 || a.Play.Session == "" ||
		!strings.HasPrefix(a.Play.URL, StreamBase+a.Play.Session+"/") || a.Play.Progress.Path != "x/app/progress" || a.Play.Progress.Every != 10 {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	pl := e.do("GET", a.Play.URL, "", admin)
	if pl.Code != 200 || !strings.Contains(pl.Body.String(), "#EXTINF") {
		t.Fatalf("playlist: %d %s", pl.Code, pl.Body)
	}
	// a file with one quality plays from its own row
	b, w := e.run(t, call("movie", 438631, multi.Ref, ""), admin)
	if w.Code != 200 || b.Play == nil || b.Play.Kind != "mp4" || b.Play.StartSec != 0 {
		t.Fatalf("single quality: %d %s", w.Code, w.Body)
	}
}

// The last choices of the title mark the rows (and the voice-over the
// preselected one).
func TestApp_Step_MarksLastChoices(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("movie", 438631, "", ""))
	a, w := e.run(t, call("movie", 438631, e.step(t, call("movie", 438631, e.step(t, call("movie", 438631, src.Rows[0].Ref, "")).Rows[0].Ref, "")).Rows[0].Ref, ""), admin)
	if w.Code != 200 {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	e.putApp(t, admin, a.Play.Session, `"positionMs":1000,"durationMs":100000`)
	src = e.step(t, call("movie", 438631, "", ""))
	if !src.Rows[0].Current {
		t.Fatalf("source not marked: %+v", src)
	}
	files := e.step(t, call("movie", 438631, src.Rows[0].Ref, ""))
	if !files.Rows[0].Current || files.Rows[1].Current {
		t.Fatalf("voice not marked: %+v", files)
	}
	quality := e.step(t, call("movie", 438631, files.Rows[0].Ref, ""))
	if !quality.Rows[0].Current || quality.Rows[1].Current { // the default, 1080p, was played
		t.Fatalf("quality not marked: %+v", quality)
	}
}

// putApp sends a position of session as u.
func (e *appEnv) putApp(t *testing.T, u *addonkit.User, session, fields string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"way":"watch","session":"` + session + `","kind":"movie","tmdbId":438631,` + fields + `}`
	w := e.do("PUT", app+"/progress", body, u)
	if w.Code != 204 {
		t.Fatalf("progress: %d %s", w.Code, w.Body)
	}
	return w
}

func (e *appEnv) saved(t *testing.T, u *addonkit.User, kind string, id int) struct {
	Entries []online.Entry
	Last    *online.Last
} {
	t.Helper()
	w := e.do("GET", "/api/addons/online/x/progress?kind="+kind+"&tmdbId="+itoa(id), "", u)
	var out struct {
		Entries []online.Entry
		Last    *online.Last
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("progress read: %d %s", w.Code, w.Body)
	}
	return out
}

// --- a series ---

// The series: source → voice-over → season → episode → quality.
func TestApp_Step_Series(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("series", 1396, "", ""))
	if labels(src) != "beta|delta" || src.Rows[0].Detail != "720p" {
		t.Fatalf("sources: %+v", src)
	}
	voices := e.step(t, call("series", 1396, src.Rows[0].Ref, ""))
	if voices.Title != "Озвучка" || labels(voices) != "Дубляж|Многоголосый" || !voices.Rows[0].Current || voices.Rows[1].Current || voices.Rows[0].Final {
		t.Fatalf("voices: %+v", voices)
	}
	// the voice-over picked, the seasons follow (no second voice step)
	seasons := e.step(t, call("series", 1396, voices.Rows[1].Ref, ""))
	if seasons.Title != "Сезон" || labels(seasons) != "Сезон 1|Сезон 2" || seasons.Rows[0].Final {
		t.Fatalf("seasons: %+v", seasons)
	}
	eps := e.step(t, call("series", 1396, seasons.Rows[0].Ref, ""))
	// Многоголосый has the first episode only
	if eps.Title != "Серия" || labels(eps) != "Серия 1" || !eps.Rows[0].Final {
		t.Fatalf("episodes in the second voice: %+v", eps)
	}
	// the first voice-over: three episodes, the DASH one left out
	seasons = e.step(t, call("series", 1396, voices.Rows[0].Ref, ""))
	eps = e.step(t, call("series", 1396, seasons.Rows[0].Ref, ""))
	if labels(eps) != "Серия 1|Пилот" || eps.Rows[0].Final || !eps.Rows[1].Final || eps.Rows[0].Detail != "1080p · 720p" ||
		len(eps.Rows[0].Badges) != 1 || eps.Rows[0].Badges[0] != "Субтитры: RU, EN" || eps.Rows[1].Badges != nil {
		t.Fatalf("episodes: %+v", eps)
	}
	quality := e.step(t, call("series", 1396, eps.Rows[0].Ref, ""))
	if labels(quality) != "1080p|720p" {
		t.Fatalf("qualities: %+v", quality)
	}
	a, w := e.run(t, call("series", 1396, quality.Rows[0].Ref, ""), admin)
	if w.Code != 200 || a.Play == nil || a.Play.Quality.Current != "1080p" || a.Play.Session == "" {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
}

// A call that names the season and episode (an episode's menu) skips the
// season and episode steps: after the voice-over, only that episode.
func TestApp_Step_SeriesGivenEpisode(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("series", 1396, "", `,"season":1,"episode":2`))
	voices := e.step(t, call("series", 1396, src.Rows[0].Ref, `,"season":1,"episode":2`))
	if voices.Title != "Озвучка" {
		t.Fatalf("voices: %+v", voices)
	}
	one := e.step(t, call("series", 1396, voices.Rows[0].Ref, `,"season":1,"episode":2`))
	if one.Title != "Серия" || labels(one) != "Пилот" || !one.Rows[0].Final {
		t.Fatalf("the episode: %+v", one)
	}
	if a, w := e.run(t, call("series", 1396, one.Rows[0].Ref, ""), admin); w.Code != 200 || a.Play == nil {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	// an episode the season lacks is an empty step, not an error
	none := e.step(t, call("series", 1396, voices.Rows[0].Ref, `,"season":1,"episode":9`))
	if len(none.Rows) != 0 || none.Empty == "" {
		t.Fatalf("missing episode: %+v", none)
	}
	// a season the source lacks, likewise
	none = e.step(t, call("series", 1396, voices.Rows[0].Ref, `,"season":7,"episode":1`))
	if len(none.Rows) != 0 || none.Empty != "Ничего не нашлось в этом источнике" {
		t.Fatalf("missing season: %+v", none)
	}
}

// --- the run: quick ---

// «Сразу» without a ref needs the last choices of the title; without them
// the app is told to pick.
func TestApp_Run_Quick(t *testing.T) {
	e := newAppEnv(t)
	if a, w := e.run(t, call("movie", 438631, "", `,"quick":true`), admin); w.Code != 200 || !a.Pick || a.Play != nil || strings.TrimSpace(w.Body.String()) != `{"pick":true}` {
		t.Fatalf("no memory: %d %s", w.Code, w.Body)
	}
	// play the film once: 720p of the first voice-over
	src := e.step(t, call("movie", 438631, "", ""))
	files := e.step(t, call("movie", 438631, src.Rows[0].Ref, ""))
	quality := e.step(t, call("movie", 438631, files.Rows[0].Ref, ""))
	first, w := e.run(t, call("movie", 438631, quality.Rows[1].Ref, ""), admin)
	if w.Code != 200 {
		t.Fatalf("first play: %d %s", w.Code, w.Body)
	}
	e.putApp(t, admin, first.Play.Session, `"positionMs":30000,"durationMs":100000`)
	a, w := e.run(t, call("movie", 438631, "", `,"quick":true`), admin)
	if w.Code != 200 || a.Play == nil || a.Play.Quality.Current != "720p" || a.Play.StartSec != 30 || a.Play.Session == first.Play.Session {
		t.Fatalf("quick: %d %s", w.Code, w.Body)
	}
	// the start the app names wins over the saved position
	if a, _ := e.run(t, call("movie", 438631, "", `,"quick":true,"startSec":0`), admin); a.Play == nil || a.Play.StartSec != 0 {
		t.Fatalf("named start: %+v", a.Play)
	}
	// another profile has no memory of it
	if a, _ := e.run(t, call("movie", 438631, "", `,"quick":true`), mama); !a.Pick {
		t.Fatalf("another profile: %+v", a)
	}
	// the cap of «Авто» reaches the resolve (the remembered quality still wins)
	if a, w := e.run(t, call("movie", 438631, "", `,"quick":true,"maxHeight":720`), admin); w.Code != 200 || a.Play == nil {
		t.Fatalf("capped: %d %s", w.Code, w.Body)
	}
}

// A series goes on at the remembered episode, or the next one when that was
// watched to the end; a call naming the episode plays that one; a source
// that no longer lists the remembered episode is a pick.
func TestApp_Run_QuickSeries(t *testing.T) {
	e := newAppEnv(t)
	playEp := func(n int) runAnswer {
		t.Helper()
		src := e.step(t, call("series", 1396, "", ""))
		voices := e.step(t, call("series", 1396, src.Rows[0].Ref, ""))
		seasons := e.step(t, call("series", 1396, voices.Rows[0].Ref, ""))
		eps := e.step(t, call("series", 1396, seasons.Rows[0].Ref, ""))
		ref := eps.Rows[n].Ref
		if !eps.Rows[n].Final {
			ref = e.step(t, call("series", 1396, ref, "")).Rows[1].Ref // 720p
		}
		a, w := e.run(t, call("series", 1396, ref, ""), admin)
		if w.Code != 200 {
			t.Fatalf("play %d: %d %s", n, w.Code, w.Body)
		}
		return a
	}
	save := func(session string, season, episode int, pos, dur int) {
		body := `{"way":"watch","session":"` + session + `","kind":"series","tmdbId":1396,"season":` + itoa(season) + `,"episode":` + itoa(episode) +
			`,"positionMs":` + itoa(pos) + `,"durationMs":` + itoa(dur) + `}`
		if w := e.do("PUT", app+"/progress", body, admin); w.Code != 204 {
			t.Fatalf("save: %d %s", w.Code, w.Body)
		}
	}
	first := playEp(0)
	save(first.Play.Session, 1, 1, 20000, 100000)
	// the title as of the last play: season 1, episode 1, 720p, at 20 s
	a, w := e.run(t, call("series", 1396, "", `,"quick":true`), admin)
	if w.Code != 200 || a.Play == nil || a.Play.StartSec != 20 || a.Play.Quality.Current != "720p" {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	out := e.saved(t, admin, "series", 1396)
	if out.Last == nil || out.Last.Source != "beta" || out.Last.Voice != "Дубляж" || out.Last.Season != 1 || out.Last.Episode != 1 {
		t.Fatalf("last choices: %+v", out.Last)
	}
	// watched to the end: the next episode, from its start
	save(a.Play.Session, 1, 1, 99000, 100000)
	next, w := e.run(t, call("series", 1396, "", `,"quick":true`), admin)
	if w.Code != 200 || next.Play == nil || next.Play.StartSec != 0 {
		t.Fatalf("next: %d %s", w.Code, w.Body)
	}
	if got := e.saved(t, admin, "series", 1396); len(got.Entries) != 1 {
		t.Fatalf("entries %+v", got.Entries)
	}
	save(next.Play.Session, 1, 2, 5000, 100000)
	if got := e.saved(t, admin, "series", 1396); got.Last.Episode != 2 || len(got.Entries) != 2 {
		t.Fatalf("after the next: %+v %+v", got.Last, got.Entries)
	}
	// the episode of the call, whatever was remembered
	exact, w := e.run(t, call("series", 1396, "", `,"quick":true,"season":1,"episode":1`), admin)
	if w.Code != 200 || exact.Play == nil || exact.Play.StartSec != 0 {
		t.Fatalf("exact (finished, not skipped): %d %s", w.Code, w.Body)
	}
	// season 2 has the single-quality episode
	if a, w := e.run(t, call("series", 1396, "", `,"quick":true,"season":2,"episode":1`), admin); w.Code != 200 || a.Play == nil {
		t.Fatalf("season 2: %d %s", w.Code, w.Body)
	}
	// an episode the source lacks: pick
	if a, _ := e.run(t, call("series", 1396, "", `,"quick":true,"season":1,"episode":9`), admin); !a.Pick {
		t.Fatalf("missing episode: %+v", a)
	}
	if a, _ := e.run(t, call("series", 1396, "", `,"quick":true,"season":8,"episode":1`), admin); !a.Pick {
		t.Fatalf("missing season: %+v", a)
	}
	// the DASH-only episode cannot play: pick
	if a, _ := e.run(t, call("series", 1396, "", `,"quick":true,"season":1,"episode":3`), admin); !a.Pick {
		t.Fatalf("unsupported: %+v", a)
	}
}

// A source that offers several titles asks which one is meant (a «pick» level);
// «сразу» cannot choose for the person.
func TestApp_Step_PickLevel(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("movie", 2, "", ""))
	if len(src.Rows) != 1 || src.Rows[0].Label != "gamma" {
		t.Fatalf("sources: %+v", src)
	}
	pick := e.step(t, call("movie", 2, src.Rows[0].Ref, ""))
	if pick.Title != "Вариант" || labels(pick) != "Фильм A (2021)|Фильм B (2020)" || pick.Rows[0].Detail != "драма" || pick.Rows[0].Final ||
		pick.Note != "Выберите подходящий вариант" {
		t.Fatalf("candidates: %+v", pick)
	}
	files := e.step(t, call("movie", 2, pick.Rows[0].Ref, ""))
	if files.Title != "Озвучка" || labels(files) != "Дубляж" || !files.Rows[0].Final {
		t.Fatalf("files: %+v", files)
	}
	a, w := e.run(t, call("movie", 2, files.Rows[0].Ref, ""), admin)
	if w.Code != 200 || a.Play == nil {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	body := `{"way":"watch","session":"` + a.Play.Session + `","kind":"movie","tmdbId":2,"positionMs":1000,"durationMs":100000}`
	if w := e.do("PUT", app+"/progress", body, admin); w.Code != 204 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if a, w := e.run(t, call("movie", 2, "", `,"quick":true`), admin); w.Code != 200 || !a.Pick {
		t.Fatalf("quick over a pick level: %d %s", w.Code, w.Body)
	}
}

// A remembered source that fails, or is gone, is a reason to pick again
// (not an error); one that does not answer at all is the service's error.
func TestApp_Run_QuickRememberedSourceGone(t *testing.T) {
	e := newAppEnv(t)
	e.step(t, call("series", 1396, "", "")) // the title's sources are known now
	remember := func(source string) {
		t.Helper()
		body := `{"kind":"series","tmdbId":1396,"season":1,"episode":1,"positionMs":1000,"durationMs":100000,"source":"` + source + `","voice":"Дубляж"}`
		if w := e.do("PUT", "/api/addons/online/x/progress", body, admin); w.Code != 204 {
			t.Fatalf("remember: %d %s", w.Code, w.Body)
		}
	}
	for _, source := range []string{"delta", "zeta"} { // fails (503), not offered for the title
		remember(source)
		if a, w := e.run(t, call("series", 1396, "", `,"quick":true`), admin); w.Code != 200 || !a.Pick {
			t.Fatalf("%s: %d %s", source, w.Code, w.Body)
		}
	}
}

// «Сразу» goes on in the remembered voice-over, switching to it where the
// source's default is another.
func TestApp_Run_QuickKeepsVoice(t *testing.T) {
	e := newAppEnv(t)
	e.step(t, call("series", 1396, "", ""))
	body := `{"kind":"series","tmdbId":1396,"season":1,"episode":1,"positionMs":1000,"durationMs":100000,"source":"beta","voice":"Многоголосый","quality":"720p"}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, admin); w.Code != 204 {
		t.Fatalf("remember: %d %s", w.Code, w.Body)
	}
	a, w := e.run(t, call("series", 1396, "", `,"quick":true`), admin)
	if w.Code != 200 || a.Play == nil || a.Play.StartSec != 1 {
		t.Fatalf("quick: %d %s", w.Code, w.Body)
	}
	put := `{"way":"watch","session":"` + a.Play.Session + `","kind":"series","tmdbId":1396,"positionMs":2000,"durationMs":100000}`
	if w := e.do("PUT", app+"/progress", put, admin); w.Code != 204 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if got := e.saved(t, admin, "series", 1396); got.Last == nil || got.Last.Voice != "Многоголосый" || got.Last.Quality != "720p" {
		t.Fatalf("voice not kept: %+v", got.Last)
	}
}

// A file the source dropped since the sheet was drawn: the run says so (422:
// a 404 would tell the app the way is gone), and the app can ask again.
func TestApp_Run_FileGone(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("series", 1396, "", ""))
	voices := e.step(t, call("series", 1396, src.Rows[0].Ref, ""))
	seasons := e.step(t, call("series", 1396, voices.Rows[0].Ref, ""))
	eps := e.step(t, call("series", 1396, seasons.Rows[0].Ref, ""))
	ref := e.step(t, call("series", 1396, eps.Rows[0].Ref, "")).Rows[1].Ref
	e.hideFirst.Store(true)
	// the re-play after a stale link reads the level fresh
	w := e.post("/run", call("series", 1396, ref, `,"refresh":true`), admin)
	if w.Code != 422 || errOf(w) != "Ничего не нашлось в этом источнике" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	e.hideFirst.Store(false)
	if a, w := e.run(t, call("series", 1396, ref, `,"refresh":true`), admin); w.Code != 200 || a.Play == nil {
		t.Fatalf("back again: %d %s", w.Code, w.Body)
	}
}

func TestParseRef(t *testing.T) {
	good := appRef{K: refFile, R: "x.y", Qs: []string{"1080p", "720p"}, NS: 1, NE: 2, L: true}
	if got, ok := parseRef(good.encode()); !ok || got == nil || got.R != "x.y" || len(got.Qs) != 2 || got.NS != 1 || !got.L {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
	if got, ok := parseRef(""); !ok || got != nil {
		t.Fatalf("none: %+v %v", got, ok)
	}
	for name, ref := range map[string]string{
		"not base64":     "!!",
		"not JSON":       "bm9wZQ",
		"no ref":         appRef{K: refPlay}.encode(),
		"unknown kind":   appRef{K: "x", R: "r"}.encode(),
		"long quality":   appRef{K: refPlay, R: "r", Q: strings.Repeat("q", 101)}.encode(),
		"many qualities": appRef{K: refFile, R: "r", Qs: make([]string, 21)}.encode(),
		"long one":       appRef{K: refFile, R: "r", Qs: []string{strings.Repeat("q", 101)}}.encode(),
		"negative":       appRef{K: refPlay, R: "r", NS: -1}.encode(),
		"huge":           strings.Repeat("A", maxAppRef+1),
	} {
		if got, ok := parseRef(ref); ok {
			t.Errorf("%s accepted: %+v", name, got)
		}
	}
}

// --- the run: errors ---

func TestApp_Run_Errors(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("movie", 438631, "", ""))
	files := e.step(t, call("movie", 438631, src.Rows[0].Ref, ""))
	for name, c := range map[string]struct {
		body string
		code int
	}{
		"nothing asked":     {call("movie", 438631, "", ""), 400},
		"way gone":          {`{"way":"old","kind":"movie","tmdbId":438631,"quick":true}`, 404},
		"bad kind":          {`{"way":"watch","kind":"music","tmdbId":1,"quick":true}`, 400},
		"no id":             {`{"way":"watch","kind":"movie","quick":true}`, 400},
		"not JSON":          {`nope`, 400},
		"bad ref":           {`{"way":"watch","kind":"movie","tmdbId":438631,"ref":"!!"}`, 400},
		"a step is no play": {call("movie", 438631, src.Rows[0].Ref, ""), 400},
		"quality row":       {call("movie", 438631, files.Rows[0].Ref, ""), 400},
		"forged online ref": {call("movie", 438631, (appRef{K: refPlay, R: "x.y"}).encode(), ""), 400},
	} {
		if w := e.post("/run", c.body, admin); w.Code != c.code || errOf(w) == "" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// the profile's rating limit is the title's gate, as for the web routes
	if w := e.post("/step", call("movie", 1, "", ""), teen); w.Code != 403 || errOf(w) != "Недоступно для этого профиля" {
		t.Fatalf("step over the limit: %d %s", w.Code, w.Body)
	}
	// a ref got by a profile that may see the title (18+) does not carry the limit of its holder:
	// the gate of the title runs on every use
	adminSrc := e.step(t, call("movie", 1, "", ""))
	adminFiles := e.step(t, call("movie", 1, adminSrc.Rows[0].Ref, ""))
	if w := e.post("/step", call("movie", 1, adminSrc.Rows[0].Ref, ""), teen); w.Code != 403 {
		t.Fatalf("browse over the limit: %d %s", w.Code, w.Body)
	}
	if w := e.post("/run", call("movie", 1, adminFiles.Rows[1].Ref, ""), teen); w.Code != 403 || errOf(w) != "Недоступно для этого профиля" {
		t.Fatalf("play over the limit: %d %s", w.Code, w.Body)
	}
	// «сразу» of a title the profile never watched is a pick (the step then refuses it)
	if a, w := e.run(t, call("movie", 1, "", `,"quick":true`), teen); w.Code != 200 || !a.Pick {
		t.Fatalf("quick over the limit: %d %s", w.Code, w.Body)
	}
}

// A source that fails is the web's «Источник не ответил» (502); a source
// with nothing in it is an empty step, never a 404 (that would mean the way
// is gone).
func TestApp_Step_SourceErrors(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("series", 1396, "", ""))
	if w := e.post("/step", call("series", 1396, src.Rows[1].Ref, ""), admin); w.Code != 502 || errOf(w) != "Источник не ответил" {
		t.Fatalf("failing source: %d %s", w.Code, w.Body)
	}
	if w := e.post("/step", call("series", 1396, "bad.ref", ""), admin); w.Code != 400 {
		t.Fatalf("bad ref: %d %s", w.Code, w.Body)
	}
	if w := e.post("/step", call("series", 1396, (appRef{K: refBrowse, R: "forged.ref"}).encode(), ""), admin); w.Code != 400 || errOf(w) != "Неверная ссылка" {
		t.Fatalf("forged online ref: %d %s", w.Code, w.Body)
	}
	if w := e.post("/step", call("series", 1396, (appRef{K: refPlay, R: src.Rows[0].Ref}).encode(), ""), admin); w.Code != 400 {
		t.Fatalf("play ref as step: %d %s", w.Code, w.Body)
	}
}

// --- the position and the close ---

func TestApp_Progress(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("series", 1396, "", ""))
	voices := e.step(t, call("series", 1396, src.Rows[0].Ref, ""))
	seasons := e.step(t, call("series", 1396, voices.Rows[1].Ref, "")) // Многоголосый
	eps := e.step(t, call("series", 1396, seasons.Rows[0].Ref, ""))
	a, w := e.run(t, call("series", 1396, eps.Rows[0].Ref, ""), admin)
	if w.Code != 200 {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	put := func(u *addonkit.User, body string) *httptest.ResponseRecorder {
		return e.do("PUT", app+"/progress", body, u)
	}
	// the app names the session (and a title it may get wrong): the session decides
	body := `{"way":"watch","session":"` + a.Play.Session + `","kind":"movie","tmdbId":438631,"season":9,"episode":9,"positionMs":40000,"durationMs":100000}`
	if w := put(admin, body); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("progress: %d %s", w.Code, w.Body)
	}
	got := e.saved(t, admin, "series", 1396)
	if len(got.Entries) != 1 {
		t.Fatalf("entries: %+v", got.Entries)
	}
	en := got.Entries[0]
	if en.Season != 1 || en.Episode != 1 || en.PositionMs != 40000 || en.DurationMs != 100000 || en.Finished || en.Source != "beta" ||
		en.Voice != "Многоголосый" || en.Quality != "720p" {
		t.Fatalf("entry: %+v", en)
	}
	if m := e.saved(t, admin, "movie", 438631); len(m.Entries) != 0 {
		t.Fatalf("the body's title was recorded: %+v", m.Entries)
	}
	// the end: «finished» makes the position the duration (finished from 92%)
	finish := `{"way":"watch","session":"` + a.Play.Session + `","kind":"series","tmdbId":1396,"positionMs":50000,"durationMs":100000,"finished":true}`
	if w := put(admin, finish); w.Code != 204 {
		t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
	if en := e.saved(t, admin, "series", 1396).Entries[0]; !en.Finished || en.PositionMs != 100000 {
		t.Fatalf("finished: %+v", en)
	}
	// the episode after it, as the source lists it: none after the only episode of this voice
	// (the last episode of the list is «last»: Continue does not offer a next one)
	// another profile's use of the session does not record under it: it falls back to the body
	other := `{"way":"watch","session":"` + a.Play.Session + `","kind":"series","tmdbId":1396,"season":2,"episode":1,"positionMs":1000,"durationMs":100000}`
	if w := put(mama, other); w.Code != 204 {
		t.Fatalf("other profile: %d %s", w.Code, w.Body)
	}
	if got := e.saved(t, mama, "series", 1396); len(got.Entries) != 1 || got.Entries[0].Season != 2 || got.Entries[0].Source != "" {
		t.Fatalf("other profile's entry: %+v", got.Entries)
	}
	if got := e.saved(t, admin, "series", 1396); len(got.Entries) != 1 {
		t.Fatalf("the admin's entries changed: %+v", got.Entries)
	}
	// a session the addon does not know keeps the entry's own choices
	unknown := `{"way":"watch","session":"gone","kind":"series","tmdbId":1396,"season":1,"episode":1,"positionMs":10000,"durationMs":100000}`
	if w := put(admin, unknown); w.Code != 204 {
		t.Fatalf("unknown session: %d %s", w.Code, w.Body)
	}
	if en := e.saved(t, admin, "series", 1396).Entries[0]; en.Source != "beta" || en.Voice != "Многоголосый" || en.Quality != "720p" || en.PositionMs != 10000 {
		t.Fatalf("choices lost: %+v", en)
	}
	// a title that is none, a way that is gone, a rating over the limit
	for name, c := range map[string]struct {
		u    *addonkit.User
		body string
		code int
	}{
		"bad kind":   {admin, `{"way":"watch","session":"gone","kind":"music","tmdbId":1,"positionMs":1,"durationMs":2}`, 400},
		"no id":      {admin, `{"way":"watch","session":"gone","kind":"movie","positionMs":1,"durationMs":2}`, 400},
		"way gone":   {admin, `{"way":"old","session":"gone","kind":"movie","tmdbId":438631,"positionMs":1,"durationMs":2}`, 404},
		"not JSON":   {admin, `nope`, 400},
		"over limit": {teen, `{"way":"watch","session":"gone","kind":"movie","tmdbId":1,"positionMs":1,"durationMs":2}`, 403},
	} {
		if w := put(c.u, c.body); w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

// What the source lists after the episode goes with the entry (the web
// player's nextSeason/nextEpisode/lastListed), so «Продолжить» goes on.
func TestApp_Progress_NextEpisode(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("series", 1396, "", ""))
	voices := e.step(t, call("series", 1396, src.Rows[0].Ref, ""))
	seasons := e.step(t, call("series", 1396, voices.Rows[0].Ref, ""))
	eps := e.step(t, call("series", 1396, seasons.Rows[0].Ref, ""))
	first, w := e.run(t, call("series", 1396, e.step(t, call("series", 1396, eps.Rows[0].Ref, "")).Rows[1].Ref, ""), admin)
	if w.Code != 200 {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	// the stored next episode and whether the source lists none after it, of the newest entry
	stored := func(session string, season, episode int) (ns, ne int, last bool) {
		t.Helper()
		body := `{"way":"watch","session":"` + session + `","kind":"series","tmdbId":1396,"season":` + itoa(season) + `,"episode":` + itoa(episode) +
			`,"positionMs":10000,"durationMs":100000}`
		if w := e.do("PUT", app+"/progress", body, admin); w.Code != 204 {
			t.Fatalf("save: %d %s", w.Code, w.Body)
		}
		var l int
		err := e.db.DB.QueryRow(`SELECT next_season, next_episode, last_listed FROM online_progress WHERE season=? AND episode=?`, season, episode).Scan(&ns, &ne, &l)
		if err != nil {
			t.Fatal(err)
		}
		return ns, ne, l != 0
	}
	// episode 1: the source lists episode 2 after it (the DASH one after that is listed too)
	if ns, ne, last := stored(first.Play.Session, 1, 1); ns != 1 || ne != 2 || last {
		t.Fatalf("after the first: %d %d %v", ns, ne, last)
	}
	// «Пилот» (episode 2) is followed by the listed third
	second, w := e.run(t, call("series", 1396, "", `,"quick":true,"season":1,"episode":2`), admin)
	if w.Code != 200 {
		t.Fatalf("second: %d %s", w.Code, w.Body)
	}
	if ns, ne, last := stored(second.Play.Session, 1, 2); ns != 1 || ne != 3 || last {
		t.Fatalf("after the second: %d %d %v", ns, ne, last)
	}
	// season 2 lists a single episode: none after it
	third, w := e.run(t, call("series", 1396, "", `,"quick":true,"season":2,"episode":1`), admin)
	if w.Code != 200 {
		t.Fatalf("third: %d %s", w.Code, w.Body)
	}
	if ns, ne, last := stored(third.Play.Session, 2, 1); ns != 0 || ne != 0 || !last {
		t.Fatalf("after the last: %d %d %v", ns, ne, last)
	}
}

func TestApp_Close(t *testing.T) {
	e := newAppEnv(t)
	src := e.step(t, call("movie", 438631, "", ""))
	files := e.step(t, call("movie", 438631, src.Rows[0].Ref, ""))
	a, w := e.run(t, call("movie", 438631, files.Rows[1].Ref, ""), admin)
	if w.Code != 200 {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	del := func(u *addonkit.User) *httptest.ResponseRecorder {
		return e.do("DELETE", app+"/play/"+a.Play.Session, "", u)
	}
	// not another profile's
	if w := del(mama); w.Code != 404 {
		t.Fatalf("other profile: %d %s", w.Code, w.Body)
	}
	if g := e.do("GET", a.Play.URL, "", admin); g.Code != 200 {
		t.Fatalf("the session died of another's close: %d", g.Code)
	}
	if w := del(admin); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("close: %d %s", w.Code, w.Body)
	}
	if g := e.do("GET", a.Play.URL, "", admin); g.Code != 404 {
		t.Fatalf("after the close: %d", g.Code)
	}
	if w := del(admin); w.Code != 404 || errOf(w) != "Сеанс не найден" {
		t.Fatalf("closed twice: %d %s", w.Code, w.Body)
	}
	// the session's memory went with it: a late position keeps the entry's choices (none: empty)
	late := `{"way":"watch","session":"` + a.Play.Session + `","kind":"movie","tmdbId":438631,"positionMs":1000,"durationMs":100000}`
	if w := e.do("PUT", app+"/progress", late, admin); w.Code != 204 {
		t.Fatalf("late position: %d %s", w.Code, w.Body)
	}
	if en := e.saved(t, admin, "movie", 438631).Entries[0]; en.Source != "" {
		t.Fatalf("the closed session still decided: %+v", en)
	}
}

func TestApp_State(t *testing.T) {
	e := newAppEnv(t)
	if w := e.get("/state?kind=movie&tmdbId=1", mama); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"ways":{}}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// --- the memory of the plays ---

func TestAppSessions(t *testing.T) {
	s := &appSessions{}
	var at int64
	s.now = func() time.Time { return time.Unix(at, 0) }
	s.put("a", appSession{user: 1, kind: "movie"})
	if _, ok := s.get("a", 1); !ok {
		t.Fatal("not kept")
	}
	if _, ok := s.get("a", 2); ok {
		t.Fatal("another profile's session")
	}
	s.drop("a", 2)
	if _, ok := s.get("a", 1); !ok {
		t.Fatal("dropped by another profile")
	}
	at = int64(appSessionLife/time.Second) + 1
	if _, ok := s.get("a", 1); ok {
		t.Fatal("kept past its life")
	}
	at = 0
	for i := 0; i < maxAppSessions+10; i++ {
		at++
		s.put(strconv.Itoa(i), appSession{user: 1})
	}
	if len(s.m) > maxAppSessions {
		t.Fatalf("%d kept", len(s.m))
	}
	if _, ok := s.get(strconv.Itoa(maxAppSessions+9), 1); !ok {
		t.Fatal("the newest is gone")
	}
	if _, ok := s.get("a", 1); ok {
		t.Fatal("the oldest stayed")
	}
	s.drop("5", 1)
	if _, ok := s.get("5", 1); ok {
		t.Fatal("not dropped")
	}
}
