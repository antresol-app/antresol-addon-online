package online

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// countingListener is a server on loopback that counts the connections
// it accepts (none may arrive).
func countingListener(t *testing.T) (addr string, n *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), n
}

func TestProxyBlocksHomeNetwork(t *testing.T) {
	home, hits := countingListener(t)
	_, homePort, _ := net.SplitHostPort(home)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			w.Write([]byte("#EXTM3U\n#EXTINF:4,\nhttp://192.168.1.1/x.ts\n#EXTINF:4,\n/redirect.ts\n#EXTINF:4,\nhttp://127.0.0.1:" + homePort +
				"/api/library\n#EXTINF:4,\nhttp://home.example:" + homePort + "/x.ts\n#EXT-X-ENDLIST\n"))
		case "/redirect.ts":
			http.Redirect(w, r, "http://127.0.0.1:"+homePort+"/api/library", http.StatusFound)
		default:
			w.WriteHeader(404)
		}
	}))
	defer cdn.Close()
	base := "http://cdn.example:" + portOf(cdn)
	// home.example resolves to loopback too (DNS rebinding)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example", "home.example"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "hls", URL: base + "/index.m3u8"}, "")
	w := get(p, 1, p.URL(sid, base+"/index.m3u8", ""))
	if w.Code != 200 {
		t.Fatalf("playlist: %d %s", w.Code, w.Body)
	}
	paths := proxyPaths(w.Body.String())
	if len(paths) != 4 {
		t.Fatalf("%v", paths)
	}
	for i, pp := range paths {
		if w := get(p, 1, pp); w.Code != 502 {
			t.Fatalf("path %d: %d %s", i, w.Code, w.Body)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d connections reached the home network", hits.Load())
	}
	// the Lampac port on another name is not Lampac
	p2 := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:"+homePort, []string{"cdn.example", "home.example", "lampac.test"})}
	sid2, _ := p2.Open(1, Resolved{Kind: "mp4", URL: "http://home.example:" + homePort + "/x.mp4"}, "")
	if w := get(p2, 1, p2.URL(sid2, "http://home.example:"+homePort+"/x.mp4", "")); w.Code != 502 || hits.Load() != 0 {
		t.Fatalf("lampac's address under another name: %d, %d connections", w.Code, hits.Load())
	}
}

func TestProxyAllowsLampacOrigin(t *testing.T) {
	var admin atomic.Int32
	lamp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/proxy/") && !strings.HasPrefix(r.URL.Path, "/lite/") {
			admin.Add(1)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("lampac-bytes"))
	}))
	defer lamp.Close()
	origin := "http://lampac.test:" + portOf(lamp)
	l, _ := NewLampac(origin + "/?uid=secret")
	// only Lampac is reachable on loopback: nothing else is let through
	p := &Proxy{Key: proxyKey, Lampac: l, HTTP: testNet(t, "lampac.test:"+portOf(lamp), []string{"lampac.test"})}
	sid, _ := p.Open(1, Resolved{Kind: "mp4", URL: origin + "/proxy/v.mp4"}, "")
	if w := get(p, 1, p.URL(sid, origin+"/proxy/v.mp4", "")); w.Code != 200 || w.Body.String() != "lampac-bytes" {
		t.Fatalf("lampac origin: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{"/admin", "/proxy/../admin", "/proxy/%2e%2e/admin", "/api/x"} {
		if w := get(p, 1, p.URL(sid, origin+bad, "")); w.Code != 502 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	if admin.Load() != 0 {
		t.Fatalf("%d requests reached Lampac outside its stream paths", admin.Load())
	}
}

func TestGuardRedirectRules(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loop":
			n++
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/ftp":
			http.Redirect(w, r, "ftp://cdn.example/x", http.StatusFound)
		}
	}))
	defer srv.Close()
	c := testNet(t, "lampac.test:9", []string{"cdn.example"}, srv.Listener.Addr().String())
	base := "http://cdn.example:" + portOf(srv)
	if _, err := c.Get(base + "/loop"); err == nil || n != 6 {
		t.Fatalf("redirect loop: %v after %d requests", err, n)
	}
	if _, err := c.Get(base + "/ftp"); err == nil {
		t.Fatal("ftp redirect followed")
	}
	if !blockedAddr("192.168.1.1") || blockedAddr("93.184.216.34") {
		t.Fatal("blockedAddr")
	}
}

func TestRedirectSendsNoUpstreamReferer(t *testing.T) {
	var got atomic.Value
	got.Store("unset")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Referer"))
		w.Write([]byte("seg"))
	}))
	defer cdn.Close()
	cdnURL := "http://cdn.example:" + portOf(cdn) + "/x.ts"
	lamp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdnURL, http.StatusFound)
	}))
	defer lamp.Close()
	origin := "http://lampac.test:" + portOf(lamp)
	l, _ := NewLampac(origin + "/?uid=secret")
	p := &Proxy{Key: proxyKey, Lampac: l,
		HTTP: testNet(t, "lampac.test:"+portOf(lamp), []string{"lampac.test", "cdn.example"}, cdn.Listener.Addr().String())}
	sid, _ := p.Open(1, Resolved{Kind: "mp4", URL: origin + "/proxy/v.ts"}, "")
	if w := get(p, 1, p.URL(sid, origin+"/proxy/v.ts", "")); w.Code != 200 || got.Load() != "" {
		t.Fatalf("Lampac → CDN redirect: %d, Referer %q", w.Code, got.Load())
	}
	// the source's own Referer is kept on the hop
	sid2, _ := p.Open(2, Resolved{Kind: "mp4", URL: origin + "/proxy/v.ts", Headers: http.Header{"Referer": {"https://player.example/"}}}, "")
	if w := get(p, 2, p.URL(sid2, origin+"/proxy/v.ts", "")); w.Code != 200 || got.Load() != "https://player.example/" {
		t.Fatalf("source Referer: %d, %q", w.Code, got.Load())
	}
}

func TestNewUpstreamClientAsWired(t *testing.T) {
	lamp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("lampac")) }))
	defer lamp.Close()
	other, hits := countingListener(t)
	l, _ := NewLampac(lamp.URL + "/?uid=secret") // http://127.0.0.1:<port>, as main.go gets it
	c := NewUpstreamClient(l)
	defer c.CloseIdleConnections()
	resp, err := c.Get(lamp.URL + "/proxy/x")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("Lampac origin: %v", err)
	}
	resp.Body.Close()
	if _, err := c.Get("http://" + other + "/x"); err == nil || hits.Load() != 0 {
		t.Fatalf("another loopback port: %v, %d connections", err, hits.Load())
	}
}
