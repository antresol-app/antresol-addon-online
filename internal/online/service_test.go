package online

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeService is a Service over a fake Lampac; title gives every title.
func fakeService(t *testing.T, h http.Handler, title TitleCtx) *Service {
	t.Helper()
	l := fakeLampac(t, h)
	return &Service{L: l, Refs: NewRefs([]byte("0123456789abcdef0123456789abcdef"), l.Base),
		Titles: func(_ context.Context, kind string, id int) (TitleCtx, error) {
			tc := title
			tc.Serial = kind == "series"
			return tc, nil
		}}
}

var dune = TitleCtx{Title: "Дюна", OriginalTitle: "Dune", OriginalLanguage: "en", Rating: "12+", IMDb: "tt1160419", Year: 2021}

func TestSourcesFlow(t *testing.T) {
	var events, ids atomic.Int32
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/externalids":
			ids.Add(1)
			if q.Get("id") != "438631" || q.Get("serial") != "0" || q.Get("imdb_id") != "tt1160419" {
				t.Errorf("externalids %q", r.URL.RawQuery)
			}
			w.Write([]byte(`{"imdb_id":"tt1160419","kinopoisk_id":123}`))
		case "/lite/events":
			events.Add(1)
			want := map[string]string{"id": "438631", "source": "tmdb", "imdb_id": "tt1160419", "kinopoisk_id": "123", "serial": "0",
				"year": "2021", "original_language": "en", "title": "Дюна", "original_title": "Dune"}
			for k, v := range want {
				if q.Get(k) != v {
					t.Errorf("events %s=%q, want %q", k, q.Get(k), v)
				}
			}
			w.Write(fixture(t, "events.json"))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}), dune)
	got, err := s.Sources(context.Background(), adult(), "movie", 438631)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "alpha" || got[0].Name != "alpha" || got[0].Quality != "1080p" || got[0].Ref == "" {
		t.Fatalf("%+v", got)
	}
	p, rc, err := s.Refs.Open(got[0].Ref)
	if err != nil || rc != (RefContext{Kind: "movie", TMDBID: 438631, Source: "alpha"}) {
		t.Fatalf("%+v %v", rc, err)
	}
	// the source ref carries the card query (Lampac's links do not)
	for _, kv := range []string{"id=438631", "source=tmdb", "kinopoisk_id=123", "imdb_id=tt1160419", "serial=0", "year=2021"} {
		if !strings.HasPrefix(p, "/lite/alpha?") || !strings.Contains(p, kv) {
			t.Fatalf("source path %q lacks %s", p, kv)
		}
	}
	if _, err := s.Sources(context.Background(), adult(), "movie", 438631); err != nil {
		t.Fatal(err)
	}
	if events.Load() != 1 || ids.Load() != 1 {
		t.Fatalf("second call reached Lampac: events %d, ids %d", events.Load(), ids.Load())
	}
}

func TestSourcesBudget(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/externalids" {
			w.Write([]byte(`{}`))
			return
		}
		select { // events that hang
		case <-release:
		case <-r.Context().Done():
		}
	}), dune)
	s.SourcesBudget = 200 * time.Millisecond
	start := time.Now()
	_, err := s.Sources(context.Background(), adult(), "movie", 438631)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestSourcesGate(t *testing.T) {
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused profile reached Lampac: %s", r.URL)
	}), TitleCtx{Title: "Альфа", Rating: "18+", Year: 2021})
	if _, err := s.Sources(context.Background(), limited("12+"), "movie", 1); err != ErrRating {
		t.Fatalf("%v", err)
	}
	if _, err := s.Sources(context.Background(), adult(), "game", 1); err != ErrUnknownTitle {
		t.Fatalf("%v", err)
	}
}

// browseLampac answers the alpha/beta/gamma levels.
func browseLampac(t *testing.T, seen *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.URL.RequestURI())
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/lite/gamma" && q.Get("href") == "one":
			w.Write(fixture(t, "movie-alpha.json"))
		case r.URL.Path == "/lite/gamma":
			w.Write(fixture(t, "similar-gamma.json"))
		case r.URL.Path == "/lite/beta" && q.Get("s") == "1":
			w.Write(fixture(t, "episode-beta-s1.json"))
		case r.URL.Path == "/lite/beta":
			w.Write(fixture(t, "season-beta.json"))
		case r.URL.Path == "/lite/alpha":
			w.Write(fixture(t, "movie-alpha.json"))
		default:
			http.NotFound(w, r)
		}
	})
}

