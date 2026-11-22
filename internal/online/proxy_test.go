package online

import (
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
)

var proxyKey = []byte("proxy-key-0123456789abcdef012345")

// testNet is a guarded upstream client in which every name in names
// resolves to 127.0.0.1 and only the addresses in reach (test servers on
// loopback) are let through besides the Lampac origin lampac.
func testNet(t *testing.T, lampac string, names []string, reach ...string) *http.Client {
	t.Helper()
	ok := map[netip.AddrPort]bool{}
	for _, a := range reach {
		ok[netip.MustParseAddrPort(a)] = true
	}
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	c := newGuarded(guardConfig{lampac: lampac,
		resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if known[host] {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		},
		allow: func(a netip.AddrPort) bool { return ok[a] }})
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// portOf is the port of an httptest server.
func portOf(s *httptest.Server) string {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	return p
}

// get asks the proxy for path ("/api/addons/online/x/media/<sid>/<blob>[?mk=…]") as user uid.
func get(p *Proxy, uid int64, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r = r.WithContext(WithUser(r.Context(), User{ID: uid}))
	rest := strings.TrimPrefix(r.URL.Path, "/api/addons/online/x/media/")
	sid, blob, _ := strings.Cut(rest, "/")
	w := httptest.NewRecorder()
	p.Serve(w, r, sid, blob)
	return w
}

// proxyPaths are the proxy paths in a rewritten playlist, in order.
func proxyPaths(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		for {
			i := strings.Index(line, "/api/addons/online/x/media/")
			if i < 0 {
				break
			}
			end := strings.IndexAny(line[i:], "\" ")
			if end < 0 {
				end = len(line) - i
			}
			out = append(out, line[i:i+end])
			line = line[i+end:]
		}
	}
	return out
}

func TestProxyRejectsForeignAndTamperedBlob(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("seg")) }))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	a, err := p.Open(1, Resolved{Kind: "mp4", URL: base + "/a.mp4"}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Open(2, Resolved{Kind: "mp4", URL: base + "/b.mp4"}, "")
	if err != nil {
		t.Fatal(err)
	}
	pa := p.URL(a, base+"/a.mp4", "")
	if w := get(p, 1, pa); w.Code != 200 || w.Body.String() != "seg" {
		t.Fatalf("own blob: %d %s", w.Code, w.Body)
	}
	// session A's blob on session B (as B's owner)
	blobA := pa[strings.LastIndex(pa, "/")+1:]
	if w := get(p, 2, "/api/addons/online/x/media/"+b+"/"+blobA); w.Code != 403 {
		t.Fatalf("foreign blob: %d", w.Code)
	}
	// a flipped byte, in the URL part and in the MAC
	for _, i := range []int{3, len(blobA) - 2} {
		c := []byte(blobA)
		if c[i] == 'A' {
			c[i] = 'B'
		} else {
			c[i] = 'A'
		}
		if w := get(p, 1, "/api/addons/online/x/media/"+a+"/"+string(c)); w.Code != 403 {
			t.Fatalf("flipped byte %d: %d", i, w.Code)
		}
	}
	// correctly sealed, but for a URL this session never handed out
	forged := p.seal(a, base+"/never.ts")
	if w := get(p, 1, "/api/addons/online/x/media/"+a+"/"+forged); w.Code != 403 {
		t.Fatalf("never-seen URL: %d", w.Code)
	}
	// someone else's session, and no session at all
	if w := get(p, 2, pa); w.Code != 404 || !strings.Contains(w.Body.String(), "Сеанс не найден") {
		t.Fatalf("another user's sid: %d %s", w.Code, w.Body)
	}
	if w := get(p, 1, "/api/addons/online/x/media/nope/"+blobA); w.Code != 404 {
		t.Fatalf("unknown sid: %d", w.Code)
	}
}

func TestProxyHeadersAndRange(t *testing.T) {
	var bad atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://player.example/" || r.Header.Get("User-Agent") != UserAgent || r.Header.Get("X") != "" {
			bad.Add(1)
			w.WriteHeader(403)
			return
		}
		if r.Header.Get("Range") == "bytes=0-9" {
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-9/100")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", "10")
			w.Header().Set("Set-Cookie", "upstream=1")
			w.WriteHeader(206)
			w.Write([]byte("0123456789"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<script>x</script>"))
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	// the answer's headers, as Lampac gives them (one with CR/LF)
	it, ok := playItem(itemDTO{Method: "play", URL: base + "/v.mp4",
		Headers: strMap{"referer": "https://player.example/", "X": "1", "Origin": "a\r\nX: y"}})
	if !ok {
		t.Fatal("play item")
	}
	h := sanitizeHeaders(it.Headers)
	if h.Get("Origin") != "" || h.Get("X") != "" || h.Get("User-Agent") != UserAgent {
		t.Fatalf("headers: %v", h)
	}
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, err := p.Open(1, Resolved{Kind: "mp4", URL: base + "/v.mp4", Headers: h}, "")
	if err != nil {
		t.Fatal(err)
	}
	w := get(p, 1, p.URL(sid, base+"/v.mp4", ""), "Range", "bytes=0-9")
	if w.Code != 206 || w.Body.String() != "0123456789" || w.Header().Get("Content-Range") != "bytes 0-9/100" ||
		w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Content-Type") != "video/mp4" || w.Header().Get("Content-Length") != "10" {
		t.Fatalf("range: %d %v %q", w.Code, w.Header(), w.Body)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("upstream cookie passed to the client")
	}
	// an upstream HTML answer is never served as HTML on Vault's origin
	w = get(p, 1, p.URL(sid, base+"/v.mp4", ""))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("html: %d %v", w.Code, w.Header())
	}
	if bad.Load() != 0 {
		t.Fatalf("%d upstream requests without the session's headers", bad.Load())
	}
}

