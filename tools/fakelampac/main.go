// fakelampac plays a Lampac sidecar for the addon's end-to-end tests (and
// as the stream source of a performance run): its /externalids, its
// /lite/events and the levels of three synthetic sources (alpha, beta,
// gamma), and a synthetic HLS video under /proxy/, the only Lampac paths a
// stream may be on (netguard.go lampacStreamPath).
//
//	go run ./tools/fakelampac [-listen 127.0.0.1:0] [-hls DIR] [-seconds 6] [-ffmpeg ffmpeg]
//
// The video is ffmpeg's testsrc2 with a tone, H.264/AAC in 2 s MPEG-TS
// segments (seg0.ts …), made into DIR on the first start (kept for the next
// ones; -seconds sets its length, 3 segments by default). Every stream URL
// of every source, voice, episode and quality plays it. Once listening, the
// program prints one line, «fakelampac http://HOST:PORT», on stdout.
//
// The sources: a film has the voices «Дубляж» (with Russian subtitles),
// «Многоголосый» and «Оригинал», each in 1080p, 720p and 480p; a series
// has one season of four episodes, voiced «Дубляж», with English
// subtitles (the online tracking scan browses a source's latest season:
// the e2e's TMDB series has season 1 only). Every link carries the
// request's own origin.
//
// Faults are injected at run time, under /_fake/ (no Lampac path):
//
//	POST /_fake/expire?segment=seg1.ts[&count=1]  the next count requests of that segment answer 410
//	POST /_fake/stale?from=2                       the link went stale: every segment from seg2.ts on answers 410
//	                                               until a source level is asked after one did (/lite/<source>:
//	                                               the player's fresh resolve)
//	POST /_fake/delay?prefix=/lite/events&ms=1200 requests whose path starts with prefix wait ms first (ms=0: none)
//	POST /_fake/down  /  POST /_fake/up            every Lampac request has its connection closed / answers again
//	GET  /_fake/requests                           the requests seen, in order: {method, path, query, status}
//	POST /_fake/reset                              no faults, no requests
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	hls := flag.String("hls", filepath.Join(os.TempDir(), "fakelampac-hls"), "the synthetic HLS video's folder (made with ffmpeg when it has no index.m3u8)")
	seconds := flag.Int("seconds", 6, "the video's length when it is made (2 s segments)")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "ffmpeg, to make the video")
	flag.Parse()
	dir, err := ensureHLS(*hls, *seconds, *ffmpeg)
	if err != nil {
		log.Fatalf("fakelampac: %v", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("fakelampac: %v", err)
	}
	fmt.Printf("fakelampac http://%s\n", ln.Addr())
	srv := &http.Server{Handler: newFake(dir), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

// ensureHLS makes the video in dir unless it is there (index.m3u8); a
// half-made one is never seen: it is made beside dir and renamed.
func ensureHLS(dir string, seconds int, ffmpeg string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "index.m3u8")); err == nil {
		return dir, nil
	}
	if seconds < 2 {
		return "", errors.New("-seconds: at least 2")
	}
	tmp := fmt.Sprintf("%s.%d.tmp", dir, os.Getpid())
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	d := strconv.Itoa(seconds)
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration="+d,
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration="+d,
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "48",
		"-c:a", "aac", "-b:a", "96k", "-shortest",
		"-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod",
		"-hls_segment_filename", filepath.Join(tmp, "seg%d.ts"), filepath.Join(tmp, "index.m3u8"))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp) // another process made it first
		if _, serr := os.Stat(filepath.Join(dir, "index.m3u8")); serr != nil {
			return "", err
		}
	}
	return dir, nil
}

// record is one request the fake saw.
type record struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query"`
	Status int    `json:"status"`
}

type fake struct {
	hls string

	mu       sync.Mutex
	expire   map[string]int // segment name → how many more 410s
	staleAt  int            // segments from this index on answer 410 (-1: none)
	staleHit bool           // one of them did: the next level asked is the fresh resolve
	delays   map[string]time.Duration
	down     bool
	requests []record
}

func newFake(hls string) *fake {
	return &fake{hls: hls, expire: map[string]int{}, delays: map[string]time.Duration{}, staleAt: -1}
}

