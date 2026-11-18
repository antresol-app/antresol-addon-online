package api

import (
	"encoding/base64"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// Every URL a client gets (the play answer's, its subtitles', and every
// URI of a rewritten playlist) is under the core's proxy prefix of the
// addon, /api/addons/online/x/media/<sid>/…, so the player fetches it through
// the core; none is under the old in-core path.
func TestPlay_URLsUnderProxyBase(t *testing.T) {
	if StreamBase != "/api/addons/online/x/media/" {
		t.Fatalf("StreamBase %q", StreamBase)
	}
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "438631")
	a, w := e.play(t, admin, playBody(refs[0], `,"quality":"720p"`))
	if w.Code != 200 || a.Session == "" || !strings.HasPrefix(a.URL, StreamBase+a.Session+"/") {
		t.Fatalf("play: %d %s", w.Code, w.Body)
	}
	if _, err := exec.LookPath("ffmpeg"); err == nil && len(a.Subtitles) == 0 {
		t.Fatalf("no subtitles: %s", w.Body)
	}
	for i, s := range a.Subtitles {
		if want := StreamBase + a.Session + "/subs/" + string(rune('0'+i)) + ".vtt"; s.URL != want {
			t.Fatalf("subtitle %d: %q, want %q", i, s.URL, want)
		}
	}
	pl := e.do("GET", a.URL, "", admin)
	if pl.Code != 200 || !strings.Contains(pl.Body.String(), "#EXTINF") {
		t.Fatalf("playlist: %d %s", pl.Code, pl.Body)
	}
	n := 0
	for _, line := range strings.Split(pl.Body.String(), "\n") {
		if strings.Contains(line, "/api/online/") {
			t.Fatalf("old path in %q", line)
		}
		for _, u := range proxyURIs(line) {
			n++
			if !strings.HasPrefix(u, StreamBase+a.Session+"/~") {
				t.Fatalf("URI %q is no token of the session", u)
			}
			if g := e.do("GET", u, "", admin); g.Code != 200 || g.Body.String() != "G-segment" {
				t.Fatalf("%s: %d %s", u, g.Code, g.Body)
			}
		}
	}
	if n != 3 {
		t.Fatalf("%d URIs in\n%s", n, pl.Body)
	}
	for _, s := range strings.Split(w.Body.String(), `"`) {
		if strings.HasPrefix(s, "/api/") && !strings.HasPrefix(s, StreamBase) {
			t.Fatalf("a play answer URL off the base: %q", s)
		}
	}
}

// A stale link mid-play: the source's 410 (or 403/404) reaches the player
// as the proxy's 410 with its body and marker, unchanged by the route, so
// hls.js re-resolves through the core proxy (which passes 410 through).
func TestStream_410PassesThrough(t *testing.T) {
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "438631")
	a, w := e.play(t, admin, playBody(refs[0], `,"quality":"720p"`))
	if w.Code != 200 {
		t.Fatalf("play: %d %s", w.Code, w.Body)
	}
	pl := e.do("GET", a.URL, "", admin)
	var seg string
	for _, line := range strings.Split(pl.Body.String(), "\n") {
		if u := proxyURIs(line); len(u) > 0 && !strings.Contains(line, "#EXT-X-MAP") {
			seg = u[0]
			break
		}
	}
	if seg == "" {
		t.Fatalf("no segment in\n%s", pl.Body)
	}
	f.gone.Store(true)
	g := e.do("GET", seg, "", admin)
	if g.Code != 410 || errOf(g) != "Источник не ответил" || g.Header().Get("X-Vault-Online") != "expired" ||
		g.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("stale segment: %d %v %s", g.Code, g.Header(), g.Body)
	}
	// a closed session is 404, also unchanged
	if d := e.do("DELETE", StreamBase+a.Session, "", admin); d.Code != 204 {
		t.Fatalf("close: %d %s", d.Code, d.Body)
	}
	if g := e.do("GET", seg, "", admin); g.Code != 404 || errOf(g) != "Сеанс не найден" {
		t.Fatalf("closed: %d %s", g.Code, g.Body)
	}
}