// A byte-range answer says so to the player: Accept-Ranges: bytes after a
// 206 or a Content-Range even when the source leaves the header out, and
// after an upstream's own bytes in any case; a source that says none (or
// nothing, on a whole answer) is not promoted.
func TestProxyAcceptRanges(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		switch r.URL.Path {
		case "/partial.mp4": // a 206 with no Accept-Ranges
			w.Header().Set("Content-Range", "bytes 0-9/100")
			w.WriteHeader(206)
			w.Write([]byte("0123456789"))
		case "/shout.mp4": // the header in another case
			w.Header().Set("Accept-Ranges", "Bytes")
			w.Write([]byte("0123456789"))
		case "/whole.mp4": // a whole answer that says nothing of ranges
			w.Write([]byte("0123456789"))
		case "/none.mp4":
			w.Header().Set("Accept-Ranges", "none")
			w.Write([]byte("0123456789"))
		case "/past.mp4":
			w.Header().Set("Content-Range", "bytes */100")
			w.WriteHeader(416)
		}
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, err := p.Open(1, Resolved{Kind: "mp4", URL: base + "/whole.mp4", Headers: http.Header{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		code int
		want string
		hdr  []string
	}{
		{"/partial.mp4", 206, "bytes", []string{"Range", "bytes=0-9"}},
		{"/shout.mp4", 200, "bytes", nil},
		{"/whole.mp4", 200, "", nil},
		{"/none.mp4", 200, "none", nil},
		{"/past.mp4", 416, "bytes", []string{"Range", "bytes=500-"}},
	} {
		w := get(p, 1, p.URL(sid, base+c.path, ""), c.hdr...)
		if w.Code != c.code || w.Header().Get("Accept-Ranges") != c.want {
			t.Errorf("%s: %d Accept-Ranges %q, want %d %q", c.path, w.Code, w.Header().Get("Accept-Ranges"), c.code, c.want)
		}
	}
}

// One INFO line per play start: the source, how long resolving took and how
// long after the session opened the first body byte went out. An error
// answer is no start, and later requests add no line.
func TestProxyLogsPlayStartOnce(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone.mp4" {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("0123456789"))
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	var logged strings.Builder
	var mu sync.Mutex
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String()),
		Log: slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return logged.Write(b)
		}), nil)),
		Now: func() time.Time { return now }}
	sid, err := p.Open(1, Resolved{Kind: "mp4", URL: base + "/v.mp4", Headers: http.Header{}, Took: 1234 * time.Millisecond,
		Ctx: RefContext{Kind: "movie", TMDBID: 1, Source: "alpha"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	lines := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, l := range strings.Split(logged.String(), "\n") {
			if strings.Contains(l, "play started") {
				out = append(out, l)
			}
		}
		return out
	}
	// an expired link answers an error: no start yet
	if w := get(p, 1, p.URL(sid, base+"/gone.mp4", "")); w.Code != 410 {
		t.Fatalf("gone: %d", w.Code)
	}
	if n := len(lines()); n != 0 {
		t.Fatalf("%d lines after an error answer", n)
	}
	now = now.Add(2500 * time.Millisecond)
	if w := get(p, 1, p.URL(sid, base+"/v.mp4", "")); w.Code != 200 || w.Body.String() != "0123456789" {
		t.Fatalf("first: %d %q", w.Code, w.Body)
	}
	got := lines()
	if len(got) != 1 || !strings.Contains(got[0], "level=INFO") || !strings.Contains(got[0], "source=alpha") ||
		!strings.Contains(got[0], "kind=mp4") || !strings.Contains(got[0], "resolve_ms=1234") || !strings.Contains(got[0], "first_byte_ms=2500") {
		t.Fatalf("lines: %q", got)
	}
	if strings.Contains(got[0], base) || strings.Contains(got[0], sid) {
		t.Fatalf("the line names a URL or the session: %q", got[0])
	}
	// the next request of the session adds none
	get(p, 1, p.URL(sid, base+"/v.mp4", ""))
	if n := len(lines()); n != 1 {
		t.Fatalf("%d lines after a second request", n)
	}
}

// writerFunc is an io.Writer of a func.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func TestUpstream403BecomesExpired(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write(fixture(t, "key-map.m3u8"))
		case "/v/seg1.ts", "/v/init.mp4":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write([]byte("G-segment"))
		case "/v/seg2.ts":
			w.WriteHeader(403)
		default:
			w.WriteHeader(503)
		}
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, err := p.Open(1, Resolved{Kind: "hls", URL: base + "/v/index.m3u8"}, "")
	if err != nil {
		t.Fatal(err)
	}
	w := get(p, 1, p.URL(sid, base+"/v/index.m3u8", ""))
	if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "#EXTM3U") || w.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("playlist: %d %v %s", w.Code, w.Header(), w.Body)
	}
	paths := proxyPaths(w.Body.String())
	if len(paths) != 3 {
		t.Fatalf("paths %v in\n%s", paths, w.Body)
	}
	// (the media key in rewritten URIs: TestMediaKeyOpensOnlineProxy)
	if w := get(p, 1, paths[1]); w.Code != 200 || w.Body.String() != "G-segment" {
		t.Fatalf("segment 1: %d %s", w.Code, w.Body)
	}
	w = get(p, 1, paths[2])
	if w.Code != 410 || w.Header().Get("X-Vault-Online") != "expired" {
		t.Fatalf("segment 2: %d %v", w.Code, w.Header())
	}
	// a 5xx is the source failing, not an expired link
	if w := get(p, 1, p.URL(sid, base+"/v/x.ts", "")); w.Code != 502 || w.Header().Get("X-Vault-Online") != "" ||
		!strings.Contains(w.Body.String(), "Источник не ответил") {
		t.Fatalf("5xx: %d %v %s", w.Code, w.Header(), w.Body)
	}
}

