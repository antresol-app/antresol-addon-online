package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-online/internal/coretest"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol-addon-online/internal/store"
	_ "github.com/antresol-app/antresol-addon-online/internal/strings"
	"github.com/antresol-app/antresol/addonkit"
)

// The profiles of the tests, as the core's X-Antresol-User and GET /users
// carry them.
var (
	teen = addonkit.User{ID: 2, UID: "uid-teen", Name: "Петя", Lang: "ru", MaxRating: "12+"}
	mama = addonkit.User{ID: 3, UID: "uid-mama", Name: "Мама", Lang: "ru"}
	kid  = addonkit.User{ID: 4, UID: "uid-kid", Name: "Малыш", IsKid: true, Lang: "ru", MaxRating: "6+"}
)

var t0 = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

// hookEnv is the addon's hooks over its own database and a fake core.
type hookEnv struct {
	t    *testing.T
	db   *store.DB
	core *coretest.Core
	m    *addonkit.HookMux
}

func newHookEnv(t *testing.T, d Deps) *hookEnv {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	core := &coretest.Core{Profiles: []addonkit.User{teen, mama, kid}}
	if d.DB == nil {
		d.DB = db.DB
	}
	if d.Users == nil {
		d.Users = core
	}
	if d.Progress == nil {
		d.Progress = &online.ProgressStore{DB: db.DB, Lib: core, Progress: core, Users: core}
	}
	m := &addonkit.HookMux{}
	Register(m, d)
	return &hookEnv{t: t, db: db, core: core, m: m}
}

// save stores an online position of u (no mirror into the library).
func (e *hookEnv) save(u addonkit.User, en online.Entry) {
	e.t.Helper()
	if en.At.IsZero() {
		en.At = t0
	}
	if err := (&online.ProgressStore{DB: e.db.DB}).Save(context.Background(), u, en); err != nil {
		e.t.Fatal(err)
	}
}

// call posts hook name with body as u (nil: no header).
func (e *hookEnv) call(name string, u *addonkit.User, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest("POST", "/hooks/"+name, strings.NewReader(body))
	if u != nil {
		b, _ := json.Marshal(u)
		r.Header.Set(addonkit.UserHeader, string(b))
	}
	w := httptest.NewRecorder()
	e.m.ServeHTTP(w, r)
	return w
}

func (e *hookEnv) cards(u *addonkit.User) []addonkit.ContinueCard {
	e.t.Helper()
	w := e.call("home.continue", u, `{}`)
	var a addonkit.ContinueAnswer
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Cards == nil {
		e.t.Fatalf("home.continue %d %s", w.Code, w.Body)
	}
	return a.Cards
}