func TestResolvePicksQualityAndFallback(t *testing.T) {
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "438631")
	// the request's caps cap «Авто» (the core's per-profile cap is not the addon's to read)
	a, w := e.play(t, admin, playBody(refs[0], `,"caps":{"maxHeight":720}`))
	if w.Code != 200 || a.Mode != "online" || a.Kind != "hls" || a.Quality.Current != "720p" ||
		strings.Join(a.Quality.Options, ",") != "1080p,720p" || a.StartSec != 12 || a.DurationMs == nil || *a.DurationMs != 0 ||
		a.AudioIndex != -1 || a.Audio == nil || len(a.Audio) != 0 || a.Session == "" || !strings.HasPrefix(a.URL, StreamBase+a.Session+"/") {
		t.Fatalf("capped default: %d %s", w.Code, w.Body)
	}
	if p := e.do("GET", a.URL, "", admin); p.Code != 200 || !strings.Contains(p.Body.String(), "#EXTINF") ||
		!strings.Contains(p.Body.String(), StreamBase) || strings.Contains(p.Body.String(), "seg1.ts") {
		t.Fatalf("720 playlist: %d %s", p.Code, p.Body)
	}
	a, w = e.play(t, admin, playBody(refs[0], `,"quality":"1080p","replaceSession":"`+a.Session+`"`))
	if w.Code != 200 || a.Quality.Current != "1080p" {
		t.Fatalf("1080p: %d %s", w.Code, w.Body)
	}
	// the first 1080p URL is dead: the second one plays (the master)
	if p := e.do("GET", a.URL, "", admin); p.Code != 200 || !strings.Contains(p.Body.String(), "#EXT-X-STREAM-INF") {
		t.Fatalf("1080p playlist: %d %s", p.Code, p.Body)
	}
	// the quality asked for is remembered for the title
	if a, w = e.play(t, admin, playBody(refs[0], `,"replaceSession":"`+a.Session+`"`)); w.Code != 200 || a.Quality.Current != "1080p" {
		t.Fatalf("remembered: %d %s", w.Code, w.Body)
	}
	// the user header's remote cap (maxHeight) caps «Авто» too: the lower
	// of it and the request's caps wins, either way round
	away := *admin
	away.MaxHeight = 720
	for name, c := range map[string]struct {
		u    *addonkit.User
		caps string
	}{
		"header only":           {&away, ""},
		"header under the caps": {&away, `,"caps":{"maxHeight":1080}`},
		"caps under the header": {&addonkit.User{ID: 1, UID: "uid-admin", Name: "Админ", IsAdmin: true, Lang: "ru", MaxHeight: 1080}, `,"caps":{"maxHeight":720}`},
	} {
		e2 := newEnv(t, newPlayFake(t).svc)
		r2, _ := fileRefs(t, e2, c.u, "438631")
		if a, w := e2.play(t, c.u, playBody(r2[0], c.caps)); w.Code != 200 || a.Quality.Current != "720p" {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// an explicit choice still applies away from home, as in the core
	if a, w := e.play(t, &away, playBody(refs[0], `,"quality":"1080p","replaceSession":"`+a.Session+`"`)); w.Code != 200 || a.Quality.Current != "1080p" {
		t.Fatalf("explicit away: %d %s", w.Code, w.Body)
	}
	if _, w := e.play(t, admin, playBody(refs[2], "")); w.Code != 415 || errOf(w) != "Этот формат не поддерживается, выберите другое качество" {
		t.Fatalf("mpd: %d %s", w.Code, w.Body)
	}
	if _, w := e.play(t, admin, playBody("forged", "")); w.Code != 400 || errOf(w) != "Неверная ссылка" {
		t.Fatalf("forged: %d %s", w.Code, w.Body)
	}
	if _, w := e.play(t, admin, `{"ref":`); w.Code != 400 {
		t.Fatalf("bad body: %d %s", w.Code, w.Body)
	}
}

func TestReplayBypassesCache(t *testing.T) {
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "438631")
	a1, w := e.play(t, admin, playBody(refs[1], ""))
	if w.Code != 200 || a1.Kind != "mp4" || f.calls.Load() != 1 {
		t.Fatalf("call play: %d %s calls=%d", w.Code, w.Body, f.calls.Load())
	}
	if p := e.do("GET", a1.URL, "", admin, "Range", "bytes=4-7"); p.Code != 206 || p.Body.String() != "ftyp" {
		t.Fatalf("mp4 range: %d %q", p.Code, p.Body)
	}
	// without refresh the call answer is cached
	a2, w := e.play(t, admin, playBody(refs[1], `,"replaceSession":"`+a1.Session+`"`))
	if w.Code != 200 || f.calls.Load() != 1 {
		t.Fatalf("cached: %d calls=%d", w.Code, f.calls.Load())
	}
	// the player's re-play after a stale link: fresh, and it replaces the session
	a3, w := e.play(t, admin, playBody(refs[1], `,"refresh":true,"replaceSession":"`+a2.Session+`"`))
	if w.Code != 200 || f.calls.Load() != 2 || a3.Session == a2.Session {
		t.Fatalf("refresh: %d calls=%d", w.Code, f.calls.Load())
	}
	if p := e.do("GET", a2.URL, "", admin); p.Code != 404 {
		t.Fatalf("replaced session: %d", p.Code)
	}
	if p := e.do("GET", a3.URL, "", admin); p.Code != 200 {
		t.Fatalf("new session: %d", p.Code)
	}
	// the player's close
	if d := e.do("DELETE", StreamBase+a3.Session, "", admin); d.Code != 204 || d.Body.Len() != 0 {
		t.Fatalf("close: %d %s", d.Code, d.Body)
	}
	if p := e.do("GET", a3.URL, "", admin); p.Code != 404 || errOf(p) != "Сеанс не найден" {
		t.Fatalf("closed session: %d %s", p.Code, p.Body)
	}
	if d := e.do("DELETE", StreamBase+a3.Session, "", admin); d.Code != 404 {
		t.Fatalf("close twice: %d", d.Code)
	}
}