func TestProxyStreamCapPerSession(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		started.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "mp4", URL: base + "/v.mp4"}, "")
	path := p.URL(sid, base+"/v.mp4", "")
	done := make(chan int, streamsPerSession)
	for i := 0; i < streamsPerSession; i++ {
		go func() { done <- get(p, 1, path).Code }()
	}
	waitFor(t, func() bool { return started.Load() == streamsPerSession })
	if w := get(p, 1, path); w.Code != 429 {
		t.Fatalf("over the cap: %d", w.Code)
	}
	close(release)
	for i := 0; i < streamsPerSession; i++ {
		if c := <-done; c != 200 {
			t.Fatalf("relay %d", c)
		}
	}
	if w := get(p, 1, path); w.Code != 200 {
		t.Fatalf("after the relays ended: %d", w.Code)
	}
	if streamsPerSession != 6 {
		t.Fatalf("cap %d, the torrent-stream cap is 6", streamsPerSession)
	}
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestProxyKeepsURLsWithoutUID(t *testing.T) {
	// a Lampac playlist that echoes the uid into its links: the signed
	// URLs never hold it (it is put back on the fetch)
	var gotUID atomic.Value
	lamp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUID.Store(r.URL.Query().Get("uid"))
		switch r.URL.Path {
		case "/proxy/abc/index.m3u8":
			w.Write([]byte("#EXTM3U\n#EXTINF:4,\n/proxy/abc/1.ts?uid=secret&x=1\n#EXT-X-ENDLIST\n"))
		default:
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write([]byte("G\x00seg"))
		}
	}))
	defer lamp.Close()
	l, _ := NewLampac("http://lampac.test:" + portOf(lamp) + "/?uid=secret")
	p := &Proxy{Key: proxyKey, Lampac: l, HTTP: testNet(t, "lampac.test:"+portOf(lamp), []string{"lampac.test"})}
	root := "http://lampac.test:" + portOf(lamp) + "/proxy/abc/index.m3u8"
	sid, _ := p.Open(1, Resolved{Kind: "hls", URL: root}, "")
	w := get(p, 1, p.URL(sid, root, ""))
	if w.Code != 200 || gotUID.Load() != "secret" {
		t.Fatalf("lampac playlist: %d uid=%v %s", w.Code, gotUID.Load(), w.Body)
	}
	paths := proxyPaths(w.Body.String())
	if len(paths) != 1 {
		t.Fatalf("%v", paths)
	}
	blob := paths[0][strings.LastIndex(paths[0], "/")+1:]
	raw, ok := p.verify(p.sessions[sid], blob)
	if !ok || strings.Contains(raw, "uid") || strings.Contains(raw, "secret") {
		t.Fatalf("a sealed URL holds the uid: %v %s", ok, raw)
	}
	if u, _ := url.Parse(string(raw)); u.Query().Get("x") != "1" {
		t.Fatalf("query lost: %s", raw)
	}
	if w := get(p, 1, paths[0]); w.Code != 200 || gotUID.Load() != "secret" {
		t.Fatalf("lampac segment: %d", w.Code)
	}
}