func TestBrowseAutoPicksSingleCandidate(t *testing.T) {
	s := fakeService(t, browseLampac(t, nil), dune)
	ref := s.Refs.Mint("/lite/gamma?id=438631&source=tmdb&title=x", RefContext{Kind: "movie", TMDBID: 438631, Source: "gamma"})
	lv, err := s.Browse(context.Background(), adult(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if lv.Level != "movie" || len(lv.Files) != 2 {
		t.Fatalf("%+v", lv)
	}
	// two candidates within the year: the person picks
	s2 := fakeService(t, browseLampac(t, nil), TitleCtx{Title: "Гамма", Rating: "12+", Year: 1985})
	ref = s2.Refs.Mint("/lite/gamma?id=1", RefContext{Kind: "movie", TMDBID: 1, Source: "gamma"})
	lv, err = s2.Browse(context.Background(), adult(), ref)
	if err != nil || lv.Level != "pick" || len(lv.Candidates) != 3 || lv.Candidates[0].Ref == "" {
		t.Fatalf("%+v %v", lv, err)
	}
}

func TestBrowseCarriesContextAndCard(t *testing.T) {
	var seen []string
	s := fakeService(t, browseLampac(t, &seen), dune)
	ctx := context.Background()
	src := s.Refs.Mint("/lite/beta?id=1396&source=tmdb&serial=1&title=%D0%91&kinopoisk_id=123", RefContext{Kind: "series", TMDBID: 1396, Source: "beta"})
	lv, err := s.Browse(ctx, adult(), src)
	if err != nil || lv.Level != "seasons" || len(lv.Seasons) != 2 {
		t.Fatalf("%+v %v", lv, err)
	}
	p, rc, err := s.Refs.Open(lv.Seasons[0].Ref)
	if err != nil || rc.Season != 1 || rc.Source != "beta" || rc.TMDBID != 1396 {
		t.Fatalf("%q %+v %v", p, rc, err)
	}
	// the season link lacked the card query: it is added (absent keys only)
	for _, kv := range []string{"id=1396", "source=tmdb", "serial=1", "s=1", "kinopoisk_id=123"} {
		if !strings.Contains(p, kv) {
			t.Fatalf("season path %q lacks %s", p, kv)
		}
	}
	if strings.Count(p, "kinopoisk_id=") != 1 {
		t.Fatalf("duplicated key: %q", p)
	}
	eps, err := s.Browse(ctx, adult(), lv.Seasons[0].Ref)
	if err != nil || eps.Level != "episodes" || len(eps.Episodes) != 2 {
		t.Fatalf("%+v %v", eps, err)
	}
	// the rjson=False of the season link went to Lampac as rjson=true
	last := seen[len(seen)-1]
	if strings.Contains(last, "rjson=False") || !strings.Contains(last, "rjson=true") || !strings.Contains(last, "uid=secret") {
		t.Fatalf("request %q", last)
	}
	_, erc, err := s.Refs.Open(eps.Episodes[1].Ref)
	if err != nil || erc.Season != 1 || erc.Episode != 2 || erc.Voice != "Дубляж" || erc.File != 2 || erc.ID == "" {
		t.Fatalf("episode ctx %+v %v", erc, err)
	}
	_, vrc, err := s.Refs.Open(eps.Voices[1].Ref)
	if err != nil || vrc.Voice != "Многоголосый" || vrc.Season != 1 || vrc.Episode != 0 {
		t.Fatalf("voice ctx %+v %v", vrc, err)
	}
	// a file ref opens that one file again, with its play data
	one, err := s.Browse(ctx, adult(), eps.Episodes[1].Ref)
	if err != nil || len(one.Episodes) != 1 || one.Episodes[0].Episode != 2 {
		t.Fatalf("%+v %v", one, err)
	}
	if it, _, err := one.PlayItemFor(erc); err != nil || it.Method != "call" || it.URLs[0] != "http://lampac.test:9118/lite/beta/video?t=1&s=1&e=2&token=abc" {
		t.Fatalf("play item %+v %v", it, err)
	}
	for _, c := range []string{"secret", "lampac.test", "cdn.example", "/proxy/"} {
		for _, r := range []string{lv.Seasons[0].Ref, eps.Episodes[0].Ref} {
			if strings.Contains(r, c) {
				t.Fatalf("ref leaks %s", c)
			}
		}
	}
}

func TestBrowseGateAndErrors(t *testing.T) {
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lite/accsdb":
			w.Write(fixture(t, "err-accsdb.json"))
		case "/lite/empty":
			w.Write([]byte(`{}`))
		case "/lite/slow":
			<-r.Context().Done()
		default:
			w.Write(fixture(t, "movie-alpha.json"))
		}
	}), TitleCtx{Title: "Альфа", Rating: "18+", Year: 2021})
	ctx := context.Background()
	c := RefContext{Kind: "movie", TMDBID: 7, Source: "alpha"}
	if _, err := s.Browse(ctx, limited("12+"), s.Refs.Mint("/lite/alpha", c)); err != ErrRating {
		t.Fatalf("rating: %v", err)
	}
	if _, err := s.Browse(ctx, adult(), "garbage"); err != ErrBadRef {
		t.Fatalf("bad ref: %v", err)
	}
	if _, err := s.Browse(ctx, adult(), s.Refs.Mint("/lite/accsdb", c)); !errors.Is(err, ErrSourceFailed) {
		t.Fatalf("accsdb: %v", err)
	}
	if _, err := s.Browse(ctx, adult(), s.Refs.Mint("/lite/empty", c)); !errors.Is(err, ErrNothing) {
		t.Fatalf("empty: %v", err)
	}
	// a slow source is that source failing, not Lampac being down
	s.LevelBudget = 100 * time.Millisecond
	if _, err := s.Browse(ctx, adult(), s.Refs.Mint("/lite/slow", c)); !errors.Is(err, ErrSourceFailed) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("slow: %v", err)
	}
}