func TestOnlineSubtitlesInPlay(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "438631")
	// play does not wait for the conversions: both tracks are offered
	a, w := e.play(t, admin, playBody(refs[0], ""))
	if w.Code != 200 || len(a.Subtitles) != 2 {
		t.Fatalf("subtitles: %d %s", w.Code, w.Body)
	}
	s := a.Subtitles[0]
	// the shape of /api/play's tracks: the players read title, not label
	if s.Index != 0 || s.Label != "Русские" || s.Title != "Русские" || s.Codec != "webvtt" || !s.External || s.Forced || s.BurnIn || s.Lang != "ru" ||
		s.URL != StreamBase+a.Session+"/subs/0.vtt" {
		t.Fatalf("track: %+v", s)
	}
	v := e.do("GET", s.URL, "", admin)
	if v.Code != 200 || !strings.HasPrefix(v.Body.String(), "WEBVTT") || !strings.HasPrefix(v.Header().Get("Content-Type"), "text/vtt") {
		t.Fatalf("vtt: %d %v %s", v.Code, v.Header(), v.Body)
	}
	// the 3 MiB one fails on its fetch, and the next play leaves it out
	if v := e.do("GET", StreamBase+a.Session+"/subs/1.vtt", "", admin); v.Code != 404 {
		t.Fatalf("failing track: %d", v.Code)
	}
	for _, p := range []string{"/subs/2.vtt", "/subs/x.vtt", "/subs/0.srt", "/subs/-1.vtt"} {
		if v := e.do("GET", StreamBase+a.Session+p, "", admin); v.Code != 404 {
			t.Fatalf("%s: %d", p, v.Code)
		}
	}
	a, w = e.play(t, admin, playBody(refs[0], `,"replaceSession":"`+a.Session+`"`))
	if w.Code != 200 || len(a.Subtitles) != 1 || a.Subtitles[0].Label != "Русские" {
		t.Fatalf("second play: %d %s", w.Code, w.Body)
	}
}

func TestRefRatingRecheckedOnPlay(t *testing.T) {
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "1") // 18+
	if _, w := e.play(t, teen, playBody(refs[1], "")); w.Code != 403 || errOf(w) != "Недоступно для этого профиля" {
		t.Fatalf("12+ replaying an adult's ref: %d %s", w.Code, w.Body)
	}
	if f.calls.Load() != 0 {
		t.Fatal("the source was asked before the gate")
	}
	if _, w := e.play(t, kid, playBody(refs[1], "")); w.Code != 403 || errOf(w) != "Недоступно в детском профиле" {
		t.Fatalf("kid: %d %s", w.Code, w.Body)
	}
	a, w := e.play(t, admin, playBody(refs[1], ""))
	if w.Code != 200 {
		t.Fatalf("adult: %d %s", w.Code, w.Body)
	}
	// another user's session
	for _, u := range []*addonkit.User{teen, mama} {
		if p := e.do("GET", a.URL, "", u); p.Code != 404 {
			t.Fatalf("another user's sid: %d", p.Code)
		}
		if d := e.do("DELETE", StreamBase+a.Session, "", u); d.Code != 404 {
			t.Fatalf("another user's close: %d", d.Code)
		}
	}
	if p := e.do("GET", a.URL, "", kid); p.Code != 403 {
		t.Fatalf("kid on the proxy: %d", p.Code)
	}
}