func TestProxyBigPlaylist(t *testing.T) {
	// a media playlist of a long film with long signed segment URLs (the
	// shape of a real source: ~3000 segments of ~3.4 KB URLs, over 4 MiB)
	seg := "/seg/" + strings.Repeat("a", 3400) + ".ts"
	var big strings.Builder
	big.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
	for i := 0; i < 1600; i++ {
		big.WriteString("#EXTINF:4.0,\n" + seg + "?n=" + strconv.Itoa(i) + "\n")
	}
	big.WriteString("#EXT-X-ENDLIST\n")
	huge := "#EXTM3U\n" + strings.Repeat("#EXTINF:4.0,\n"+seg+"\n", 5200)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big.m3u8":
			w.Write([]byte(big.String()))
		case "/huge.m3u8":
			w.Write([]byte(huge))
		}
	}))
	defer cdn.Close()
	if big.Len() <= 4<<20 || len(huge) <= maxPlaylist {
		t.Fatalf("fixture sizes %d %d", big.Len(), len(huge))
	}
	base := "http://cdn.example:" + portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "hls", URL: base + "/big.m3u8"}, "")
	w := get(p, 1, p.URL(sid, base+"/big.m3u8", ""))
	if w.Code != 200 || len(proxyPaths(w.Body.String())) != 1600 {
		t.Fatalf("big playlist: %d", w.Code)
	}
	if w := get(p, 1, proxyPaths(w.Body.String())[1599]); w.Code != 200 {
		t.Fatalf("its last segment: %d", w.Code)
	}
	if w := get(p, 1, p.URL(sid, base+"/huge.m3u8", "")); w.Code != 502 {
		t.Fatalf("over the cap: %d", w.Code)
	}
}

