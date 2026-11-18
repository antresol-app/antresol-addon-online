package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol-addon-online/internal/store"
	_ "github.com/antresol-app/antresol-addon-online/internal/strings"
	"github.com/antresol-app/antresol/addonkit"
)

// The profiles of the tests, as the core's X-Antresol-User carries them.
var (
	admin = &addonkit.User{ID: 1, UID: "uid-admin", Name: "Админ", IsAdmin: true, Lang: "ru"}
	teen  = &addonkit.User{ID: 2, UID: "uid-teen", Name: "Петя", Lang: "ru", MaxRating: "12+"}
	mama  = &addonkit.User{ID: 3, UID: "uid-mama", Name: "Мама", Lang: "ru"}
	kid   = &addonkit.User{ID: 4, UID: "uid-kid", Name: "Малыш", IsKid: true, Lang: "ru", MaxRating: "6+"}
)

// onlineTitles are the fake TMDB ratings of the tests, by tmdb id.
var onlineTitles = map[int]string{438631: "12+", 1: "18+", 2: "", 3: "12+"}

func fakeTitles(_ context.Context, kind string, id int) (online.TitleCtx, error) {
	r, ok := onlineTitles[id]
	if !ok {
		return online.TitleCtx{}, online.ErrUnknownTitle
	}
	return online.TitleCtx{Title: "Альфа", Rating: r, Year: 2021, Serial: kind == "series",
		Poster: "https://image.example/t/p/w342/" + kind + ".jpg"}, nil
}

// fakeOnline is an online.Service over a synthetic Lampac; hang makes
// /lite/events of title 438631 never answer.
func fakeOnline(t *testing.T, hang bool) *online.Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uid") != "secret" {
			w.Write([]byte(`{"accsdb":true}`))
			return
		}
		switch r.URL.Path {
		case "/externalids":
			w.Write([]byte(`{"imdb_id":"tt1","kinopoisk_id":"123"}`))
		case "/lite/events":
			if hang && r.URL.Query().Get("id") == "438631" {
				<-r.Context().Done()
				return
			}
			w.Write([]byte(`[{"name":"alpha - 1080p","url":"http://` + r.Host + `/lite/alpha","show":true,"balanser":"alpha"},` +
				`{"name":"gamma","url":"http://` + r.Host + `/lite/gamma","show":true,"rch":true,"balanser":"gamma"}]`))
		case "/lite/alpha":
			w.Write([]byte(`{"type":"movie","data":[{"method":"play","url":"https://cdn.example/a.m3u8","quality":{"1080p":"https://cdn.example/a.m3u8"},"translate":"Дубляж"}]}`))
		case "/lite/fail":
			w.WriteHeader(503)
		case "/lite/empty":
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	l, err := online.NewLampac(srv.URL + "/?uid=secret")
	if err != nil {
		t.Fatal(err)
	}
	return &online.Service{L: l, Refs: online.NewRefs([]byte("0123456789abcdef0123456789abcdef"), l.Base), Titles: fakeTitles}
}

// env is the addon's routes over one service, as the core's proxy reaches
// them: the path after /api/addons/online/x/ under /api/, the profile in
// X-Antresol-User.
type env struct {
	h       http.Handler
	api     *API
	enabled atomic.Bool
	db      *store.DB
}

type envOpt func(*Deps)

func newEnv(t *testing.T, svc *online.Service, opts ...envOpt) *env {
	t.Helper()
	e := &env{}
	e.enabled.Store(true)
	d, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	e.db = d
	if svc != nil {
		svc.Enabled = e.enabled.Load
	}
	deps := Deps{
		Online:   svc,
		Progress: &online.ProgressStore{DB: d.DB},
		Enabled:  e.enabled.Load,
		SetEnabled: func(_ context.Context, on bool) error {
			e.enabled.Store(on)
			return nil
		},
	}
	for _, o := range opts {
		o(&deps)
	}
	mux := http.NewServeMux()
	e.api = Register(mux, deps)
	t.Cleanup(e.api.Wait)
	e.h = mux
	return e
}

