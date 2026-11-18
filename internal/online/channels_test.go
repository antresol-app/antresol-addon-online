package online

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-online/internal/coretest"
	"github.com/antresol-app/antresol/addonkit"
)

// TestRelayExt: the extension a relayed URI gets, the upstream one when
// ffmpeg takes it, else by the playlist (a master playlist lists
// playlists, a media playlist segments).
func TestRelayExt(t *testing.T) {
	for _, c := range []struct {
		url    string
		master bool
		want   string
	}{
		{"https://cdn.example/a/seg-1.ts?sig=x", false, ".ts"},
		{"https://cdn.example/a/1.m4s", false, ".m4s"},
		{"https://cdn.example/a/init.MP4", false, ".mp4"},
		{"https://cdn.example/a/720/index.m3u8", true, ".m3u8"},
		{"https://cdn.example/a/seg", false, ".ts"},
		{"https://cdn.example/a/720", true, ".m3u8"},
		{"https://cdn.example/a/x.php", false, ".ts"},
		{"https://cdn.example/a/sub.vtt", true, ".vtt"},
	} {
		u, _ := url.Parse(c.url)
		if got := relayExt(u, c.master); got != c.want {
			t.Errorf("relayExt(%s, %v) = %q, want %q", c.url, c.master, got, c.want)
		}
	}
	for _, c := range []struct{ in, want string }{{"~1.ts", "~1"}, {"abc.m3u8", "abc"}, {"abc", "abc"}, {"abc.exe", "abc.exe"}} {
		if got := cutRelayExt(c.in); got != c.want {
			t.Errorf("cutRelayExt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRefusedStream(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef01234567"
	for _, u := range []string{
		"magnet:?xt=urn:btih:" + hash,
		"http://cdn.example/a.torrent",
		"http://cdn.example/get?file=a.TORRENT",
		"http://ts.example:8090/stream/film.mkv?link=" + hash + "&index=1&play",
		"http://ts.example/stream/film.mkv?link=magnet%3A%3Fxt%3Durn%3Abtih%3A" + hash,
		"http://ts.example/play/" + hash + "/1",
		"http://lampac.test:9118/ts/stream?link=" + hash,
		"http://torrserver.lan/x.mp4",
		"http://lampac.test:9118/proxy/http%3A%2F%2Fts%3A8090%2Fstream%3Flink%3Dmagnet%3A",
		"ftp://cdn.example/a.mp4",
		"file:///etc/passwd",
		"",
	} {
		if !refusedStream(u) {
			t.Errorf("not refused: %q", u)
		}
	}
	for _, u := range []string{
		"https://cdn.example/a/1080.m3u8",
		"http://lampac.test:9118/proxy/https/cdn.example/a/index.m3u8?sig=abc",
		"https://cdn.example/hls/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08/master.m3u8",
	} {
		if refusedStream(u) {
			t.Errorf("refused: %q", u)
		}
	}
	for _, s := range []string{"torrserver", "TorrServer", "ts", "jackett", "jacred", "rutor", "kinozal", "torrents", "magnet"} {
		if !refusedSource(s) {
			t.Errorf("source %q not refused", s)
		}
	}
	for _, s := range []string{"alpha", "filmix", "kinobase", "collaps", "videocdn", "zetflix", "rezka"} {
		if refusedSource(s) {
			t.Errorf("source %q refused", s)
		}
	}
}

// fakeCatalog is TMDB: genres 35 Комедия and 18 Драма; /discover gives
// ids 1…n of a page of 4; details by id.
type fakeCatalog struct {
	mu      sync.Mutex
	n       int
	queries []tmdb.DiscoverQuery
	details map[int]tmdb.Details
}

func (c *fakeCatalog) GenreNames(context.Context, string) (map[int]string, error) {
	return map[int]string{35: "комедия", 18: "драма", 99: "документальный"}, nil
}

func (c *fakeCatalog) DiscoverMovies(_ context.Context, q tmdb.DiscoverQuery) ([]tmdb.DiscoverResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, q)
	var out []tmdb.DiscoverResult
	for id := (q.Page-1)*4 + 1; id <= q.Page*4 && id <= c.n; id++ {
		out = append(out, tmdb.DiscoverResult{ID: id, Title: "Film", Year: 2000, Poster: "/p.jpg"})
	}
	return out, nil
}

func (c *fakeCatalog) Movie(_ context.Context, id int) (tmdb.Details, error) {
	if d, ok := c.details[id]; ok {
		return d, nil
	}
	return tmdb.Details{ID: id, Title: "Фильм", Year: 2000, Runtime: 100, Rating: "12+", Genres: []string{"комедия"}, Backdrop: "/b.jpg"}, nil
}

func TestChannelCandidates(t *testing.T) {
	s := fakeService(t, http.NotFoundHandler(), dune)
	s.Proxy = &Proxy{Key: proxyKey}
	cat := &fakeCatalog{n: 30, details: map[int]tmdb.Details{
		3: {ID: 3, Title: "Короткий", Runtime: 20},
		4: {ID: 4, Title: "Взрослый", Runtime: 100, Rating: "18+"},
		5: {ID: 5, Title: "Без длины"},
	}}
	lib := &coretest.Core{} // the library has films 1 and 2
	lib.AddMovie(1)
	lib.AddMovie(2)
	x := &ChannelExtras{Online: s, TMDB: cat, Library: lib}
	ctx := context.Background()
	got, err := x.Candidates(ctx, addonkit.ChannelRules{Genres: []string{"Comedy", "драма"}, YearFrom: 1990, YearTo: 1999, MaxRating: "16+"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("%d candidates: %+v", len(got), got)
	}
	for _, c := range got {
		if c.TMDBID <= 5 || c.RuntimeMs != 100*60000 || c.Key == "" || c.Kind != "movie" || c.Backdrop != "/b.jpg" || c.Poster != "/p.jpg" {
			t.Fatalf("candidate %+v (library, short, adult or no runtime let through?)", c)
		}
	}
	q := cat.queries[0]
	if len(q.GenreIDs) != 2 || q.GenreIDs[0] != 18 || q.GenreIDs[1] != 35 || q.YearFrom != 1990 || q.YearTo != 1999 || q.MinVotes == 0 {
		t.Fatalf("query %+v", q)
	}
	// genres TMDB has none of, online off: nothing (a channel with its
	// extras off is never asked: the core checks the rule)
	if got, _ := x.Candidates(ctx, addonkit.ChannelRules{Genres: []string{"вестерн-мюзикл"}}, 5); len(got) != 0 {
		t.Fatalf("unknown genre: %+v", got)
	}
	x.On = func(context.Context) bool { return false }
	if x.Ready(ctx) {
		t.Fatal("ready with the setting off")
	}
	if got, _ := x.Candidates(ctx, addonkit.ChannelRules{}, 5); len(got) != 0 {
		t.Fatal("setting off")
	}
}

// TestChannelResolveOnTheSocket: the torrent-looking source is never
// asked; the stream is a route under /ch/<token>/ on the addon's socket
// (the core's relay reads it), served only while the stream is open; a
// playlist's URIs are relative, so they stay under the stream's folder.
// A source whose stream is TorrServer is refused.
func TestChannelResolveOnTheSocket(t *testing.T) {
	body := "\x00\x00\x00\x18ftypmp42" + strings.Repeat("m", 600)
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts\n#EXTINF:4,\n/abs/seg2.ts\n#EXT-X-ENDLIST\n"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/h/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(playlist))
		case "/h/seg1.ts", "/abs/seg2.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write([]byte("G-" + r.URL.Path))
		default:
			w.Write([]byte(body))
		}
	}))
	defer cdn.Close()
	port := portOf(cdn)
	var mu sync.Mutex
	var asked []string
	streamURL := "http://cdn.example:" + port + "/a/1080.mp4"
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		u := streamURL
		mu.Unlock()
		switch r.URL.Path {
		case "/externalids":
			w.Write([]byte(`{}`))
		case "/lite/events":
			w.Write([]byte(`[{"name":"TorrServer","url":"http://lampac.test:9118/lite/torrserver","show":true,"balanser":"torrserver"},` +
				`{"name":"alpha","url":"http://lampac.test:9118/lite/alpha","show":true,"balanser":"alpha"}]`))
		case "/lite/alpha":
			w.Write([]byte(`{"type":"movie","data":[{"method":"play","translate":"Дубляж","url":"` + u + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	}), dune)
	s.Upstream = testNet(t, "lampac.test:9118", []string{"cdn.example"}, cdn.Listener.Addr().String())
	s.Proxy = &Proxy{Key: proxyKey, HTTP: s.Upstream, Lampac: s.L, Base: StreamBase}
	x := &ChannelExtras{Online: s, TMDB: &fakeCatalog{}}
	defer x.Close()
	ctx := context.Background()
	st, err := x.Resolve(ctx, 7, addonkit.ChannelTitle{Key: "tmdb:movie:438631", Kind: "movie", TMDBID: 438631, Year: 2021})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range asked {
		if strings.Contains(p, "torrserver") {
			t.Fatalf("the torrent source was asked: %v", asked)
		}
	}
	if !strings.HasPrefix(st.Path, ChannelPrefix) || strings.Count(st.Path, "/") != 3 || strings.Contains(st.Path, "127.0.0.1") ||
		strings.Contains(st.Path, StreamBase) || st.Release == nil {
		t.Fatalf("%+v: not a socket route under %s", st, ChannelPrefix)
	}
	fetch := func(method, path string) (int, string) {
		w := httptest.NewRecorder()
		x.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w.Code, w.Body.String()
	}
	if code, b := fetch("GET", st.Path); code != 200 || b != body {
		t.Fatalf("socket route: %d %q", code, b[:min(len(b), 60)])
	}
	if code, _ := fetch("HEAD", st.Path); code != 200 {
		t.Fatalf("HEAD: %d", code)
	}
	if code, _ := fetch("POST", st.Path); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", code)
	}
	dir, _ := path.Split(st.Path)
	if code, _ := fetch("GET", ChannelPrefix+strings.Repeat("0", 32)+"/"+path.Base(st.Path)); code != 404 {
		t.Fatalf("another token: %d", code)
	}
	st.Release()
	if code, _ := fetch("GET", st.Path); code != 404 {
		t.Fatalf("after Release: %d", code)
	}

	// an HLS stream: the playlist's URIs are relative to its folder
	mu.Lock()
	streamURL = "http://cdn.example:" + port + "/h/index.m3u8"
	mu.Unlock()
	hl, err := x.Resolve(ctx, 9, addonkit.ChannelTitle{Key: "tmdb:movie:3", Kind: "movie", TMDBID: 3, Year: 2021})
	if err != nil {
		t.Fatal(err)
	}
	// ffmpeg's HLS demuxer opens only URLs with a media extension
	// (allowed_extensions, extension_picky): the route says .m3u8, each URI
	// its own kind
	if !strings.HasSuffix(hl.Path, ".m3u8") {
		t.Fatalf("stream route %q has no .m3u8", hl.Path)
	}
	code, pl := fetch("GET", hl.Path)
	if code != 200 || !strings.HasPrefix(pl, "#EXTM3U") {
		t.Fatalf("playlist %d %q", code, pl)
	}
	dir, _ = path.Split(hl.Path)
	var uris []string
	for _, line := range strings.Split(pl, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			uris = append(uris, line)
		}
	}
	if len(uris) != 2 {
		t.Fatalf("uris %q", pl)
	}
	for i, u := range uris {
		if strings.HasPrefix(u, "/") || strings.Contains(u, "/") || strings.Contains(u, "://") {
			t.Fatalf("uri %q is not relative to the playlist", u)
		}
		if !strings.HasSuffix(u, ".ts") {
			t.Fatalf("segment uri %q has no .ts", u)
		}
		want := []string{"G-/h/seg1.ts", "G-/abs/seg2.ts"}[i]
		if code, b := fetch("GET", dir+u); code != 200 || b != want {
			t.Fatalf("segment %q: %d %q", u, code, b)
		}
	}
	// only the one extension the relay gave is cut: a double or a foreign
	// one leaves a blob the proxy does not know
	bare := strings.TrimSuffix(uris[0], ".ts")
	for _, u := range []string{bare + ".ts.ts", bare + ".m3u8.ts", bare + ".exe", bare + ".ts.exe"} {
		if code, b := fetch("GET", dir+u); code != 403 {
			t.Fatalf("segment %q: %d %q, want 403", u, code, b)
		}
	}
	open, _, ok := x.streamOf(hl.Path)
	if !ok {
		t.Fatal("no open stream")
	}
	if !x.Release(hl.Path) || x.Release(hl.Path) {
		t.Fatal("Release by path: once")
	}
	if s.Proxy.Close(open.user, open.sid) {
		t.Fatal("the proxy session outlived the release")
	}
	if code, _ := fetch("GET", dir+uris[0]); code != 404 {
		t.Fatalf("segment after release: %d", code)
	}

	// a stream that is TorrServer's is never played
	mu.Lock()
	streamURL = "http://cdn.example:" + port + "/stream/film.mkv?link=0123456789abcdef0123456789abcdef01234567&index=1&play"
	mu.Unlock()
	_, err = x.Resolve(ctx, 8, addonkit.ChannelTitle{Key: "tmdb:movie:2", Kind: "movie", TMDBID: 2, Year: 2021}) // another title: not cached
	if !errors.Is(err, ErrRefusedStream) {
		t.Fatalf("TorrServer stream: %v", err)
	}
}