func TestSourcePlaylistNeverReachesLampac(t *testing.T) {
	var lampHits atomic.Int32
	lamp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lampHits.Add(1)
		switch r.URL.Path {
		case "/proxy/json.ts": // Lampac's own JSON, with the uid in its links
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"url":"http://lampac.test/lite/x?uid=secret"}`))
		case "/proxy/mid.mp4": // a range from the middle of a film
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("{\x00\x01<binary"))
		case "/proxy/echo.m3u8":
			w.Write([]byte("#EXTM3U\n# uid=secret\n#EXTINF:4,\n/proxy/1.ts\n"))
		case "/proxy/ok.m3u8":
			w.Write([]byte("#EXTM3U\n#EXT-X-MAP:URI=\"/proxy/init.mp4?uid=secret\"\n#EXTINF:4,\n/proxy/1.ts\n"))
		default:
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write([]byte("G\x00seg"))
		}
	}))
	defer lamp.Close()
	lport := portOf(lamp)
	origin := "http://lampac.test:" + lport
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"" + origin + "/lite/events?id=1\"\n#EXTINF:4,\n" + origin +
			"/lite/events?id=1\n#EXTINF:4,\n" + origin + "/proxy/x.ts\n#EXTINF:4,\nseg.ts\n"))
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	l, _ := NewLampac(origin + "/?uid=secret")
	p := &Proxy{Key: proxyKey, Lampac: l,
		HTTP: testNet(t, "lampac.test:"+lport, []string{"lampac.test", "cdn.example"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "hls", URL: base + "/index.m3u8"}, "")
	w := get(p, 1, p.URL(sid, base+"/index.m3u8", ""))
	paths := proxyPaths(w.Body.String())
	if w.Code != 200 || len(paths) != 1 || !strings.Contains(w.Body.String(), "URI=\"\"") || strings.Count(w.Body.String(), "\n#\n") != 2 {
		t.Fatalf("a source playlist kept Lampac URIs: %d\n%s", w.Code, w.Body)
	}
	if raw, _ := p.verify(p.sessions[sid], paths[0][strings.LastIndex(paths[0], "/")+1:]); raw != base+"/seg.ts" {
		t.Fatalf("kept %s", raw)
	}
	if lampHits.Load() != 0 {
		t.Fatalf("Lampac was called %d times", lampHits.Load())
	}
	// a session resolved through Lampac: its playlists keep Lampac URIs
	// (without the uid), but Lampac's non-media answers and a playlist
	// echoing the uid are never relayed
	sid2, _ := p.Open(2, Resolved{Kind: "hls", URL: origin + "/proxy/ok.m3u8"}, "")
	w = get(p, 2, p.URL(sid2, origin+"/proxy/ok.m3u8", ""))
	if w.Code != 200 || len(proxyPaths(w.Body.String())) != 2 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("lampac playlist: %d %s", w.Code, w.Body)
	}
	for _, pp := range proxyPaths(w.Body.String()) {
		if g := get(p, 2, pp); g.Code != 200 || strings.Contains(g.Body.String(), "secret") {
			t.Fatalf("lampac URI: %d %s", g.Code, g.Body)
		}
	}
	sid4, _ := p.Open(4, Resolved{Kind: "mp4", URL: origin + "/proxy/mid.mp4"}, "")
	if g := get(p, 4, p.URL(sid4, origin+"/proxy/mid.mp4", "")); g.Code != 200 {
		t.Fatalf("binary range starting with {: %d", g.Code)
	}
	for _, bad := range []string{"/proxy/json.ts", "/proxy/echo.m3u8"} {
		sid3, _ := p.Open(3, Resolved{Kind: "hls", URL: origin + bad}, "")
		if g := get(p, 3, p.URL(sid3, origin+bad, "")); g.Code != 502 || strings.Contains(g.Body.String(), "secret") {
			t.Fatalf("%s: %d %s", bad, g.Code, g.Body)
		}
	}
}

func TestRewriteBounds(t *testing.T) {
	tiny := "#EXTM3U\n" + strings.Repeat("a\n", (maxPlaylist-16)/2)
	many := "#EXTM3U\n" + strings.Repeat("#EXTINF:1,\nx.ts\n", maxPlaylistURIs+1)
	long := "#EXTM3U\n#EXTINF:1,\n/" + strings.Repeat("l", 6400) + ".ts\n#EXTINF:1,\n/" + strings.Repeat("h", maxLongURL) +
		".ts\n#EXTINF:1,\nok.ts\n"
	longSegs := "#EXTM3U\n" + strings.Repeat("#EXTINF:1,\n/"+strings.Repeat("s", 6000)+".ts\n", maxLongBytes/6000+1)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tiny.m3u8":
			w.Write([]byte(tiny))
		case "/many.m3u8":
			w.Write([]byte(many))
		case "/long.m3u8":
			w.Write([]byte(long))
		case "/longsegs.m3u8":
			w.Write([]byte(longSegs))
		}
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "hls", URL: base + "/tiny.m3u8"}, "")
	t0 := time.Now()
	for _, name := range []string{"/tiny.m3u8", "/many.m3u8"} {
		if w := get(p, 1, p.URL(sid, base+name, "")); w.Code != 502 || w.Body.Len() > 200 {
			t.Fatalf("%s: %d (%d bytes)", name, w.Code, w.Body.Len())
		}
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("refusing took %v", d)
	}
	// a URL too long to seal (Lampac's ~6.4 KB) is kept by reference: a
	// short blob; one over maxLongURL is dropped
	w := get(p, 1, p.URL(sid, base+"/long.m3u8", ""))
	pp := proxyPaths(w.Body.String())
	if w.Code != 200 || len(pp) != 2 || len(pp[0]) > 300 {
		t.Fatalf("long URIs: %d %d", w.Code, len(pp))
	}
	if raw, ok := p.verify(p.sessions[sid], pp[0][strings.LastIndex(pp[0], "/")+1:]); !ok || len(raw) < 6400 {
		t.Fatalf("long reference: %v %d", ok, len(raw))
	}
	if raw, _ := p.verify(p.sessions[sid], pp[1][strings.LastIndex(pp[1], "/")+1:]); raw != base+"/ok.ts" {
		t.Fatalf("%s", raw)
	}
	// a root URL of 6.4 KB too
	root := base + "/" + strings.Repeat("r", 6400) + ".m3u8"
	if u := p.URL(sid, root, ""); len(u) > 300 {
		t.Fatalf("long root: %d", len(u))
	}
	// the longest blob stays under ~6 KB
	if n := p.pathLen(sid, maxBlobURL); n > 6000 {
		t.Fatalf("longest path %d", n)
	}
	// past its token bytes a session seals its URLs again; more long
	// URLs than it keeps then: 410, the player starts anew
	p.mu.Lock()
	p.sessions[sid].tokBytes = maxTokenBytes
	p.mu.Unlock()
	if w := get(p, 1, p.URL(sid, base+"/longsegs.m3u8", "")); w.Code != 410 {
		t.Fatalf("long URLs over the budget: %d", w.Code)
	}
	// the session handed out all it may: 410, the player starts anew
	p.mu.Lock()
	p.sessions[sid].tokIdx = nil // (its known tokens would still be handed out)
	for i := 0; len(p.sessions[sid].seen) < maxSeen; i++ {
		p.sessions[sid].seen[urlID(sid, strconv.Itoa(i))] = struct{}{}
	}
	p.mu.Unlock()
	if w := get(p, 1, p.URL(sid, base+"/long.m3u8", "")); w.Code != 410 || w.Header().Get("X-Vault-Online") != "expired" {
		t.Fatalf("seen full: %d", w.Code)
	}
	// one rewrite per session at a time: a second one waits
	sid2, _ := p.Open(2, Resolved{Kind: "hls", URL: base + "/long.m3u8"}, "")
	p.mu.Lock()
	s2 := p.sessions[sid2]
	p.mu.Unlock()
	s2.rewrite <- struct{}{}
	r := httptest.NewRequest("GET", "/x", nil)
	ctx, cancel := context.WithTimeout(WithUser(r.Context(), User{ID: 2}), 200*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	path := p.URL(sid2, base+"/long.m3u8", "")
	p.Serve(rec, r.WithContext(ctx), sid2, path[strings.LastIndex(path, "/")+1:])
	if rec.Body.Len() != 0 || ctx.Err() == nil {
		t.Fatalf("a second rewrite ran: %d %s", rec.Code, rec.Body)
	}
	<-s2.rewrite
	if w := get(p, 2, path); w.Code != 200 {
		t.Fatalf("after the first: %d", w.Code)
	}
}

func TestRewriteQuotedAttrs(t *testing.T) {
	base, _ := url.Parse("https://cdn.example/x/index.m3u8")
	in := "#EXT-X-MEDIA:TYPE=AUDIO,NAME=\"a,URI=\\\"evil\",URI=\"rus.m3u8\"\n"
	out := rewriteM3U8(in, base, testSign, "")
	if !strings.Contains(out, `NAME="a,URI=\"evil"`) || !strings.Contains(out, `URI="`+testSign("https://cdn.example/x/rus.m3u8")+`"`) {
		t.Fatalf("%s", out)
	}
}