// The core lets a media key (?mk=) through on its media routes and hands
// the query on; the URIs of a playlist fetched with one carry it on, so
// AVPlayer and AirPlay can fetch them without a session.
func TestMediaKeyOpensOnlineProxy(t *testing.T) {
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "438631")
	a, w := e.play(t, admin, playBody(refs[0], `,"quality":"720p"`))
	if w.Code != 200 {
		t.Fatalf("play: %d %s", w.Code, w.Body)
	}
	const key = "v1.1.2.3.abc"
	p := e.do("GET", a.URL+"?mk="+key, "", admin)
	if p.Code != 200 || !strings.Contains(p.Body.String(), "?mk="+key) {
		t.Fatalf("keyed playlist: %d %s", p.Code, p.Body)
	}
	n := 0
	for _, line := range strings.Split(p.Body.String(), "\n") {
		for _, u := range proxyURIs(line) {
			n++
			if !strings.HasSuffix(u, "?mk="+key) {
				t.Fatalf("URI without the key: %s", u)
			}
			if g := e.do("GET", u, "", admin); g.Code != 200 {
				t.Fatalf("keyed %s: %d", u, g.Code)
			}
		}
	}
	if n != 3 {
		t.Fatalf("%d URIs in\n%s", n, p.Body)
	}
	if h := e.do("HEAD", a.URL+"?mk="+key, "", admin); h.Code != 200 || h.Body.Len() != 0 ||
		h.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("keyed HEAD: %d %v", h.Code, h.Header())
	}
	// without a key the URIs carry none
	if p := e.do("GET", a.URL, "", admin); strings.Contains(p.Body.String(), "mk=") {
		t.Fatalf("unkeyed playlist: %s", p.Body)
	}
}

// leaks are what no client output may hold: Lampac, its uid, its proxy
// paths, CDN hosts and upstream header values.
var leaks = []string{"lampac.test", "uid=", "secret", "/proxy/", "cdn.example", "cdn2.example", "player.example"}

func TestNoUpstreamInClientOutput(t *testing.T) {
	_, noFF := exec.LookPath("ffmpeg")
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, browse := fileRefs(t, e, admin, "438631")
	a, w := e.play(t, admin, playBody(refs[0], `,"quality":"1080p"`))
	if w.Code != 200 || (noFF == nil && len(a.Subtitles) != 2) {
		t.Fatalf("play: %d %s", w.Code, w.Body)
	}
	master := e.do("GET", a.URL, "", admin)
	var variant string
	for _, line := range strings.Split(master.Body.String(), "\n") {
		if strings.HasPrefix(line, StreamBase) {
			variant = line
			break
		}
	}
	mediaPL := e.do("GET", variant, "", admin)
	outs := map[string]*httptest.ResponseRecorder{"play": w, "master": master, "media": mediaPL}
	bodies := map[string]string{"browse": browse, "play": w.Body.String(), "master": master.Body.String(), "media": mediaPL.Body.String()}
	if noFF == nil { // the VTT part needs ffmpeg; the rest always runs
		vtt := e.do("GET", a.Subtitles[0].URL, "", admin)
		outs["vtt"], bodies["vtt"] = vtt, vtt.Body.String()
	}
	for name, o := range outs {
		if o.Code != 200 {
			t.Fatalf("%s: %d %s", name, o.Code, o.Body)
		}
	}
	if !strings.Contains(mediaPL.Body.String(), "#EXT-X-MAP:URI=\""+StreamBase) {
		t.Fatalf("media playlist:\n%s", mediaPL.Body)
	}
	// every blob is sealed: its bytes hold no URL either
	blobs := 0
	for name, body := range bodies {
		for _, line := range strings.Split(body, "\n") {
			for _, u := range proxyURIs(strings.ReplaceAll(line, `"url":"`, " ")) {
				p, _, _ := strings.Cut(u, "?")
				rest := strings.TrimPrefix(p, StreamBase)
				_, blob, _ := strings.Cut(rest, "/")
				if strings.HasPrefix(blob, "subs/") {
					continue
				}
				if strings.HasPrefix(blob, "~") { // a token: an index into the session's URLs
					if len(blob) > 8 {
						t.Fatalf("%s: token %q", name, blob)
					}
					blobs++
					continue
				}
				raw, err := base64.RawURLEncoding.DecodeString(blob)
				if err != nil {
					t.Fatalf("%s: blob %q: %v", name, blob, err)
				}
				blobs++
				for _, l := range append(leaks, "http", "/v/", ".m3u8") {
					if strings.Contains(string(raw), l) {
						t.Fatalf("%s: a blob decodes to %q", name, l)
					}
				}
			}
		}
	}
	if blobs < 8 {
		t.Fatalf("%d blobs checked", blobs)
	}
	for name, body := range bodies {
		for _, l := range leaks {
			if strings.Contains(body, l) {
				t.Fatalf("%s holds %q:\n%s", name, l, body)
			}
		}
	}
	for name, o := range outs {
		for k, vs := range o.Header() {
			for _, v := range vs {
				for _, l := range leaks {
					if strings.Contains(v, l) {
						t.Fatalf("%s header %s holds %q", name, k, l)
					}
				}
			}
		}
	}
}