// addonPath is the path the addon gets for a client URL under the core's
// proxy (/api/addons/online/x/… → /api/…); other paths are kept.
func addonPath(u string) string {
	if rest, ok := strings.CutPrefix(u, "/api/addons/online/x/"); ok {
		return "/api/" + rest
	}
	return u
}

// do sends method path (a client URL under the proxy, or the addon's
// /api/… path) as u (nil: no header) with hdr as header pairs.
func (e *env) do(method, path, body string, u *addonkit.User, hdr ...string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, addonPath(path), strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, addonPath(path), nil)
	}
	if u != nil {
		b, _ := json.Marshal(u)
		r.Header.Set(addonkit.UserHeader, string(b))
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// doRaw sends GET path with the X-Antresol-User header exactly as given.
func (e *env) doRaw(path, header string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", addonPath(path), nil)
	r.Header.Set(addonkit.UserHeader, header)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

var queryEscape = url.QueryEscape

func errOf(w *httptest.ResponseRecorder) string {
	var m struct{ Error string }
	json.Unmarshal(w.Body.Bytes(), &m)
	return m.Error
}

// playFake is a synthetic Lampac (lampac.test:9118) and CDN (cdn.example),
// reached through a client that maps those names to test servers.
type playFake struct {
	svc   *online.Service
	calls atomic.Int32 // hits of the call endpoint
	cdn   atomic.Int32 // CDN requests
	// gone makes the CDN answer 410 for every segment (a stale link)
	gone atomic.Bool
}

func newPlayFake(t *testing.T) *playFake {
	t.Helper()
	f := &playFake{}
	lamp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uid") != "secret" {
			w.Write([]byte(`{"accsdb":true}`))
			return
		}
		switch r.URL.Path {
		case "/externalids":
			w.Write([]byte(`{"imdb_id":"tt1","kinopoisk_id":"123"}`))
		case "/lite/events":
			w.Write([]byte(`[{"name":"alpha","url":"http://lampac.test:9118/lite/alpha","show":true,"balanser":"alpha"}]`))
		case "/lite/alpha":
			w.Write([]byte(`{"type":"movie","data":[` +
				`{"method":"play","translate":"Дубляж","url":"http://cdn.example/dead/master.m3u8 or http://cdn.example/v/master.m3u8",` +
				`"quality":{"720p":"http://cdn.example/v720/index.m3u8","1080p":"http://cdn.example/dead/master.m3u8 or http://cdn.example/v/master.m3u8"},` +
				`"headers":{"Referer":"https://player.example/","X-Forwarded-For":"10.0.0.1"},` +
				`"subtitles":[{"label":"Русские","url":"http://cdn.example/ru.srt"},{"label":"Big","url":"http://cdn.example/big.srt"}]},` +
				`{"method":"call","translate":"Многоголосый","url":"http://lampac.test:9118/lite/alpha/video?id=1&uid=secret"},` +
				`{"method":"play","translate":"DASH","url":"http://cdn.example/v.mpd","quality":{"1080p":"http://cdn.example/v.mpd"}}]}`))
		case "/lite/alpha/video":
			f.calls.Add(1)
			w.Write([]byte(`{"method":"play","url":"http://cdn.example/c/video.mp4","headers":{"Referer":"https://player.example/"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(lamp.Close)
	srt, err := os.ReadFile(filepath.Join("..", "online", "testdata", "subs.srt"))
	if err != nil {
		t.Fatal(err)
	}
	master, _ := os.ReadFile(filepath.Join("..", "online", "testdata", "master.m3u8"))
	mediaPL, _ := os.ReadFile(filepath.Join("..", "online", "testdata", "media.m3u8"))
	keyMap, _ := os.ReadFile(filepath.Join("..", "online", "testdata", "key-map.m3u8"))
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.cdn.Add(1)
		if r.Header.Get("Referer") != "https://player.example/" || r.Header.Get("User-Agent") != online.UserAgent || r.Header.Get("X-Forwarded-For") != "" {
			w.WriteHeader(403)
			return
		}
		// a ranged request for a playlist gets an empty 200 (a real source's shape)
		if strings.HasSuffix(r.URL.Path, ".m3u8") && r.Header.Get("Range") != "" {
			return
		}
		if f.gone.Load() && !strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.WriteHeader(http.StatusGone)
			return
		}
		switch r.URL.Path {
		case "/v/master.m3u8":
			w.Write(master)
		case "/v/1080/index.m3u8":
			w.Write(mediaPL)
		case "/v720/index.m3u8":
			w.Write(keyMap)
		case "/v720/seg1.ts", "/v720/seg2.ts", "/v720/init.mp4":
			w.Write([]byte("G-segment"))
		case "/c/video.mp4":
			http.ServeContent(w, r, "video.mp4", time.Time{}, strings.NewReader("\x00\x00\x00\x18ftypmp42"+strings.Repeat("m", 1000)))
		case "/ru.srt":
			w.Write(srt)
		case "/big.srt":
			w.Write([]byte(strings.Repeat("1\n00:00:01,000 --> 00:00:02,000\nx\n\n", 3<<20/40)))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(cdn.Close)
	hosts := map[string]string{"lampac.test:9118": lamp.Listener.Addr().String(), "cdn.example:80": cdn.Listener.Addr().String()}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		a, ok := hosts[addr]
		if !ok {
			return nil, errors.New("no route to " + addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, a)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}
	l, err := online.NewLampac("http://lampac.test:9118/?uid=secret")
	if err != nil {
		t.Fatal(err)
	}
	l.HTTP = client
	ff, _ := exec.LookPath("ffmpeg")
	f.svc = &online.Service{L: l, Refs: online.NewRefs([]byte("0123456789abcdef0123456789abcdef"), l.Base), Upstream: client,
		Proxy:  &online.Proxy{Key: []byte("proxy-key-0123456789abcdef012345"), HTTP: client, Lampac: l, Base: StreamBase},
		Subs:   &online.Subs{Dir: t.TempDir(), FFmpeg: ff, HTTP: client, Lampac: l},
		Titles: fakeTitles}
	t.Cleanup(f.svc.Subs.Wait) // before its Dir goes
	return f
}

// fileRefs are the file refs of tmdb id's alpha source, as u browses them.
func fileRefs(t *testing.T, e *env, u *addonkit.User, id string) ([]string, string) {
	t.Helper()
	w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId="+id, "", u)
	var src struct{ Sources []struct{ Ref string } }
	if json.Unmarshal(w.Body.Bytes(), &src); w.Code != 200 || len(src.Sources) != 1 {
		t.Fatalf("sources %d %s", w.Code, w.Body)
	}
	b := e.do("GET", "/api/addons/online/x/browse?ref="+queryEscape(src.Sources[0].Ref), "", u)
	var lv struct{ Files []struct{ Ref string } }
	if json.Unmarshal(b.Body.Bytes(), &lv); b.Code != 200 || len(lv.Files) != 3 {
		t.Fatalf("browse %d %s", b.Code, b.Body)
	}
	refs := []string{}
	for _, f := range lv.Files {
		refs = append(refs, f.Ref)
	}
	return refs, w.Body.String() + b.Body.String()
}

type playAnswer struct {
	Mode, Kind, URL, Session string
	StartSec                 float64
	DurationMs               *int64
	Audio                    []any
	AudioIndex               int
	Subtitles                []struct {
		Index                          int
		Label, Lang, URL, Title, Codec string
		External, Forced, BurnIn       bool
	}
	Quality struct {
		Options []string
		Current string
	}
}

func (e *env) play(t *testing.T, u *addonkit.User, body string, hdr ...string) (playAnswer, *httptest.ResponseRecorder) {
	t.Helper()
	w := e.do("POST", "/api/addons/online/x/play", body, u, hdr...)
	var a playAnswer
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
	}
	return a, w
}

func playBody(ref string, extra string) string {
	b, _ := json.Marshal(ref)
	return `{"ref":` + string(b) + `,"startSec":12` + extra + `}`
}

// proxyURIs are the proxy URIs of a playlist line (a URI line or URI="…").
func proxyURIs(line string) []string {
	var out []string
	for {
		i := strings.Index(line, StreamBase)
		if i < 0 {
			return out
		}
		end := strings.IndexAny(line[i:], "\" \r")
		if end < 0 {
			end = len(line) - i
		}
		out = append(out, line[i:i+end])
		line = line[i+end:]
	}
}