func TestCookieOnlyToTheSourceSite(t *testing.T) {
	var got atomic.Value
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Cookie"))
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write([]byte("G\x00"))
	}))
	defer cdn.Close()
	port := portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example", "seg.cdn.example", "other.test"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "mp4", URL: "http://cdn.example:" + port + "/v.mp4", Headers: http.Header{"Cookie": {"s=1"}}}, "")
	for host, want := range map[string]string{"cdn.example": "s=1", "seg.cdn.example": "s=1", "other.test": ""} {
		u := "http://" + host + ":" + port + "/x.ts"
		if w := get(p, 1, p.URL(sid, u, "")); w.Code != 200 || got.Load() != want {
			t.Fatalf("%s: %d cookie %q", host, w.Code, got.Load())
		}
	}
}

// TestWriteErrorLanguage: the proxy's errors follow the request's language.
func TestWriteErrorLanguage(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	writeError(w, r, http.StatusNotFound, ErrNoSession.Error())
	if !strings.Contains(w.Body.String(), "Сеанс не найден") {
		t.Fatalf("ru: %s", w.Body)
	}
	w = httptest.NewRecorder()
	writeError(w, r.WithContext(i18n.WithLanguage(r.Context(), i18n.EN)), http.StatusNotFound, ErrNoSession.Error())
	if !strings.Contains(w.Body.String(), "Session not found") {
		t.Fatalf("en: %s", w.Body)
	}
}

// The root playlist the probe read is served on the session's first
// request of it, not fetched again; later requests and a stale one fetch.
func TestRootPlaylistFromProbe(t *testing.T) {
	var fetches atomic.Int32
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n" + strings.Repeat("#EXTINF:4,\nseg.ts\n", 200) + "#EXT-X-ENDLIST\n"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v/index.m3u8" {
			fetches.Add(1)
			w.Write([]byte(body))
			return
		}
		w.WriteHeader(404)
	}))
	defer cdn.Close()
	root := "http://cdn.example:" + portOf(cdn) + "/v/index.m3u8"
	up := testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())
	s := &Service{Upstream: up}
	s.init()
	pr, err := s.probe(context.Background(), root, http.Header{})
	if err != nil || pr.kind != "hls" || pr.root == nil {
		t.Fatalf("probe: %+v %v", pr, err)
	}
	<-pr.root.done
	if string(pr.root.b) != body || pr.root.base.String() != root {
		t.Fatalf("probe read: %d bytes from %v", len(pr.root.b), pr.root.base)
	}
	now := time.Now()
	p := &Proxy{Key: proxyKey, HTTP: up, Now: func() time.Time { return now }}
	r := Resolved{Kind: "hls", URL: root, root: pr.root}
	sid, _ := p.Open(1, r, "")
	first := get(p, 1, p.URL(sid, root, ""))
	if first.Code != 200 || fetches.Load() != 1 || len(proxyPaths(first.Body.String())) != 200 {
		t.Fatalf("first: %d fetches %d", first.Code, fetches.Load())
	}
	second := get(p, 1, p.URL(sid, root, ""))
	if second.Code != 200 || fetches.Load() != 2 {
		t.Fatalf("second: %d fetches %d", second.Code, fetches.Load())
	}
	// past rootTTL the probe's read is not used
	sid2, _ := p.Open(1, r, sid)
	now = now.Add(rootTTL + time.Second)
	if w := get(p, 1, p.URL(sid2, root, "")); w.Code != 200 || fetches.Load() != 3 {
		t.Fatalf("stale: %d fetches %d", w.Code, fetches.Load())
	}
}

