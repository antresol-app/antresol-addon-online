package online

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// level parses a Lampac answer as a source's level.
func level(t *testing.T, body string) Level {
	t.Helper()
	lv, err := parseLevel([]byte(body), RefContext{})
	if err != nil {
		t.Fatal(err)
	}
	return lv
}

const (
	voiceLinks = `"voice":[{"method":"link","url":"http://lampac.test:9118/lite/x?t=1","active":true,"name":"Дубляж"},` +
		`{"method":"link","url":"http://lampac.test:9118/lite/x?t=2","active":false,"name":"Оригинал"}]`
	ruEn = `"subtitles":[{"label":"Русские","url":"http://cdn.example/ru.srt"},{"label":"English","url":"http://cdn.example/en.srt"}]`
)

func movieItem(voice, qualities, extra string) string {
	return `{"method":"play","url":"http://cdn.example/v.m3u8","quality":{` + qualities + `},"translate":"` + voice + `"` + extra + `}`
}

func TestOfferOf_MovieNamesVoicesAndSubtitles(t *testing.T) {
	lv := level(t, `{"type":"movie",`+voiceLinks+`,"data":[`+
		movieItem("Дубляж", `"1080p":"http://cdn.example/a","720p":"http://cdn.example/b"`, `,`+ruEn)+`,`+
		movieItem("Оригинал", `"720p":"http://cdn.example/c"`, "")+`]}`)
	o, ok := OfferOf(lv, nil)
	if !ok {
		t.Fatal("a movie level has an offer")
	}
	if strings.Join(o.Voices, "|") != "Дубляж|Оригинал" || !o.Subs || strings.Join(o.SubLangs, "|") != "RU|EN" || o.Quality != "1080p" {
		t.Fatalf("%+v", o)
	}
}

// Without the level's own voice list, the voices are those of its files
// (a file a source cannot play, or one that names none, adds none).
func TestOfferOf_VoicesFromFiles(t *testing.T) {
	lv := level(t, `{"type":"movie","data":[`+
		movieItem("Дубляж", `"720p":"http://cdn.example/a"`, "")+`,`+
		movieItem("Дубляж", `"480p":"http://cdn.example/b"`, "")+`,`+
		`{"method":"play","url":"http://cdn.example/v.mpd","quality":{"1080p":"http://cdn.example/v.mpd"},"translate":"DASH"}]}`)
	o, ok := OfferOf(lv, nil)
	if !ok || strings.Join(o.Voices, "|") != "Дубляж" || o.Subs || len(o.SubLangs) != 0 || o.Quality != "720p" {
		t.Fatalf("%+v %v", o, ok)
	}
}

// A series' seasons say nothing of subtitles: they come from its first
// season, whose voices stand in where the seasons list none.
func TestOfferOf_SeriesFromItsFirstSeason(t *testing.T) {
	seasons := level(t, `{"type":"season","data":[{"method":"link","id":1,"url":"http://lampac.test:9118/lite/x?s=1","name":"1 сезон"}]}`)
	season := level(t, `{"type":"episode",`+voiceLinks+`,"data":[`+
		`{"method":"play","url":"http://cdn.example/e1","quality":{"1080p":"http://cdn.example/e1"},"s":1,"e":1,"name":"Серия 1",`+ruEn+`},`+
		`{"method":"play","url":"http://cdn.example/e2","quality":{"720p":"http://cdn.example/e2"},"s":1,"e":2,"name":"Серия 2"}]}`)
	o, ok := OfferOf(seasons, nil)
	if !ok || o.Subs || len(o.Voices) != 0 || o.Quality != "" {
		t.Fatalf("seasons alone: %+v %v", o, ok)
	}
	o, ok = OfferOf(seasons, &season)
	if !ok || strings.Join(o.Voices, "|") != "Дубляж|Оригинал" || !o.Subs || strings.Join(o.SubLangs, "|") != "RU|EN" || o.Quality != "1080p" {
		t.Fatalf("with the first season: %+v %v", o, ok)
	}
}

func TestOfferOf_PickLevelHasNone(t *testing.T) {
	pick := level(t, `{"type":"similar","data":[{"method":"link","url":"http://lampac.test:9118/lite/x?href=a","title":"Фильм A","year":2021}]}`)
	if o, ok := OfferOf(pick, nil); ok {
		t.Fatalf("%+v", o)
	}
}