func TestStatus(t *testing.T) {
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lite/events" {
			if r.URL.Query().Get("id") != "438631" {
				t.Errorf("probe title %q", r.URL.RawQuery)
			}
			w.Header().Set("Server", "lampac-test")
			w.Write(fixture(t, "events.json"))
			return
		}
		w.Write([]byte(`{}`))
	}), dune)
	st, err := s.Status(context.Background())
	if err != nil || !st.Configured || !st.Reachable || st.Sources != 1 || st.Version != "lampac-test" {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestCacheTTLAndLRU(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newTTLCache[string, int](2, time.Minute)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	c.put("a", 1)
	c.put("b", 2)
	if _, ok := c.get(ctx, "a"); !ok { // a is now the most recent
		t.Fatal("a")
	}
	c.put("c", 3) // evicts b
	if _, ok := c.get(ctx, "b"); ok {
		t.Fatal("b not evicted")
	}
	if _, ok := c.get(Bypass(ctx), "a"); ok {
		t.Fatal("bypass served from cache")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.get(ctx, "a"); ok {
		t.Fatal("expired entry served")
	}
}

// A byte budget drops the oldest values while all together are over it;
// replacing a value counts its new size.
func TestCacheByteBudget(t *testing.T) {
	c := newTTLCache[string, []byte](10, time.Minute)
	c.limitBytes(10, func(b []byte) int { return len(b) })
	ctx := context.Background()
	c.put("a", make([]byte, 4))
	c.put("b", make([]byte, 4))
	c.put("c", make([]byte, 4)) // 12 > 10: a goes
	if _, ok := c.get(ctx, "a"); ok {
		t.Fatal("a kept over the budget")
	}
	if _, ok := c.get(ctx, "b"); !ok || c.bytes != 8 {
		t.Fatalf("b dropped or bytes %d", c.bytes)
	}
	c.put("b", make([]byte, 1))
	if c.bytes != 5 {
		t.Fatalf("bytes after a replace: %d", c.bytes)
	}
	c.put("d", make([]byte, 20)) // alone over the budget: kept, nothing else is
	if c.ll.Len() != 1 || c.bytes != 20 {
		t.Fatalf("len %d bytes %d", c.ll.Len(), c.bytes)
	}
}

// A call answer over maxCachedLevel is passed on but not kept.
func TestCallAnswerTooBigIsNotCached(t *testing.T) {
	l := fakeLampac(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/big") {
			fmt.Fprintf(w, `{"url":"https://cdn.test/%s.m3u8"}`, strings.Repeat("x", maxCachedLevel))
			return
		}
		io.WriteString(w, `{"url":"https://cdn.test/a.m3u8"}`)
	}))
	s := &Service{L: l}
	s.init()
	ctx := context.Background()
	for _, p := range []string{"/lite/big", "/lite/small"} {
		if _, err := s.call(ctx, p, RefContext{}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if _, ok := s.calls.get(ctx, "/lite/big"); ok {
		t.Fatal("an answer over the limit was kept")
	}
	if _, ok := s.calls.get(ctx, "/lite/small"); !ok {
		t.Fatal("a small answer was not kept")
	}
}

// A failed title read is remembered; concurrent reads share one call,
// which outlives a caller that stops waiting.
func TestTitleNegativeCacheAndCoalescing(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	fail := atomic.Bool{}
	s := &Service{L: &Lampac{}, Titles: func(ctx context.Context, kind string, id int) (TitleCtx, error) {
		calls.Add(1)
		if fail.Load() {
			return TitleCtx{}, errors.New("tmdb down")
		}
		<-release
		return TitleCtx{Title: "Альфа", Rating: "12+"}, nil
	}}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tc, err := s.Title(context.Background(), "movie", 1); err != nil || tc.Title != "Альфа" {
				t.Errorf("%+v %v", tc, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	// a caller that gives up gets «unavailable» and leaves a miss behind…
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Title(short, "movie", 1); !errors.Is(err, ErrTitleUnavailable) {
		t.Fatalf("short: %v", err)
	}
	if _, err := s.Title(context.Background(), "movie", 1); !errors.Is(err, ErrTitleUnavailable) {
		t.Fatalf("after a slow read: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	// …while the read goes on for those who wait, and its answer is cached
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls %d", n)
	}
	if tc, err := s.Title(context.Background(), "movie", 1); err != nil || tc.Title != "Альфа" || calls.Load() != 1 {
		t.Fatalf("cached: %+v %v", tc, err)
	}
	fail.Store(true)
	for range 3 {
		if _, err := s.Title(context.Background(), "movie", 2); !errors.Is(err, ErrTitleUnavailable) {
			t.Fatalf("%v", err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("failure not remembered: %d calls", n)
	}
}

// An episode ref of a level opened without a voice is pinned to the voice
// that was active: a re-fetch plays that voice even when Lampac's default
// changes.
func TestEpisodeRefPinsActiveVoice(t *testing.T) {
	s := fakeService(t, browseLampac(t, nil), dune)
	ctx := context.Background()
	season := s.Refs.Mint("/lite/beta?id=1396&s=1&kinopoisk_id=123", RefContext{Kind: "series", TMDBID: 1396, Source: "beta", Season: 1})
	eps, err := s.Browse(ctx, adult(), season)
	if err != nil || len(eps.Episodes) != 2 {
		t.Fatalf("%+v %v", eps, err)
	}
	p, rc, err := s.Refs.Open(eps.Episodes[0].Ref)
	if err != nil || rc.Voice != "Дубляж" || !strings.Contains(p, "t=1") {
		t.Fatalf("episode ref %q %+v %v", p, rc, err)
	}
	// a level opened with a voice already names it
	vp, _, _ := s.Refs.Open(eps.Voices[1].Ref)
	if !strings.Contains(vp, "t=2") {
		t.Fatalf("voice ref %q", vp)
	}
}

// A source's Cookie goes only to its own site (the site of its default
// stream): not to another quality's host, not to a subtitle host.
func TestCookieOnlyToTheSourceSiteInResolve(t *testing.T) {
	var mu sync.Mutex
	cookies := map[string]string{}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		mu.Lock()
		cookies[host+r.URL.Path] = r.Header.Get("Cookie")
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ".vtt") {
			w.Write([]byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nx\n"))
			return
		}
		w.Write([]byte("\x00\x00\x00\x18ftypmp42" + strings.Repeat("m", 600)))
	}))
	defer cdn.Close()
	port := portOf(cdn)
	level := `{"type":"movie","data":[{"method":"play","translate":"Дубляж","url":"http://cdn.example:` + port + `/a/1080.mp4",` +
		`"quality":{"1080p":"http://cdn.example:` + port + `/a/1080.mp4","480p":"http://cdn2.example:` + port + `/b/480.mp4"},` +
		`"headers":{"Cookie":"s=1","Referer":"https://player.example/"},` +
		`"subtitles":[{"label":"Rus","url":"http://cdn2.example:` + port + `/b/ru.vtt"}]}]}`
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(level)) }), dune)
	up := testNet(t, "lampac.test:9118", []string{"cdn.example", "cdn2.example"}, cdn.Listener.Addr().String())
	s.Upstream = up
	s.Subs = &Subs{Dir: t.TempDir(), HTTP: up}
	t.Cleanup(s.Subs.Wait)
	s.Proxy = &Proxy{Key: proxyKey, HTTP: up}
	ctx := context.Background()
	lv, err := s.Browse(ctx, adult(), s.Refs.Mint("/lite/alpha?id=1", RefContext{Kind: "movie", TMDBID: 1, Source: "alpha"}))
	if err != nil || len(lv.Files) != 1 {
		t.Fatalf("%+v %v", lv, err)
	}
	r, err := s.Resolve(ctx, adult(), lv.Files[0].Ref, "480p", false)
	if err != nil || r.Quality != "480p" || r.CookieSite != "cdn.example" {
		t.Fatalf("%+v %v", r, err)
	}
	subs, warm := s.PlaySubtitles(ctx, r)
	if len(subs) != 1 {
		t.Fatalf("subs %+v", subs)
	}
	warm()
	s.Subs.Wait() // the subtitle's fetch
	sid, err := s.Proxy.Open(1, r, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := get(s.Proxy, 1, s.Proxy.URL(sid, r.URL, "")); w.Code != 200 {
		t.Fatalf("proxy %d", w.Code)
	}
	if _, err := s.Resolve(ctx, adult(), lv.Files[0].Ref, "1080p", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for path, want := range map[string]string{"cdn2.example/b/480.mp4": "", "cdn2.example/b/ru.vtt": "", "cdn.example/a/1080.mp4": "s=1"} {
		if got, ok := cookies[path]; !ok || got != want {
			t.Fatalf("%s: cookie %q (asked %v)", path, got, ok)
		}
	}
}

// A title answered from the library after a failing TMDB read is kept
// for a minute, not a day: TMDB is asked again soon.
func TestTitleFallbackCachedBriefly(t *testing.T) {
	var reads atomic.Int32
	fallback := true
	s := fakeService(t, http.NotFoundHandler(), dune)
	s.Titles = func(context.Context, string, int) (TitleCtx, error) {
		reads.Add(1)
		t := dune
		t.Fallback = fallback
		return t, nil
	}
	ctx := context.Background()
	now := time.Now()
	s.init()
	s.titles.now = func() time.Time { return now }
	for range 2 {
		if _, err := s.Title(ctx, "movie", 1); err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("reads %d", reads.Load())
	}
	now = now.Add(2 * time.Minute)
	fallback = false
	if _, err := s.Title(ctx, "movie", 1); err != nil || reads.Load() != 2 {
		t.Fatalf("after a minute: reads %d %v", reads.Load(), err)
	}
	// a TMDB answer is kept for the day
	now = now.Add(2 * time.Hour)
	if _, err := s.Title(ctx, "movie", 1); err != nil || reads.Load() != 2 {
		t.Fatalf("TMDB answer: reads %d %v", reads.Load(), err)
	}
}

// Concurrent callers of one key share a single fetch; one caller leaving
// does not cancel it for the others.
func TestSharedFetchOnce(t *testing.T) {
	s := &Service{}
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func(ctx context.Context) ([]byte, error) {
		calls.Add(1)
		select {
		case <-release:
			return []byte("ok"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	leaving, leave := context.WithCancel(context.Background())
	errs := make(chan error, 3)
	go func() { _, err := s.shared(leaving, "k", fetch); errs <- err }()
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			b, err := s.shared(context.Background(), "k", fetch)
			if err != nil {
				errs <- err
				return
			}
			results <- string(b)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	leave()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("leaving caller: %v", err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if got := <-results; got != "ok" {
			t.Fatalf("got %q", got)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
}

func TestSourceWithoutEvents(t *testing.T) {
	var events atomic.Int32
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/externalids":
			w.Write([]byte(`{"imdb_id":"tt1160419","kinopoisk_id":123}`))
		case "/lite/events":
			events.Add(1)
			w.Write(fixture(t, "events.json"))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}), dune)
	ctx := context.Background()
	// nothing seen yet: an id waits for the title's own list, which
	// lists neither a hidden (beta) nor an rch source (gamma)
	for _, id := range []string{"beta", "gamma", "events"} {
		if _, err := s.Source(ctx, adult(), "movie", 438631, id); err != ErrNothing {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if events.Load() != 1 {
		t.Fatalf("events asked %d times", events.Load())
	}
	// the list cached, then gone (6 h): a balancer the last events listed
	// as shown and non-rch gets its ref without them; others still wait
	s.events = newTTLCache[titleKey, []Source](500, eventsTTL)
	got, err := s.Source(ctx, adult(), "movie", 438631, "alpha")
	if err != nil || got.ID != "alpha" || got.Ref == "" {
		t.Fatalf("%+v %v", got, err)
	}
	if events.Load() != 1 {
		t.Fatal("Source waited for /lite/events")
	}
	// the ref is the one Sources mints
	list, err := s.Sources(ctx, adult(), "movie", 438631)
	if err != nil || len(list) != 1 || events.Load() != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	p1, rc1, _ := s.Refs.Open(got.Ref)
	p2, rc2, _ := s.Refs.Open(list[0].Ref)
	if p1 != p2 || rc1 != rc2 {
		t.Fatalf("Source %q %+v, Sources %q %+v", p1, rc1, p2, rc2)
	}
	// with the list cached, it answers from the list
	if got, err := s.Source(ctx, adult(), "movie", 438631, "alpha"); err != nil || got.Ref != list[0].Ref || got.Quality != "1080p" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.Source(ctx, adult(), "movie", 438631, "gamma"); err != ErrNothing {
		t.Fatalf("a source not listed: %v", err)
	}
	for _, id := range []string{"", "../x", "a/b", "a?b=1"} {
		if _, err := s.Source(ctx, adult(), "movie", 1, id); err != ErrNothing {
			t.Fatalf("%q: %v", id, err)
		}
	}
}

func TestSourceGate(t *testing.T) {
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused profile reached Lampac: %s", r.URL)
	}), TitleCtx{Title: "Альфа", Rating: "18+", Year: 2021})
	if _, err := s.Source(context.Background(), limited("12+"), "movie", 1, "alpha"); err != ErrRating {
		t.Fatalf("%v", err)
	}
	kid := adult()
	kid.IsKid = true
	if _, err := s.Source(context.Background(), kid, "movie", 1, "alpha"); err != ErrKid {
		t.Fatalf("%v", err)
	}
	s.Enabled = func() bool { return false }
	if _, err := s.Source(context.Background(), adult(), "movie", 1, "alpha"); err != ErrOff {
		t.Fatalf("%v", err)
	}
}

// The probes race with a stagger: a dead first URL costs the stagger, not
// probeTimeout; an earlier URL that answers in time still wins.
func TestProbeRace(t *testing.T) {
	old := probeStagger
	probeStagger = 200 * time.Millisecond
	t.Cleanup(func() { probeStagger = old })
	var mu sync.Mutex
	asked := map[string]int{}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".mp4")
		switch {
		case name == "dead":
			<-r.Context().Done()
			return
		case name == "gone":
			http.NotFound(w, r)
			return
		case name == "dash":
			w.Write([]byte(`<?xml version="1.0"?><MPD>`))
			return
		case strings.HasPrefix(name, "slow"):
			d, _ := time.ParseDuration(strings.TrimPrefix(name, "slow"))
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		w.Write([]byte("\x00\x00\x00\x18ftypmp42" + strings.Repeat("m", 600)))
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn) + "/"
	s := &Service{Upstream: testNet(t, "lampac.test:9118", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	s.init()
	race := func(names ...string) (int, time.Duration, bool, int, error) {
		var cs []candidate
		for i, n := range names {
			cs = append(cs, candidate{raw: base + n + ".mp4", quality: i})
		}
		start := time.Now()
		won, _, dash, failed, err := s.race(context.Background(), cs, func(candidate) http.Header { return http.Header{} },
			func(candidate, error) {})
		return won, time.Since(start), dash, failed, err
	}
	// a dead first URL: the second wins after one stagger
	if won, d, _, _, err := race("dead", "ok"); err != nil || won != 1 || d > 2*time.Second {
		t.Fatalf("dead first: won %d in %v, %v", won, d, err)
	}
	// a failing one starts the next at once
	if won, d, _, failed, _ := race("gone", "ok"); won != 1 || failed != 1 || d >= probeStagger {
		t.Fatalf("gone first: won %d in %v, failed %d", won, d, failed)
	}
	// an earlier URL that answers within the stagger wins; the later one is not asked
	if won, _, _, _, _ := race("slow50ms", "ok-b"); won != 0 {
		t.Fatalf("slow first: won %d", won)
	}
	mu.Lock()
	if asked["/ok-b.mp4"] != 0 {
		t.Fatal("the later URL was probed")
	}
	mu.Unlock()
	// one that answers just after the stagger still beats a later one
	if won, _, _, _, _ := race("slow300ms", "ok-c"); won != 0 {
		t.Fatalf("late first: won %d", won)
	}
	// nothing plays: DASH and failures are told apart
	if won, _, dash, failed, err := race("dash", "gone"); won != -1 || !dash || failed != 1 || err != nil {
		t.Fatalf("none: %d %v %d %v", won, dash, failed, err)
	}
	// a caller that leaves gets its error
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, _, _, _, err := s.race(ctx, []candidate{{raw: base + "dead.mp4"}}, func(candidate) http.Header { return http.Header{} },
		func(candidate, error) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// A flight every caller left (a badge prefetch aborted, the sheet closed,
// the title page's warm-up) still fills the caches for the next request.
func TestFlightFillsCacheWhenCallersLeave(t *testing.T) {
	var events, levels atomic.Int32
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		switch r.URL.Path {
		case "/externalids":
			w.Write([]byte(`{"imdb_id":"tt1160419","kinopoisk_id":123}`))
		case "/lite/events":
			events.Add(1)
			w.Write(fixture(t, "events.json"))
		default:
			levels.Add(1)
			w.Write(fixture(t, "movie-alpha.json"))
		}
	}), dune)
	s.init()
	// the ids first, so the next cancellations land inside the flights
	s.card(context.Background(), "movie", 438631, dune)
	ctx, stop := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); stop() }()
	if _, err := s.Sources(ctx, adult(), "movie", 438631); err == nil {
		t.Fatal("the caller left: no answer expected")
	}
	lctx, lstop := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); lstop() }()
	if _, err := s.levelBody(lctx, "/lite/alpha?id=1"); err == nil {
		t.Fatal("the caller left: no level expected")
	}
	waitFor(t, func() bool {
		_, ev := s.events.get(context.Background(), titleKey{"movie", 438631})
		_, lv := s.levels.get(context.Background(), "/lite/alpha?id=1")
		return ev && lv
	})
	if _, err := s.Sources(context.Background(), adult(), "movie", 438631); err != nil || events.Load() != 1 {
		t.Fatalf("events asked %d times: %v", events.Load(), err)
	}
	if _, err := s.levelBody(context.Background(), "/lite/alpha?id=1"); err != nil || levels.Load() != 1 {
		t.Fatalf("level asked %d times: %v", levels.Load(), err)
	}
}

