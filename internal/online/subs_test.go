package online

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubtitlesToVTT(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Referer") != "https://player.example/" {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/ru.srt":
			w.Write(fixture(t, "subs.srt"))
		case "/en.vtt":
			w.Write([]byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello there, synthetic.\n"))
		case "/big.srt":
			w.Write([]byte(strings.Repeat("1\n00:00:01,000 --> 00:00:02,000\nx\n\n", 3<<20/40)))
		}
	}))
	defer srv.Close()
	base := "http://cdn.example:" + portOf(srv)
	s := &Subs{Dir: t.TempDir(), FFmpeg: ff, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, srv.Listener.Addr().String())}
	h := http.Header{"Referer": {"https://player.example/"}}
	ctx := context.Background()

	path, err := s.VTT(ctx, Sub{Label: "Rus", URL: base + "/ru.srt"}, h)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "WEBVTT") || !strings.Contains(string(b), "Привет, это синтетические субтитры.") {
		t.Fatalf("vtt:\n%s", b)
	}
	if filepath.Dir(path) != s.Dir || !strings.HasSuffix(path, ".vtt") {
		t.Fatalf("path %s", path)
	}
	if l := SubLang("", path); l != "ru" {
		t.Fatalf("sniffed lang %q", l)
	}
	if l := SubLang("English", path); l != "en" {
		t.Fatalf("label lang %q", l)
	}
	// the second call is the cache
	n := hits.Load()
	if p2, err := s.VTT(ctx, Sub{Label: "Rus", URL: base + "/ru.srt"}, h); err != nil || p2 != path || hits.Load() != n {
		t.Fatalf("cache: %v %s %d", err, p2, hits.Load()-n)
	}
	// WebVTT as is
	p3, err := s.VTT(ctx, Sub{Label: "eng", URL: base + "/en.vtt"}, h)
	if b, _ := os.ReadFile(p3); err != nil || !strings.HasPrefix(string(b), "WEBVTT\n\n00:00:01.000") {
		t.Fatalf("vtt as is: %v %s", err, b)
	}
	// over 2 MiB: refused, nothing cached
	if _, err := s.VTT(ctx, Sub{URL: base + "/big.srt"}, h); err == nil {
		t.Fatal("3 MiB subtitle accepted")
	}
	ents, _ := os.ReadDir(s.Dir)
	if len(ents) != 2 {
		t.Fatalf("cache dir: %v", ents)
	}
}

func TestSubsCacheCap(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for i, name := range []string{"a.vtt", "b.vtt", "c.vtt"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, make([]byte, 100), 0o644)
		os.Chtimes(p, old.Add(time.Duration(i)*time.Minute), old.Add(time.Duration(i)*time.Minute))
	}
	(&Subs{Dir: dir, MaxCache: 250}).prune()
	if _, err := os.Stat(filepath.Join(dir, "a.vtt")); !os.IsNotExist(err) {
		t.Fatal("oldest kept")
	}
	for _, n := range []string{"b.vtt", "c.vtt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if defaultSubsCache != 200<<20 {
		t.Fatal("cache cap")
	}
}

// A play answer does not wait for its subtitles: a slow track is listed
// at once and converted on its first fetch (sharing the warm-up).
func TestPlaySubtitlesDoNotWait(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		w.Write([]byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nПривет.\n"))
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	up := testNet(t, "lampac.test:9", []string{"cdn.example"}, srv.Listener.Addr().String())
	s := &Service{L: &Lampac{}, Subs: &Subs{Dir: t.TempDir(), HTTP: up}, Proxy: &Proxy{Key: proxyKey, HTTP: up}}
	t.Cleanup(s.Subs.Wait)
	s.init()
	r := Resolved{Kind: "mp4", URL: "http://cdn.example:" + portOf(srv) + "/v.mp4",
		Subtitles: []Sub{{Label: "Русские", URL: "http://cdn.example:" + portOf(srv) + "/ru.vtt"}}}
	start := time.Now()
	subs, warm := s.PlaySubtitles(context.Background(), r)
	if d := time.Since(start); d > time.Second || len(subs) != 1 || subs[0].File != "" || subs[0].Lang != "ru" {
		t.Fatalf("%+v in %v", subs, d)
	}
	r.Subtitles = subs
	sid, err := s.Proxy.Open(1, r, "")
	if err != nil {
		t.Fatal(err)
	}
	warm()
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		req := httptest.NewRequest("GET", "/x", nil).WithContext(WithUser(context.Background(), User{ID: 1}))
		w := httptest.NewRecorder()
		s.Proxy.ServeSub(w, req, sid, 0)
		done <- w
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	w := <-done
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Привет.") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1 (the warm-up shared)", n)
	}
	// converted now: the next play answer has its file
	if subs, _ := s.PlaySubtitles(context.Background(), r); len(subs) != 1 || subs[0].File == "" {
		t.Fatalf("%+v", subs)
	}
}

// A track that is not subtitles, or that its server refuses, is left out
// for 10 minutes; one that failed in a way that may pass (busy, a server
// error, a timeout, the network) for a minute.
func TestSubFailFor(t *testing.T) {
	for err, want := range map[error]time.Duration{
		fmt.Errorf("%w: unknown format", errSubtitle): subFailTTL,
		fmt.Errorf("%w: HTTP 404", errSubtitle):       subFailTTL,
		fmt.Errorf("%w: HTTP 502", errSubtitleServer): subRetryTTL,
		fmt.Errorf("%w: deadline", errSubtitleBusy):   subRetryTTL,
		fmt.Errorf("x: %w", context.DeadlineExceeded): subRetryTTL,
		fmt.Errorf("%w: reset", ErrUnavailable):       subRetryTTL,
	} {
		if got := subFailFor(err); got != want {
			t.Errorf("%v: %v, want %v", err, got, want)
		}
	}
}