func (e *hookEnv) inLibrary(uid, kind string, tmdbID int) bool {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT in_library FROM online_progress WHERE user_uid=? AND kind=? AND tmdb_id=?`, uid, kind, tmdbID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

func (e *hookEnv) rows(uid string) int {
	var n int
	e.db.QueryRow(`SELECT count(*) FROM online_progress WHERE user_uid=?`, uid).Scan(&n)
	return n
}

func movie(id int, rating string, posMs int64) online.Entry {
	return online.Entry{Kind: "movie", TMDBID: id, PositionMs: posMs, DurationMs: 6_000_000, Title: "Фильм", Rating: rating, Year: 2021}
}

// TestHomeContinue_AddonAppliesLimit: «Продолжить» from the snapshot,
// gated per card by the header's maxRating (an unrated title is left out
// for a limited profile), none for a kid; the card's href opens the
// addon's player where the profile stopped.
func TestHomeContinue_AddonAppliesLimit(t *testing.T) {
	e := newHookEnv(t, Deps{})
	for _, u := range []addonkit.User{teen, mama} {
		e.save(u, movie(438631, "12+", 600_000))
		e.save(u, movie(1, "18+", 600_000))
		e.save(u, movie(2, "", 600_000))
		e.save(u, online.Entry{Kind: "series", TMDBID: 3, Season: 1, Episode: 2, PositionMs: 600_500, DurationMs: 2_400_000,
			Title: "Сериал", Rating: "12+", Year: 2020, Poster: "0123456789abcdef0123456789abcdef01234567.jpg", At: t0.Add(time.Minute)})
	}
	// saved without a title snapshot: no card (no TMDB call to name it)
	e.save(mama, online.Entry{Kind: "movie", TMDBID: 5, PositionMs: 1000, DurationMs: 6_000_000})

	got := e.cards(&teen)
	if len(got) != 2 || got[0].Key != "series:3" || got[1].Key != "movie:438631" {
		t.Fatalf("teen %+v", got)
	}
	s := got[0]
	if s.Kind != "series" || s.TMDBID != 3 || s.Title != "Сериал" || s.Subtitle != "Сезон 1, серия 2" || s.Year != 2020 ||
		s.Rating != "12+" || s.Poster != "0123456789abcdef0123456789abcdef01234567.jpg" || s.Season != 1 || s.Episode != 2 ||
		s.ResumeSec != 600.5 || s.Progress != 600_500.0/2_400_000 || !s.UpdatedAt.Equal(t0.Add(time.Minute)) ||
		s.Href != "/addons/online/play/series/3?s=1&e=2&t=600" {
		t.Fatalf("series card %+v", s)
	}
	if m := got[1]; m.Href != "/addons/online/play/movie/438631?t=600" || m.Subtitle != "" || m.Season != 0 {
		t.Fatalf("movie card %+v", m)
	}
	if got := e.cards(&mama); len(got) != 4 {
		t.Fatalf("mama %+v", got)
	}
	en := mama
	en.Lang = "en"
	if got := e.cards(&en); got[0].Subtitle != "Season 1, Episode 2" {
		t.Fatalf("en %+v", got[0])
	}
	if got := e.cards(&kid); len(got) != 0 {
		t.Fatalf("kid %+v", got)
	}
	if got := e.cards(nil); len(got) != 0 {
		t.Fatalf("no profile %+v", got)
	}
	// off (the setting, or no Lampac): no cards
	off := newHookEnv(t, Deps{Available: func() bool { return false }})
	off.save(mama, movie(438631, "12+", 600_000))
	if got := off.cards(&mama); len(got) != 0 {
		t.Fatalf("off %+v", got)
	}
}

// countingCatalog is a TMDB that counts its calls.
type countingCatalog struct{ n atomic.Int32 }

func (c *countingCatalog) GenreNames(context.Context, string) (map[int]string, error) {
	c.n.Add(1)
	return nil, nil
}
func (c *countingCatalog) DiscoverMovies(context.Context, tmdb.DiscoverQuery) ([]tmdb.DiscoverResult, error) {
	c.n.Add(1)
	return nil, nil
}
func (c *countingCatalog) Movie(context.Context, int) (tmdb.Details, error) {
	c.n.Add(1)
	return tmdb.Details{}, nil
}

// TestHomeContinue_NoUpstreamCalls: «Продолжить» answers from the stored
// snapshot alone: no Lampac call, no TMDB call (the core's 500 ms).
func TestHomeContinue_NoUpstreamCalls(t *testing.T) {
	var lampac, titles atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lampac.Add(1)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	l, err := online.NewLampac(srv.URL + "/?uid=secret")
	if err != nil {
		t.Fatal(err)
	}
	svc := &online.Service{L: l, Refs: online.NewRefs([]byte("0123456789abcdef0123456789abcdef"), l.Base),
		Titles: func(context.Context, string, int) (online.TitleCtx, error) {
			titles.Add(1)
			return online.TitleCtx{Title: "x", Rating: "12+"}, nil
		}}
	cat := &countingCatalog{}
	e := newHookEnv(t, Deps{Scanner: online.NewTrackingScanner(svc, nil, online.User{ID: -1}, 0),
		Channels: &online.ChannelExtras{Online: svc, TMDB: cat}})
	e.save(mama, movie(438631, "12+", 600_000))
	e.save(mama, online.Entry{Kind: "series", TMDBID: 3, Season: 1, Episode: 1, PositionMs: 1, DurationMs: 10, Title: "С", Rating: "12+"})
	if got := e.cards(&mama); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if lampac.Load() != 0 || titles.Load() != 0 || cat.n.Load() != 0 {
		t.Fatalf("upstream calls: Lampac %d, titles %d, TMDB %d", lampac.Load(), titles.Load(), cat.n.Load())
	}
}

// TestAfterScan_SeedsOnce: a title watched online that reached the library
// gets its online position there, once.
func TestAfterScan_SeedsOnce(t *testing.T) {
	e := newHookEnv(t, Deps{})
	id := e.core.AddMovie(438631)
	e.save(mama, movie(438631, "12+", 600_000))
	for range 2 {
		if w := e.call("library.afterScan", nil, `{}`); w.Code != 204 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	if len(e.core.Saves) != 1 {
		t.Fatalf("saves %+v", e.core.Saves)
	}
	s := e.core.Saves[0]
	if s.UserID != mama.ID || s.ItemID != id || s.PositionMs != 600_000 || s.DurationMs != 6_000_000 || !s.At.Equal(t0) || s.Played {
		t.Fatalf("save %+v", s)
	}
	if !e.inLibrary(mama.UID, "movie", 438631) {
		t.Fatal("not marked")
	}
}

// TestAfterScan_RefusedRowSkippedNotRetried: a row whose item the core
// refuses (404: over the profile's limit; 400: not a movie or an episode)
// is counted, the rest of the seed goes on, the refused rows are marked
// and not sent on the next scan; one log line per scan. A kid's row is
// never sent and stays unmarked.
func TestAfterScan_RefusedRowSkippedNotRetried(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e := newHookEnv(t, Deps{})
	hidden, notVideo := e.core.AddMovie(1), e.core.AddMovie(2)
	ok := e.core.AddMovie(438631)
	e.core.Hidden = map[int64]bool{hidden: true}
	e.core.NotVideo = map[int64]bool{notVideo: true}
	e.save(mama, movie(1, "18+", 1000))
	e.save(mama, movie(2, "12+", 1000))
	e.save(mama, movie(438631, "12+", 1000))
	e.save(kid, movie(438631, "12+", 1000))

	if w := e.call("library.afterScan", nil, `{}`); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(e.core.Saves) != 1 || e.core.Saves[0].ItemID != ok || e.core.Saves[0].UserID != mama.ID {
		t.Fatalf("saves %+v", e.core.Saves)
	}
	if strings.Count(logs.String(), "\n") != 1 || !strings.Contains(logs.String(), "2 rows the library refused") {
		t.Fatalf("log %q", logs.String())
	}
	for _, id := range []int{1, 2, 438631} {
		if !e.inLibrary(mama.UID, "movie", id) {
			t.Fatalf("mama's %d not marked", id)
		}
	}
	if e.inLibrary(kid.UID, "movie", 438631) {
		t.Fatal("the kid's row was marked")
	}
	logs.Reset()
	if w := e.call("library.afterScan", nil, `{}`); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(e.core.Saves) != 1 || logs.Len() != 0 {
		t.Fatalf("second scan: saves %d, log %q", len(e.core.Saves), logs.String())
	}
}

// TestAfterScan_SeedsEpisodeByItemID: an episode is saved with the item id
// the core's EpisodeItems gives the profile; one the library lacks is not.
func TestAfterScan_SeedsEpisodeByItemID(t *testing.T) {
	e := newHookEnv(t, Deps{})
	e.core.AddEpisode(77, 1, 1)
	id := e.core.AddEpisode(77, 1, 2)
	ep := func(n int) online.Entry {
		return online.Entry{Kind: "series", TMDBID: 77, Season: 1, Episode: n, PositionMs: 5000, DurationMs: 60_000, Title: "С"}
	}
	e.save(mama, ep(2))
	e.save(mama, ep(3))
	if w := e.call("library.afterScan", nil, `{}`); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(e.core.Saves) != 1 || e.core.Saves[0].ItemID != id || e.core.Saves[0].PositionMs != 5000 {
		t.Fatalf("saves %+v", e.core.Saves)
	}
}

// TestAfterScan_DropsRowsOfDeletedProfiles: after the seed, the rows of
// every uid GET /users no longer lists go; a /users error skips that.
func TestAfterScan_DropsRowsOfDeletedProfiles(t *testing.T) {
	e := newHookEnv(t, Deps{})
	gone := addonkit.User{ID: 9, UID: "uid-gone", Name: "Ушёл"}
	e.save(mama, movie(438631, "12+", 1000))
	e.save(gone, movie(438631, "12+", 1000))
	e.core.UsersFail = errors.New("core down")
	e.call("library.afterScan", nil, `{}`)
	if e.rows(gone.UID) != 1 || e.rows(mama.UID) != 1 {
		t.Fatal("a /users error dropped rows")
	}
	e.core.UsersFail = nil
	if w := e.call("library.afterScan", nil, `{}`); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if e.rows(gone.UID) != 0 || e.rows(mama.UID) != 1 {
		t.Fatalf("rows: gone %d, mama %d", e.rows(gone.UID), e.rows(mama.UID))
	}
}

// TestTrackingWatched_Shape: the profile (by the body's userId) of a
// title: every episode it finished online (watched) or stopped in
// (positionSec), in the hook's answer shape; 204 for a profile the
// server does not list.
func TestTrackingWatched_Shape(t *testing.T) {
	e := newHookEnv(t, Deps{})
	ep := func(n int, pos int64) online.Entry {
		return online.Entry{Kind: "series", TMDBID: 42, Season: 1, Episode: n, PositionMs: pos, DurationMs: 100_000, Title: "С"}
	}
	e.save(mama, ep(1, 95_000))
	e.save(mama, ep(2, 90_500))
	e.save(mama, ep(3, 0))
	e.save(teen, ep(4, 50_000))
	e.save(mama, movie(7, "", 6_000_000))
	w := e.call("tracking.watched", nil, `{"userId":3,"kind":"series","tmdbId":42}`)
	want := `{"episodes":[{"season":1,"number":1,"watched":true,"positionSec":0},{"season":1,"number":2,"watched":false,"positionSec":90}]}`
	if w.Code != 200 || w.Body.String() != want {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = e.call("tracking.watched", nil, `{"userId":3,"kind":"movie","tmdbId":7}`)
	if want := `{"episodes":[{"season":0,"number":0,"watched":true,"positionSec":0}]}`; w.Code != 200 || w.Body.String() != want {
		t.Fatalf("movie %d %s", w.Code, w.Body)
	}
	if w := e.call("tracking.watched", nil, `{"userId":99,"kind":"series","tmdbId":42}`); w.Code != 204 {
		t.Fatalf("unknown profile %d %s", w.Code, w.Body)
	}
	if w := e.call("tracking.watched", nil, `{"userId":3,"kind":"album","tmdbId":42}`); w.Code != 500 {
		t.Fatalf("bad kind %d %s", w.Code, w.Body)
	}
}

// TestTrackingScan_Unchanged: without «Смотреть онлайн» (no Lampac) the
// scan keeps the stored rows; a body without a title is a failure.
func TestTrackingScan_Unchanged(t *testing.T) {
	e := newHookEnv(t, Deps{})
	w := e.call("tracking.scan:online", nil, `{"title":{"id":5,"kind":"series","tmdbId":42},"episodes":[]}`)
	if w.Code != 200 || w.Body.String() != `{"unchanged":true}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.call("tracking.scan:online", nil, `{}`); w.Code != 500 {
		t.Fatalf("no title %d", w.Code)
	}
}