// fakeKnownStore is the saved balancers of a Service: Save keeps the last
// map and tells saved.
type fakeKnownStore struct {
	mu    sync.Mutex
	shown map[string]bool
	at    time.Time
	err   error
	saves int
	saved chan struct{}
}

func newFakeKnownStore() *fakeKnownStore { return &fakeKnownStore{saved: make(chan struct{}, 10)} }

func (f *fakeKnownStore) attach(s *Service) {
	s.LoadKnown = func(context.Context) (map[string]bool, time.Time, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return maps.Clone(f.shown), f.at, f.err
	}
	s.SaveKnown = func(_ context.Context, shown map[string]bool) error {
		f.mu.Lock()
		f.shown, f.at, f.saves = maps.Clone(shown), time.Now(), f.saves+1
		f.mu.Unlock()
		f.saved <- struct{}{}
		return nil
	}
}

func (f *fakeKnownStore) waitSaved(t *testing.T) {
	t.Helper()
	select {
	case <-f.saved:
	case <-time.After(3 * time.Second):
		t.Fatal("the balancers were not saved")
	}
}

func eventsHandler(t *testing.T, events *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/externalids":
			w.Write([]byte(`{"imdb_id":"tt1160419","kinopoisk_id":123}`))
		case "/lite/events":
			events.Add(1)
			w.Write(fixture(t, "events.json"))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
}