// A media playlist of ~1000 long signed segment URLs: tokens instead of
// sealed URLs, and gzip when the client takes it.
func TestPlaylistTokensAndGzip(t *testing.T) {
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"init.mp4\"\n")
	for i := 0; i < 1000; i++ {
		// the shape of a real CDN's: ~3.6 KB of signature per segment
		pl.WriteString("#EXTINF:4.0,\n/seg/" + strconv.Itoa(i) + ".ts?sig=" + strings.Repeat(strconv.Itoa(i%10), 3600) + "\n")
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v.m3u8":
			w.Write([]byte(pl.String()))
		default:
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write([]byte("G" + r.URL.Path))
		}
	}))
	defer cdn.Close()
	root := "http://cdn.example:" + portOf(cdn) + "/v.m3u8"
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	// before: every URI sealed whole (a session past its token bytes)
	old, _ := p.Open(1, Resolved{Kind: "hls", URL: root}, "")
	p.mu.Lock()
	p.sessions[old].tokBytes = maxTokenBytes
	p.mu.Unlock()
	sealed := get(p, 1, p.URL(old, root, ""))
	if sealed.Code != 200 || len(proxyPaths(sealed.Body.String())) != 1001 || strings.Contains(sealed.Body.String(), "/~") {
		t.Fatalf("sealed: %d", sealed.Code)
	}
	sid, _ := p.Open(2, Resolved{Kind: "hls", URL: root}, "")
	plain := get(p, 2, p.URL(sid, root, ""))
	paths := proxyPaths(plain.Body.String())
	if plain.Code != 200 || len(paths) != 1001 || plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("tokens: %d %v", plain.Code, plain.Header())
	}
	zipped := get(p, 2, p.URL(sid, root, ""), "Accept-Encoding", "br, gzip")
	if zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip: %v", zipped.Header())
	}
	zlen := zipped.Body.Len()
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	unzipped, _ := io.ReadAll(zr)
	if string(unzipped) != plain.Body.String() {
		t.Fatal("the gzip body differs (the same playlist again has the same tokens)")
	}
	t.Logf("upstream %d B; sealed %d B; tokens %d B; tokens+gzip %d B",
		pl.Len(), sealed.Body.Len(), plain.Body.Len(), zlen)
	if sealed.Body.Len() < pl.Len() || plain.Body.Len() > 100<<10 || zlen > 20<<10 {
		t.Fatalf("sizes: sealed %d, tokens %d, gzip %d", sealed.Body.Len(), plain.Body.Len(), zlen)
	}
	// a token opens its URL, on its own session only
	if w := get(p, 2, paths[1000]); w.Code != 200 || w.Body.String() != "G/seg/999.ts" {
		t.Fatalf("last segment: %d %s", w.Code, w.Body)
	}
	if w := get(p, 2, paths[0]); w.Code != 200 || w.Body.String() != "G/init.mp4" {
		t.Fatalf("map: %d %s", w.Code, w.Body)
	}
	tok := paths[5][strings.LastIndex(paths[5], "/")+1:]
	if w := get(p, 1, "/api/addons/online/x/media/"+old+"/"+tok); w.Code != 403 {
		t.Fatalf("another session's token: %d", w.Code)
	}
	for _, bad := range []string{"~", "~zzzz", "~-1", "~01", "~1X", "~1.", "~" + strings.Repeat("9", 20)} {
		if w := get(p, 2, "/api/addons/online/x/media/"+sid+"/"+bad); w.Code != 403 {
			t.Fatalf("%q: %d", bad, w.Code)
		}
	}
	// tokens die with the session, and their bytes are given back
	p.mu.Lock()
	total := p.tokenTotal
	p.mu.Unlock()
	if total <= 1000*3600 {
		t.Fatalf("token bytes %d", total)
	}
	if !p.Close(2, sid) {
		t.Fatal("close")
	}
	if w := get(p, 2, paths[3]); w.Code != 404 {
		t.Fatalf("closed session's token: %d", w.Code)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tokenTotal != 0 { // (the sealed session's tokBytes was set by hand, never counted)
		t.Fatalf("token bytes after close %d", p.tokenTotal)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for h, want := range map[string]bool{"": false, "gzip": true, "deflate, gzip;q=0.5": true, "GZIP": true, "gzip;q=0": false,
		"br": false, "gzip; q=0.000": false, "x-gzip": false} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", h)
		if acceptsGzip(r) != want {
			t.Fatalf("%q: %v", h, !want)
		}
	}
}

// A session dropped while one of its playlists is rewritten (a quality
// switch, «Другой источник», the per-user cap) mints no tokens after the
// drop, and every token byte returns: tokenTotal ends at 0.
func TestTokenTotalSurvivesDropDuringRewrite(t *testing.T) {
	// the deterministic case: tokens held for a rewrite, then a drop
	p := &Proxy{Key: proxyKey}
	sid, _ := p.Open(1, Resolved{Kind: "hls", URL: "http://cdn.example/v.m3u8"}, "")
	p.mu.Lock()
	s := p.sessions[sid]
	if _, ok := p.token(s, "http://cdn.example/a.ts"); !ok {
		t.Fatal("token")
	}
	s.tokHeld += 100
	p.tokenTotal += 100
	p.drop(sid)
	if _, ok := p.token(s, "http://cdn.example/b.ts"); ok {
		t.Error("a dropped session minted a token")
	}
	if p.tokenTotal != 0 {
		t.Errorf("token bytes after the drop %d", p.tokenTotal)
	}
	p.mu.Unlock()

	// the race: a playlist of long URLs whose session closes mid-rewrite
	// (the client's first write waits for the close)
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
	for i := 0; i < 1500; i++ {
		pl.WriteString("#EXTINF:4.0,\n/seg/" + strconv.Itoa(i) + ".ts?sig=" + strings.Repeat("s", 3600) + "\n")
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(pl.String()))
	}))
	defer cdn.Close()
	root := "http://cdn.example:" + portOf(cdn) + "/v.m3u8"
	p = &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, _ = p.Open(1, Resolved{Kind: "hls", URL: root}, "")
	r := httptest.NewRequest("GET", p.URL(sid, root, ""), nil)
	r = r.WithContext(WithUser(r.Context(), User{ID: 1}))
	blob := strings.TrimPrefix(r.URL.Path, "/api/addons/online/x/media/"+sid+"/")
	w := &heldWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); p.Serve(w, r, sid, blob) }()
	<-w.started
	p.mu.Lock()
	if p.tokenTotal == 0 {
		t.Error("the rewrite holds no token bytes")
	}
	p.mu.Unlock()
	if !p.Close(1, sid) {
		t.Fatal("close")
	}
	close(w.release)
	<-done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tokenTotal != 0 {
		t.Fatalf("token bytes leaked: %d", p.tokenTotal)
	}
}