// Subtitles that name no language still count; each language is listed once.
func TestSubLangsOf(t *testing.T) {
	got := SubLangsOf([]SubInfo{{Label: "Русские", Lang: "ru"}, {Label: "Forced", Lang: ""}, {Label: "English", Lang: "en"}, {Label: "ru again", Lang: " ru "}})
	if strings.Join(got, "|") != "RU|EN" {
		t.Fatalf("%q", got)
	}
	if got := SubLangsOf(nil); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

// --- the service ---

// offerLampac is a Lampac with sources s1…sN (a film each, answering after
// delay), `ser` (a series), `pick` (titles to pick from) and `bad` (a failing
// one); it counts the requests and the most that were open at once.
type offerLampac struct {
	delay    atomic.Int64 // ns
	open     atomic.Int32
	maxOpen  atomic.Int32
	mu       sync.Mutex
	requests map[string]int // by path and season
}

func (f *offerLampac) hits(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[key]
}

func (f *offerLampac) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.requests {
		n += c
	}
	return n
}

func (f *offerLampac) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.URL.Path == "/externalids" {
		w.Write([]byte(`{"imdb_id":"tt1","kinopoisk_id":1}`))
		return
	}
	if r.URL.Path == "/lite/events" {
		var evs []string
		for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "ser", "pick", "bad"} {
			evs = append(evs, fmt.Sprintf(`{"name":"%s - 1080p","url":"http://lampac.test:9118/lite/%s","show":true,"balanser":"%s"}`, id, id, id))
		}
		w.Write([]byte("[" + strings.Join(evs, ",") + "]"))
		return
	}
	key := r.URL.Path
	if s := q.Get("s"); s != "" {
		key += "?s=" + s
	}
	f.mu.Lock()
	if f.requests == nil {
		f.requests = map[string]int{}
	}
	f.requests[key]++
	f.mu.Unlock()
	n := f.open.Add(1)
	defer f.open.Add(-1)
	for {
		m := f.maxOpen.Load()
		if n <= m || f.maxOpen.CompareAndSwap(m, n) {
			break
		}
	}
	if d := time.Duration(f.delay.Load()); d > 0 {
		time.Sleep(d)
	}
	switch id := strings.TrimPrefix(r.URL.Path, "/lite/"); {
	case id == "bad":
		w.WriteHeader(503)
	case id == "pick":
		w.Write([]byte(`{"type":"similar","data":[{"method":"link","url":"http://lampac.test:9118/lite/pick?href=a","title":"A","year":1990},` +
			`{"method":"link","url":"http://lampac.test:9118/lite/pick?href=b","title":"B","year":1991}]}`))
	case id == "ser" && q.Get("s") == "":
		w.Write([]byte(`{"type":"season","data":[{"method":"link","id":1,"url":"http://lampac.test:9118/lite/ser?s=1","name":"1 сезон"}]}`))
	case id == "ser":
		w.Write([]byte(`{"type":"episode","voice":[{"method":"link","url":"http://lampac.test:9118/lite/ser?s=1&t=1","active":true,"name":"Дубляж"}],"data":[` +
			`{"method":"play","url":"http://cdn.example/e1","quality":{"720p":"http://cdn.example/e1"},"s":1,"e":1,"name":"Серия 1",` + ruEn + `}]}`))
	default: // a film with a different voice-over per source
		w.Write([]byte(`{"type":"movie","data":[` + movieItem("Озвучка "+id, `"720p":"http://cdn.example/a"`, "") + `]}`))
	}
}

func offerService(t *testing.T) (*Service, *offerLampac, []Source) {
	t.Helper()
	f := &offerLampac{}
	s := fakeService(t, f, dune)
	t.Cleanup(s.WaitOffers) // before the fake goes
	srcs, err := s.Sources(context.Background(), adult(), "movie", 438631)
	if err != nil || len(srcs) != 9 {
		t.Fatalf("%d sources: %v", len(srcs), err)
	}
	return s, f, srcs
}

func voiceNames(o Offer) string { return strings.Join(o.Voices, "|") }

func TestOffers_ReadEverySource(t *testing.T) {
	s, f, srcs := offerService(t)
	got := s.Offers(context.Background(), adult(), "movie", 438631, srcs, 5*time.Second)
	// the pick and the failing sources have none
	if len(got) != 7 || voiceNames(got["s1"]) != "Озвучка s1" || voiceNames(got["s6"]) != "Озвучка s6" {
		t.Fatalf("%+v", got)
	}
	if o := got["ser"]; voiceNames(o) != "Дубляж" || !o.Subs || strings.Join(o.SubLangs, "|") != "RU|EN" || o.Quality != "720p" {
		t.Fatalf("series: %+v", o)
	}
	if _, ok := got["pick"]; ok {
		t.Fatalf("pick: %+v", got["pick"])
	}
	if _, ok := got["bad"]; ok {
		t.Fatalf("bad: %+v", got["bad"])
	}
	if f.hits("/lite/ser") != 1 || f.hits("/lite/ser?s=1") != 1 {
		t.Fatalf("the series was asked for its seasons and its first season: %v", f.requests)
	}
}