// The balancers a list named are saved, once, and a service made after a
// restart answers Source for a shown one at once, without the title's list.
func TestKnownSourcesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	store := newFakeKnownStore()
	var events1 atomic.Int32
	s1 := fakeService(t, eventsHandler(t, &events1), dune)
	store.attach(s1)
	if _, err := s1.Sources(ctx, adult(), "movie", 438631); err != nil {
		t.Fatal(err)
	}
	store.waitSaved(t)
	if store.shown["alpha"] != true || store.shown["beta"] != false || store.shown["gamma"] != false {
		t.Fatalf("saved %v", store.shown)
	}
	// another title lists the same: nothing changed, nothing saved again
	if _, err := s1.Sources(ctx, adult(), "movie", 438632); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if store.saves != 1 {
		t.Fatalf("saved %d times, want once", store.saves)
	}

	// after the restart: a shown balancer needs no events, a hidden or unknown one still waits for them
	var events2 atomic.Int32
	s2 := fakeService(t, eventsHandler(t, &events2), dune)
	store.attach(s2)
	got, err := s2.Source(ctx, adult(), "movie", 438631, "alpha")
	if err != nil || got.ID != "alpha" || got.Ref == "" {
		t.Fatalf("%+v %v", got, err)
	}
	if events2.Load() != 0 {
		t.Fatalf("Source waited for /lite/events %d times after a restart", events2.Load())
	}
	if _, err := s2.Source(ctx, adult(), "movie", 438631, "beta"); err != ErrNothing || events2.Load() != 1 {
		t.Fatalf("a hidden balancer: %v, events %d", err, events2.Load())
	}
}