// heldWriter's first Write waits for release.
type heldWriter struct {
	*httptest.ResponseRecorder
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (s *heldWriter) Write(b []byte) (int, error) {
	s.once.Do(func() { close(s.started); <-s.release })
	return s.ResponseRecorder.Write(b)
}

// Token bytes are held for a rewrite before it starts: a second rewrite
// meanwhile sees them taken, and never runs out halfway (long URLs
// dropped as gaps in a 200 playlist).
func TestTokenBytesHeldBeforeTheRewrite(t *testing.T) {
	var pl strings.Builder
	pl.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
	for i := 0; i < 1200; i++ {
		pl.WriteString("#EXTINF:4.0,\n/seg/" + strconv.Itoa(i) + ".ts?sig=" + strings.Repeat("s", 4200) + "\n")
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(pl.String()))
	}))
	defer cdn.Close()
	root := "http://cdn.example:" + portOf(cdn) + "/v.m3u8"
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	a, _ := p.Open(1, Resolved{Kind: "hls", URL: root}, "")
	r := httptest.NewRequest("GET", p.URL(a, root, ""), nil)
	r = r.WithContext(WithUser(r.Context(), User{ID: 1}))
	w := &heldWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Serve(w, r, a, strings.TrimPrefix(r.URL.Path, "/api/addons/online/x/media/"+a+"/"))
	}()
	<-w.started
	// the other sessions of the server use all but room for one playlist
	p.mu.Lock()
	held := p.tokenTotal
	other := maxTokenTotal - 2*held + held/2
	p.tokenTotal += other
	p.mu.Unlock()
	b, _ := p.Open(2, Resolved{Kind: "hls", URL: root}, "")
	got := get(p, 2, p.URL(b, root, ""))
	if got.Code == 200 && len(proxyPaths(got.Body.String())) != 1200 {
		t.Fatalf("a playlist with gaps: %d of 1200 URIs", len(proxyPaths(got.Body.String())))
	}
	if got.Code != 200 && got.Code != http.StatusGone {
		t.Fatalf("%d", got.Code)
	}
	close(w.release)
	<-done
	if n := len(proxyPaths(w.Body.String())); n != 1200 {
		t.Fatalf("the first playlist: %d URIs", n)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokenTotal -= other
	for id := range p.sessions {
		p.drop(id)
	}
	if p.tokenTotal != 0 {
		t.Fatalf("token bytes %d", p.tokenTotal)
	}
}

// The probe answers once the first bytes say HLS: a slow CDN's large
// playlist is read in the background, not before playback starts.
func TestProbeAnswersBeforeThePlaylistIsRead(t *testing.T) {
	head := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n" + strings.Repeat("#EXTINF:4,\nseg.ts\n", 100)
	tail := strings.Repeat("#EXTINF:4,\nseg.ts\n", 100) + "#EXT-X-ENDLIST\n"
	release := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(head))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte(tail))
	}))
	defer cdn.Close()
	defer close(release)
	root := "http://cdn.example:" + portOf(cdn) + "/v/index.m3u8"
	s := &Service{Upstream: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	s.init()
	start := time.Now()
	pr, err := s.probe(context.Background(), root, http.Header{})
	if err != nil || pr.kind != "hls" || pr.root == nil || time.Since(start) > time.Second {
		t.Fatalf("probe: %+v %v after %v", pr, err, time.Since(start))
	}
	select {
	case <-pr.root.done:
		t.Fatal("the read ended before the playlist did")
	default:
	}
	release <- struct{}{}
	<-pr.root.done
	if string(pr.root.b) != head+tail {
		t.Fatalf("read %d bytes", len(pr.root.b))
	}
}

// The player gets a master that says it has no closed captions (a source
// master that says nothing of them makes AVFoundation offer an empty «CC»),
// and one that has captions is served as it is.
func TestProxyMasterSaysNoClosedCaptions(t *testing.T) {
	plain := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.64001e,mp4a.40.2\"\nv/media.m3u8\n"
	captioned := "#EXTM3U\n#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",NAME=\"English\",INSTREAM-ID=\"CC1\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,CLOSED-CAPTIONS=\"cc\"\nv/media.m3u8\n"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plain.m3u8":
			w.Write([]byte(plain))
		case "/captioned.m3u8":
			w.Write([]byte(captioned))
		}
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "hls", URL: base + "/plain.m3u8"}, "")
	w := get(p, 1, p.URL(sid, base+"/plain.m3u8", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `CODECS="avc1.64001e,mp4a.40.2",CLOSED-CAPTIONS=NONE`+"\n") {
		t.Fatalf("plain master: %d\n%s", w.Code, w.Body)
	}
	w = get(p, 1, p.URL(sid, base+"/captioned.m3u8", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), "NONE") || !strings.Contains(w.Body.String(), `CLOSED-CAPTIONS="cc"`) {
		t.Fatalf("captioned master: %d\n%s", w.Code, w.Body)
	}
}