// statusWriter keeps the status for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") {
		f.control(w, r)
		return
	}
	f.mu.Lock()
	down := f.down
	var wait time.Duration
	for p, d := range f.delays {
		if strings.HasPrefix(r.URL.Path, p) && d > wait {
			wait = d
		}
	}
	f.mu.Unlock()
	if down {
		f.log(r, 0)
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
	}
	sw := &statusWriter{ResponseWriter: w}
	f.serve(sw, r)
	f.log(r, sw.status)
}

func (f *fake) log(r *http.Request, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) < 10000 {
		f.requests = append(f.requests, record{r.Method, r.URL.Path, r.URL.RawQuery, status})
	}
}

func (f *fake) control(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/_fake/requests":
		writeJSON(w, f.requests)
		return
	case r.Method != http.MethodPost:
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	case r.URL.Path == "/_fake/expire":
		n, err := strconv.Atoi(q.Get("count"))
		if err != nil || n < 1 {
			n = 1
		}
		if q.Get("segment") == "" {
			http.Error(w, "segment", http.StatusBadRequest)
			return
		}
		f.expire[q.Get("segment")] = n
	case r.URL.Path == "/_fake/stale":
		n, err := strconv.Atoi(q.Get("from"))
		if err != nil || n < 0 {
			http.Error(w, "from", http.StatusBadRequest)
			return
		}
		f.staleAt, f.staleHit = n, false
	case r.URL.Path == "/_fake/delay":
		ms, err := strconv.Atoi(q.Get("ms"))
		if err != nil || ms < 0 {
			http.Error(w, "ms", http.StatusBadRequest)
			return
		}
		f.delays[q.Get("prefix")] = time.Duration(ms) * time.Millisecond
	case r.URL.Path == "/_fake/down":
		f.down = true
	case r.URL.Path == "/_fake/up":
		f.down = false
	case r.URL.Path == "/_fake/reset":
		f.expire, f.delays, f.down, f.requests, f.staleAt = map[string]int{}, map[string]time.Duration{}, false, nil, -1
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// The synthetic sources (no real source's name), in Lampac's order.
var sources = []struct{ id, name string }{
	{"alpha", "Alpha - 1080p"},
	{"beta", "Beta - 720p"},
	{"gamma", "Gamma"},
}

// The words a real source would answer in Russian (its voices, its
// season names, its subtitles), as data (testdata/words.json): the
// addon's untranslated-literal check reads its Go code, and these are a
// source's, not the addon's.
//
//go:embed testdata/words.json
var wordsJSON []byte

var words struct {
	FilmVoices   []string `json:"filmVoices"`
	SeriesVoices []string `json:"seriesVoices"`
	Season       string   `json:"season"`
	Subtitles    map[string]struct {
		Label string `json:"label"`
		Cue   string `json:"cue"`
	} `json:"subtitles"`
}

func init() {
	if err := json.Unmarshal(wordsJSON, &words); err != nil {
		panic(err)
	}
	filmVoices, seriesVoices = words.FilmVoices, words.SeriesVoices
}

var (
	filmVoices, seriesVoices []string
	qualities                = []string{"1080p", "720p", "480p"}
)

const (
	seasons  = 1
	episodes = 4
)

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET", http.StatusMethodNotAllowed)
		return
	}
	origin := "http://" + r.Host
	q := r.URL.Query()
	p := r.URL.Path
	switch {
	case p == "/externalids":
		id, _ := strconv.Atoi(q.Get("id"))
		writeJSON(w, map[string]string{"imdb_id": fmt.Sprintf("tt%07d", id), "kinopoisk_id": strconv.Itoa(id)})
	case p == "/lite/events":
		var out []map[string]any
		for i, s := range sources {
			out = append(out, map[string]any{"name": s.name, "url": origin + "/lite/" + s.id, "index": (i + 1) * 10,
				"show": true, "balanser": s.id, "rch": false})
		}
		writeJSON(w, out)
	case strings.HasPrefix(p, "/lite/"):
		b := strings.TrimPrefix(p, "/lite/")
		if !known(b) {
			writeJSON(w, map[string]any{})
			return
		}
		// a source's level asked again after a stale answer: its links are fresh
		f.mu.Lock()
		if f.staleHit {
			f.staleAt, f.staleHit = -1, false
		}
		f.mu.Unlock()
		if q.Get("serial") == "1" {
			f.seriesLevel(w, origin, b, q)
			return
		}
		f.filmLevel(w, origin, b, q)
	case strings.HasPrefix(p, "/proxy/subs/"):
		sub, ok := words.Subtitles[strings.TrimSuffix(path.Base(p), ".srt")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
		fmt.Fprintf(w, "1\n00:00:00,500 --> 00:00:30,000\n%s\n", sub.Cue)
	case strings.HasPrefix(p, "/proxy/hls/"):
		f.stream(w, r, path.Base(p))
	default:
		http.NotFound(w, r)
	}
}