// channelFake is a synthetic Lampac (lampac.test:9118) and CDN
// (cdn.example) for the channels' streams, reached through a client that
// maps those names to one test server.
func channelFake(t *testing.T) *online.ChannelExtras {
	t.Helper()
	body := "\x00\x00\x00\x18ftypmp42" + strings.Repeat("m", 600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host == "cdn.example":
			w.Write([]byte(body))
		case r.URL.Path == "/externalids":
			w.Write([]byte(`{}`))
		case r.URL.Path == "/lite/events":
			w.Write([]byte(`[{"name":"alpha","url":"http://lampac.test:9118/lite/alpha","show":true,"balanser":"alpha"}]`))
		case r.URL.Path == "/lite/alpha":
			w.Write([]byte(`{"type":"movie","data":[{"method":"play","translate":"Дубляж","url":"http://cdn.example/a/1080.mp4"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}
	l, err := online.NewLampac("http://lampac.test:9118/?uid=secret")
	if err != nil {
		t.Fatal(err)
	}
	l.HTTP = client
	svc := &online.Service{L: l, Refs: online.NewRefs([]byte("0123456789abcdef0123456789abcdef"), l.Base), Upstream: client,
		Proxy: &online.Proxy{Key: []byte("proxy-key-0123456789abcdef012345"), HTTP: client, Lampac: l, Base: online.StreamBase},
		Titles: func(context.Context, string, int) (online.TitleCtx, error) {
			return online.TitleCtx{Title: "Дюна", Rating: "12+", Year: 2021}, nil
		}}
	return &online.ChannelExtras{Online: svc, TMDB: &countingCatalog{}}
}

// socket is the addon's socket as the core reaches it: the hooks and the
// channels' stream routes (main mounts both the same way).
func socket(e *hookEnv, x *online.ChannelExtras) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/", e.m)
	mux.Handle(online.ChannelPrefix, x)
	return mux
}

func (e *hookEnv) resolve(h http.Handler) string {
	e.t.Helper()
	r := httptest.NewRequest("POST", "/hooks/channels.resolve",
		strings.NewReader(`{"channelId":5,"title":{"key":"tmdb:movie:438631","kind":"movie","tmdbId":438631,"title":"Дюна","year":2021,"runtimeMs":9000000}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var st map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || len(st) != 1 {
		e.t.Fatalf("resolve %d %s", w.Code, w.Body)
	}
	p, _ := st["path"].(string)
	return p
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// TestChannels_ResolvePathOnSocket: channels.resolve gives a route on the
// addon's socket under /ch/<token>/ (no headers, no loopback URL), and the
// socket serves the stream there, as the core's relay reads it; ready and
// candidates answer in their shapes.
func TestChannels_ResolvePathOnSocket(t *testing.T) {
	x := channelFake(t)
	e := newHookEnv(t, Deps{Channels: x})
	h := socket(e, x)
	p := e.resolve(h)
	rest, ok := strings.CutPrefix(p, online.ChannelPrefix)
	token, blob, _ := strings.Cut(rest, "/")
	if !ok || len(token) != 32 || blob == "" || strings.Contains(blob, "/") {
		t.Fatalf("path %q", p)
	}
	if w := get(h, p); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "\x00\x00\x00\x18ftyp") {
		t.Fatalf("stream %d %q", w.Code, w.Body.String()[:min(w.Body.Len(), 40)])
	}
	if w := e.call("channels.ready", nil, `{}`); w.Code != 200 || w.Body.String() != `{"ready":true}` {
		t.Fatalf("ready %d %s", w.Code, w.Body)
	}
	w := e.call("channels.candidates", nil, `{"rules":{"genres":[]},"limit":5}`)
	if w.Code != 200 || w.Body.String() != `{"titles":[]}` {
		t.Fatalf("candidates %d %s", w.Code, w.Body)
	}
	// without channel extras (no Lampac or TMDB): not ready, nothing
	none := newHookEnv(t, Deps{})
	if w := none.call("channels.ready", nil, `{}`); w.Body.String() != `{"ready":false}` {
		t.Fatalf("ready without %s", w.Body)
	}
	if w := none.call("channels.candidates", nil, `{"rules":{},"limit":5}`); w.Body.String() != `{"titles":[]}` {
		t.Fatalf("candidates without %s", w.Body)
	}
	if w := none.call("channels.resolve", nil, `{"channelId":5,"title":{"key":"k","kind":"movie","tmdbId":1}}`); w.Code != 500 {
		t.Fatalf("resolve without %d", w.Code)
	}
}

// TestChannels_ReleaseClosesSession: channels.release of the resolved path
// ends the stream: the socket route is gone (404); a second release, or
// one of a path never resolved, is still 204.
func TestChannels_ReleaseClosesSession(t *testing.T) {
	x := channelFake(t)
	e := newHookEnv(t, Deps{Channels: x})
	h := socket(e, x)
	p := e.resolve(h)
	if w := get(h, p); w.Code != 200 {
		t.Fatalf("before %d", w.Code)
	}
	body := `{"token":"0123456789abcdef0123456789abcdef","path":` + strings.TrimSpace(mustJSON(p)) + `}` // gitleaks:allow (a test token)
	for range 2 {
		if w := e.call("channels.release", nil, body); w.Code != 204 {
			t.Fatalf("release %d %s", w.Code, w.Body)
		}
	}
	if w := get(h, p); w.Code != 404 {
		t.Fatalf("after release %d", w.Code)
	}
	if w := e.call("channels.release", nil, `{"token":"x","path":"/ch/nothing/here"}`); w.Code != 204 {
		t.Fatalf("unknown %d", w.Code)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestTrackingWatched_ReusedIDNeverGetsOldData: a profile id the server
// gives again after a deletion (users.id is reused) is read anew on every
// call: the new profile never gets the deleted one's states, even right
// after them.
func TestTrackingWatched_ReusedIDNeverGetsOldData(t *testing.T) {
	e := newHookEnv(t, Deps{})
	e.save(mama, online.Entry{Kind: "series", TMDBID: 42, Season: 1, Episode: 1, PositionMs: 95_000, DurationMs: 100_000, Title: "С"})
	body := `{"userId":3,"kind":"series","tmdbId":42}`
	if w := e.call("tracking.watched", nil, body); w.Code != 200 || !strings.Contains(w.Body.String(), `"watched":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// mama is deleted; a new kid gets her id at once
	e.core.Profiles = []addonkit.User{teen, {ID: 3, UID: "uid-new", Name: "Новый", IsKid: true}}
	if w := e.call("tracking.watched", nil, body); w.Code != 204 {
		t.Fatalf("reused id as a kid: %d %s", w.Code, w.Body)
	}
	e.core.Profiles = []addonkit.User{teen, {ID: 3, UID: "uid-new", Name: "Новый"}}
	if w := e.call("tracking.watched", nil, body); w.Code != 200 || w.Body.String() != `{"episodes":[]}` {
		t.Fatalf("reused id: %d %s", w.Code, w.Body)
	}
}

// blockingUsers is GET /users whose first call waits for release.
type blockingUsers struct {
	core    *coretest.Core
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func (b *blockingUsers) Users(ctx context.Context) ([]addonkit.User, error) {
	b.calls.Add(1)
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.core.Users(ctx)
}

// TestAfterScan_OverlappingCallRepeatsThePass: a library.afterScan that
// arrives while one runs answers at once, and the running one makes one
// more pass after its own (the scan it reports is not lost).
func TestAfterScan_OverlappingCallRepeatsThePass(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	core := &coretest.Core{Profiles: []addonkit.User{mama}}
	bu := &blockingUsers{core: core, entered: make(chan struct{}), release: make(chan struct{})}
	e := newHookEnv(t, Deps{DB: db.DB, Users: bu, Progress: &online.ProgressStore{DB: db.DB, Lib: core, Progress: core, Users: bu}})
	e.db, e.core = db, core
	done := make(chan int)
	go func() { done <- e.call("library.afterScan", nil, `{}`).Code }()
	<-bu.entered
	// the scan's movie and its online row arrive while the first pass runs
	id := core.AddMovie(438631)
	e.save(mama, movie(438631, "12+", 600_000))
	if w := e.call("library.afterScan", nil, `{}`); w.Code != 204 {
		t.Fatalf("overlapping %d", w.Code)
	}
	close(bu.release)
	if code := <-done; code != 204 {
		t.Fatalf("first %d", code)
	}
	if len(core.Saves) != 1 || core.Saves[0].ItemID != id {
		t.Fatalf("the overlapping scan's pass was lost: saves %+v", core.Saves)
	}
	if n := bu.calls.Load(); n != 4 { // two passes: seed + clean-up each
		t.Fatalf("/users calls %d, want 4", n)
	}
}