// Saved balancers older than a week, unreadable ones, and a service with no
// store are not believed: Source waits for the list as before.
func TestKnownSourcesNotBelieved(t *testing.T) {
	ctx := context.Background()
	for name, edit := range map[string]func(f *fakeKnownStore){
		"stale":   func(f *fakeKnownStore) { f.at = time.Now().Add(-knownMaxAge - time.Hour) },
		"error":   func(f *fakeKnownStore) { f.err = errors.New("db gone") },
		"no data": func(f *fakeKnownStore) { f.shown = nil },
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeKnownStore()
			store.shown, store.at = map[string]bool{"alpha": true}, time.Now()
			edit(store)
			var events atomic.Int32
			s := fakeService(t, eventsHandler(t, &events), dune)
			store.attach(s)
			got, err := s.Source(ctx, adult(), "movie", 438631, "alpha")
			if err != nil || got.ID != "alpha" {
				t.Fatalf("%+v %v", got, err)
			}
			if events.Load() != 1 {
				t.Fatalf("events asked %d times, want 1 (the list is awaited)", events.Load())
			}
		})
	}
}

// Balancers saved a day ago are saved again by the next list, so their age
// is that of the last confirmation.
func TestKnownSourcesRefreshed(t *testing.T) {
	store := newFakeKnownStore()
	store.shown, store.at = map[string]bool{"alpha": true, "beta": false, "gamma": false}, time.Now().Add(-2*knownRefresh)
	var events atomic.Int32
	s := fakeService(t, eventsHandler(t, &events), dune)
	store.attach(s)
	if _, err := s.Sources(context.Background(), adult(), "movie", 438631); err != nil {
		t.Fatal(err)
	}
	store.waitSaved(t)
	if time.Since(store.at) > time.Minute {
		t.Fatalf("saved at %v", store.at)
	}
}