func known(b string) bool {
	for _, s := range sources {
		if s.id == b {
			return true
		}
	}
	return false
}

// pick is the 1-based index t of the query in n voices (1 by default).
func pick(q url.Values, n int) int {
	t, err := strconv.Atoi(q.Get("t"))
	if err != nil || t < 1 || t > n {
		return 1
	}
	return t
}

// streamURLs are a file's qualities: every one plays the one video.
func streamURLs(origin, b, file string) map[string]string {
	out := map[string]string{}
	for _, ql := range qualities {
		out[ql] = fmt.Sprintf("%s/proxy/hls/%s/%s/%s/index.m3u8", origin, b, file, ql)
	}
	return out
}

func voiceLinks(origin, b string, voices []string, active int, extra string) []map[string]any {
	var out []map[string]any
	for i, v := range voices {
		out = append(out, map[string]any{"method": "link", "url": fmt.Sprintf("%s/lite/%s?t=%d%s", origin, b, i+1, extra),
			"active": i+1 == active, "name": v})
	}
	return out
}

func (f *fake) filmLevel(w http.ResponseWriter, origin, b string, q url.Values) {
	t := pick(q, len(filmVoices))
	voice := filmVoices[t-1]
	file := fmt.Sprintf("m%s-t%d", q.Get("id"), t)
	quality := streamURLs(origin, b, file)
	item := map[string]any{"method": "play", "url": quality[qualities[0]], "quality": quality,
		"translate": voice, "maxquality": qualities[0], "title": voice}
	if t == 1 {
		item["subtitles"] = []map[string]string{{"label": words.Subtitles["ru"].Label, "url": origin + "/proxy/subs/ru.srt"}}
	}
	writeJSON(w, map[string]any{"type": "movie", "voice": voiceLinks(origin, b, filmVoices, t, ""), "data": []any{item}})
}

func (f *fake) seriesLevel(w http.ResponseWriter, origin, b string, q url.Values) {
	s, err := strconv.Atoi(q.Get("s"))
	if err != nil || s < 1 || s > seasons {
		var data []map[string]any
		for n := 1; n <= seasons; n++ {
			data = append(data, map[string]any{"method": "link", "id": n, "url": fmt.Sprintf("%s/lite/%s?s=%d", origin, b, n),
				"name": fmt.Sprintf(words.Season, n)})
		}
		writeJSON(w, map[string]any{"type": "season", "data": data})
		return
	}
	t := pick(q, len(seriesVoices))
	var data []map[string]any
	for e := 1; e <= episodes; e++ {
		file := fmt.Sprintf("tv%s-t%d-s%de%d", q.Get("id"), t, s, e)
		quality := streamURLs(origin, b, file)
		data = append(data, map[string]any{"method": "play", "url": quality[qualities[0]], "quality": quality,
			"s": s, "e": e, "name": fmt.Sprintf("Серия %d", e), "maxquality": qualities[0],
			"subtitles": []map[string]string{{"label": words.Subtitles["en"].Label, "url": origin + "/proxy/subs/en.srt"}}})
	}
	writeJSON(w, map[string]any{"type": "episode", "voice": voiceLinks(origin, b, seriesVoices, t, fmt.Sprintf("&s=%d", s)), "data": data})
}

// stream serves the video's playlist or a segment by its name, or the
// injected 410 of a stale link.
func (f *fake) stream(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	n := f.expire[name]
	if n > 0 {
		f.expire[name] = n - 1
	}
	stale := false
	if i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".ts")); err == nil && f.staleAt >= 0 && i >= f.staleAt {
		stale, f.staleHit = true, true
	}
	f.mu.Unlock()
	if n > 0 || stale {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	if name == "." || name == "/" || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	file := filepath.Join(f.hls, name)
	if _, err := os.Stat(file); err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	http.ServeFile(w, r, file)
}
