package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/online"
)

// start serves the fake over a folder with a made-up playlist and segment
// (no ffmpeg in the unit test).
func start(t *testing.T) (*httptest.Server, *online.Lampac) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nseg0.ts\n#EXT-X-ENDLIST\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "seg0.ts"), []byte("ts"), 0o644)
	if _, err := ensureHLS(dir, 6, "no-ffmpeg-needed"); err != nil {
		t.Fatalf("an existing video is kept: %v", err)
	}
	srv := httptest.NewServer(newFake(dir))
	t.Cleanup(srv.Close)
	l, err := online.NewLampac(srv.URL + "/?uid=e2e")
	if err != nil {
		t.Fatal(err)
	}
	return srv, l
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST %s: %d", url, resp.StatusCode)
	}
}

func TestEvents_TheAdapterReadsThem(t *testing.T) {
	srv, l := start(t)
	evs, err := l.Events(context.Background(), online.Q{TMDBID: 4242, Title: "Фильм", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].Balanser != "alpha" || !evs[0].Show || evs[0].Rch || evs[0].URL != srv.URL+"/lite/alpha" {
		t.Fatalf("events %+v", evs)
	}
	imdb, kp, err := l.ExternalIDs(context.Background(), 4242, false, "")
	if err != nil || imdb != "tt0004242" || kp != "4242" {
		t.Fatalf("externalids %q %q %v", imdb, kp, err)
	}
}

func TestLevels(t *testing.T) {
	srv, l := start(t)
	ctx := context.Background()
	var film struct {
		Type  string `json:"type"`
		Voice []struct {
			URL    string `json:"url"`
			Active bool   `json:"active"`
			Name   string `json:"name"`
		} `json:"voice"`
		Data []struct {
			Method    string            `json:"method"`
			URL       string            `json:"url"`
			Quality   map[string]string `json:"quality"`
			Translate string            `json:"translate"`
			Subtitles []struct{ Label string }
		} `json:"data"`
	}
	b, err := l.Get(ctx, "/lite/beta?id=4242&serial=0&t=2")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &film); err != nil {
		t.Fatal(err)
	}
	if film.Type != "movie" || len(film.Voice) != 3 || !film.Voice[1].Active || film.Voice[1].Name != "Многоголосый" ||
		len(film.Data) != 1 || film.Data[0].Translate != "Многоголосый" || len(film.Data[0].Subtitles) != 0 ||
		len(film.Data[0].Quality) != 3 || !strings.HasPrefix(film.Data[0].URL, srv.URL+"/proxy/hls/beta/") {
		t.Fatalf("film level %s", b)
	}
	if b, _ := l.Get(ctx, "/lite/beta?id=4242&serial=0"); !strings.Contains(string(b), "/proxy/subs/ru.srt") {
		t.Fatalf("the dub has Russian subtitles: %s", b)
	}
	b, err = l.Get(ctx, "/lite/alpha?id=4343&serial=1")
	if err != nil || !strings.Contains(string(b), `"type":"season"`) || !strings.Contains(string(b), "/lite/alpha?s=1") {
		t.Fatalf("seasons %s %v", b, err)
	}
	b, err = l.Get(ctx, "/lite/alpha?id=4343&serial=1&s=1")
	if err != nil || !strings.Contains(string(b), `"type":"episode"`) || strings.Count(string(b), `"method":"play"`) != episodes {
		t.Fatalf("episodes %s %v", b, err)
	}
	if _, err := l.Get(ctx, "/lite/nope?id=1"); !errors.Is(err, online.ErrNothing) {
		t.Fatalf("an unknown source is {}: %v", err)
	}
}

func TestStream_ExpireDelayDownAndLog(t *testing.T) {
	srv, l := start(t)
	if code, body := get(t, srv.URL+"/proxy/hls/alpha/m1-t1/720p/index.m3u8"); code != 200 || !strings.HasPrefix(body, "#EXTM3U") {
		t.Fatalf("playlist %d %q", code, body)
	}
	post(t, srv.URL+"/_fake/expire?segment=seg0.ts")
	if code, _ := get(t, srv.URL+"/proxy/hls/alpha/m1-t1/720p/seg0.ts"); code != http.StatusGone {
		t.Fatalf("expired segment: %d", code)
	}
	if code, body := get(t, srv.URL+"/proxy/hls/alpha/m1-t1/720p/seg0.ts"); code != 200 || body != "ts" {
		t.Fatalf("once only: %d %q", code, body)
	}
	if code, _ := get(t, srv.URL+"/proxy/hls/alpha/m1-t1/720p/..%2fsecret"); code != http.StatusNotFound {
		t.Fatalf("outside the folder: %d", code)
	}
	// a stale link: from seg0 on, until a source's level is asked again
	post(t, srv.URL+"/_fake/stale?from=0")
	if _, err := l.Get(context.Background(), "/lite/alpha?id=1"); err != nil { // before any stale answer: still stale
		t.Fatal(err)
	}
	if code, _ := get(t, srv.URL+"/proxy/hls/alpha/m1-t1/720p/seg0.ts"); code != http.StatusGone {
		t.Fatalf("stale segment: %d", code)
	}
	if code, _ := get(t, srv.URL+"/proxy/hls/alpha/m1-t1/720p/index.m3u8"); code != 200 {
		t.Fatalf("the playlist stays: %d", code)
	}
	if _, err := l.Get(context.Background(), "/lite/alpha?id=1"); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, srv.URL+"/proxy/hls/alpha/m1-t1/720p/seg0.ts"); code != 200 {
		t.Fatalf("fresh after a resolve: %d", code)
	}
	post(t, srv.URL+"/_fake/down")
	if _, err := l.Events(context.Background(), online.Q{TMDBID: 1}); !errors.Is(err, online.ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
	post(t, srv.URL+"/_fake/up")
	_, body := get(t, srv.URL+"/_fake/requests")
	var recs []record
	if err := json.Unmarshal([]byte(body), &recs); err != nil || len(recs) < 10 || recs[1].Status != http.StatusGone || recs[len(recs)-1].Status != 0 {
		// (Go's client asks again once over a new connection: 5 or 6)
		t.Fatalf("requests %s", body)
	}
	post(t, srv.URL+"/_fake/reset")
	if _, body := get(t, srv.URL+"/_fake/requests"); strings.TrimSpace(body) != "null" {
		t.Fatalf("reset: %s", body)
	}
}