// The lookups are bounded (a profile's two Lampac slots), the step does not
// wait for slow sources beyond its budget, and what was asked in that time is
// not asked again: the next call finds it.
func TestOffers_BoundedBudgetAndCached(t *testing.T) {
	s, f, srcs := offerService(t)
	f.delay.Store(int64(150 * time.Millisecond))
	start := time.Now()
	first := s.Offers(context.Background(), adult(), "movie", 438631, srcs, 100*time.Millisecond)
	if d := time.Since(start); d > 140*time.Millisecond {
		t.Fatalf("waited %v for a 100 ms budget", d)
	}
	if len(first) != 0 {
		t.Fatalf("nothing is ready after 100 ms: %+v", first)
	}
	s.WaitOffers() // the background lookups end
	if m := f.maxOpen.Load(); m > offerWorkers {
		t.Fatalf("%d lookups at once, the limit is %d", m, offerWorkers)
	}
	asked := f.total()
	f.delay.Store(0)
	second := s.Offers(context.Background(), adult(), "movie", 438631, srcs, 5*time.Second)
	if len(second) != 7 {
		t.Fatalf("the second call is complete: %+v", second)
	}
	if f.total() > asked {
		t.Fatalf("asked %d requests more", f.total()-asked)
	}
}

// After the step has answered, the lookups in flight end and one at a time
// goes on in the background (the others would hold the profile's slots).
func TestOffers_BackgroundOneAtATime(t *testing.T) {
	s, f, srcs := offerService(t)
	f.delay.Store(int64(150 * time.Millisecond))
	s.Offers(context.Background(), adult(), "movie", 438631, srcs, 30*time.Millisecond)
	if f.maxOpen.Load() != offerWorkers {
		t.Fatalf("%d lookups at once while the step waited", f.maxOpen.Load())
	}
	time.Sleep(170 * time.Millisecond) // the two in flight have ended: one goes on
	f.maxOpen.Store(f.open.Load())
	s.WaitOffers()
	if f.maxOpen.Load() != 1 {
		t.Fatalf("%d lookups at once in the background", f.maxOpen.Load())
	}
	f.delay.Store(0)
	// everything was looked up in the background
	asked := f.total()
	if got := s.Offers(context.Background(), adult(), "movie", 438631, srcs, 5*time.Second); len(got) != 7 {
		t.Fatalf("%+v", got)
	}
	if f.total() > asked { // the failing one is remembered too
		t.Fatalf("not all was looked up before: %d more requests", f.total()-asked)
	}
}

// A source that failed is not asked again at once by every call.
func TestOffers_FailedSourceRemembered(t *testing.T) {
	s, f, srcs := offerService(t)
	for range 3 {
		s.Offers(context.Background(), adult(), "movie", 438631, srcs, 5*time.Second)
	}
	if n := f.hits("/lite/bad"); n != 1 {
		t.Fatalf("the failing source was asked %d times", n)
	}
}

func TestOffers_NothingWhenOffOrKid(t *testing.T) {
	s, f, srcs := offerService(t)
	before := f.total()
	if got := s.Offers(context.Background(), User{ID: 9, IsKid: true}, "movie", 438631, srcs, time.Second); len(got) != 0 {
		t.Fatalf("kid: %+v", got)
	}
	s.Enabled = func() bool { return false }
	if got := s.Offers(context.Background(), adult(), "movie", 438631, srcs, time.Second); len(got) != 0 {
		t.Fatalf("off: %+v", got)
	}
	if got := s.Offers(context.Background(), adult(), "movie", 438631, nil, time.Second); len(got) != 0 {
		t.Fatalf("no sources: %+v", got)
	}
	if f.total() != before {
		t.Fatal("the source was reached")
	}
}

// The request that asked leaving does not fail the lookups: they fill the
// cache for the next call.
func TestOffers_CallerLeaves(t *testing.T) {
	s, f, srcs := offerService(t)
	f.delay.Store(int64(80 * time.Millisecond))
	ctx, stop := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); stop() }()
	s.Offers(ctx, adult(), "movie", 438631, srcs, 5*time.Second)
	s.WaitOffers()
	f.delay.Store(0)
	asked := f.total()
	if got := s.Offers(context.Background(), adult(), "movie", 438631, srcs, 5*time.Second); len(got) != 7 || f.total() > asked {
		t.Fatalf("%d offers, %d more requests", len(got), f.total()-asked)
	}
}
