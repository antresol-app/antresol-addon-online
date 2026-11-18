package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// The profile's rating limit comes from the core's header alone:
// "maxRating" set gates by it, absent or null is no limit.
func TestGate_MaxRatingFromHeader(t *testing.T) {
	e := newEnv(t, fakeOnline(t, false))
	limited := `{"id":2,"uid":"uid-teen","name":"Петя","isAdmin":false,"isKid":false,"lang":"ru","maxRating":"12+"}`
	for id, want := range map[string]int{"1": 403, "2": 403, "3": 200} { // 18+, unrated, 12+
		w := e.doRaw("/api/addons/online/x/sources?kind=movie&tmdbId="+id, limited)
		if w.Code != want {
			t.Fatalf("12+ %s: %d %s", id, w.Code, w.Body)
		}
		if want == 403 && errOf(w) != "Недоступно для этого профиля" {
			t.Fatalf("12+ %s: %s", id, w.Body)
		}
	}
	for _, h := range []string{
		`{"id":2,"uid":"uid-teen","name":"Петя","isAdmin":false,"isKid":false,"lang":"ru"}`,
		`{"id":2,"uid":"uid-teen","name":"Петя","isAdmin":false,"isKid":false,"lang":"ru","maxRating":null}`,
	} {
		for _, id := range []string{"1", "2"} {
			if w := e.doRaw("/api/addons/online/x/sources?kind=movie&tmdbId="+id, h); w.Code != 200 {
				t.Fatalf("no limit %s (%s): %d %s", id, h, w.Code, w.Body)
			}
		}
	}
	// the same limit holds on a typed URL of another route: the progress
	if w := e.doRaw("/api/addons/online/x/progress?kind=movie&tmdbId=1", limited); w.Code != 403 || errOf(w) != "Недоступно для этого профиля" {
		t.Fatalf("progress over the limit: %d %s", w.Code, w.Body)
	}
	// the limit is the profile's language too: the refusal in English
	en := `{"id":2,"uid":"uid-teen","name":"Pete","lang":"en","maxRating":"12+"}`
	if w := e.doRaw("/api/addons/online/x/sources?kind=movie&tmdbId=1", en); w.Code != 403 || errOf(w) != "Not available for this profile" {
		t.Fatalf("en: %d %s", w.Code, w.Body)
	}
}

// A kid header is refused on every route, before anything else is read
// (the core never proxies kids; this is defence in depth), and so is a
// request with no profile or a broken header.
func TestRoutes_KidRefused(t *testing.T) {
	f := newPlayFake(t)
	e := newEnv(t, f.svc)
	refs, _ := fileRefs(t, e, admin, "438631")
	a, w := e.play(t, admin, playBody(refs[0], `,"quality":"720p"`))
	if w.Code != 200 {
		t.Fatalf("play: %d %s", w.Code, w.Body)
	}
	f.svc.Subs.Wait() // the play's subtitle warm-up reads the CDN in the background
	cdn := f.cdn.Load()
	routes := [][3]string{
		{"GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", ""},
		{"GET", "/api/addons/online/x/source?kind=movie&tmdbId=438631&source=alpha", ""},
		{"GET", "/api/addons/online/x/browse?ref=" + queryEscape(refs[0]), ""},
		{"POST", "/api/addons/online/x/play", playBody(refs[0], "")},
		{"GET", a.URL, ""},
		{"HEAD", a.URL, ""},
		{"GET", "/api/addons/online/x/media/" + a.Session + "/subs/0.vtt", ""},
		{"DELETE", "/api/addons/online/x/media/" + a.Session, ""},
		{"GET", "/api/addons/online/x/progress?kind=movie&tmdbId=3", ""},
		{"PUT", "/api/addons/online/x/progress", `{"kind":"movie","tmdbId":3,"positionMs":1,"durationMs":10}`},
		{"GET", "/api/addons/online/x/available", ""},
		{"GET", "/api/addons/online/x/admin/status", ""},
		{"GET", "/api/addons/online/x/admin/settings", ""},
		{"PUT", "/api/addons/online/x/admin/settings", `{"enabled":false}`},
	}
	kidAdmin := *kid
	kidAdmin.IsAdmin = true // never so in the core; refused all the same
	for _, rt := range routes {
		for name, w := range map[string]func() int{
			"kid":       func() int { return e.do(rt[0], rt[1], rt[2], kid).Code },
			"kid admin": func() int { return e.do(rt[0], rt[1], rt[2], &kidAdmin).Code },
			"no header": func() int { return e.do(rt[0], rt[1], rt[2], nil).Code },
		} {
			if code := w(); code != 403 {
				t.Fatalf("%s %s %s: %d", name, rt[0], rt[1], code)
			}
		}
		if rt[0] != "HEAD" {
			if w := e.do(rt[0], rt[1], rt[2], kid); errOf(w) != "Недоступно в детском профиле" {
				t.Fatalf("kid %s %s: %s", rt[0], rt[1], w.Body)
			}
		}
		if rt[0] == "GET" {
			// broken, and no profile at all: null, {}, no id, no uid
			for _, h := range []string{`{"id":4,"isKid":`, `null`, `{}`, `{"id":0,"uid":"uid-x"}`, `{"id":-1,"uid":"uid-x"}`,
				`{"id":5,"uid":""}`, `{"id":5,"isAdmin":true}`} {
				if w := e.doRaw(rt[1], h); w.Code != 403 {
					t.Fatalf("header %s %s: %d", h, rt[1], w.Code)
				}
			}
		}
	}
	if f.cdn.Load() != cdn {
		t.Fatal("a kid's request reached the source")
	}
	// the adult's session is untouched by the kid's close
	if w := e.do("GET", a.URL, "", admin); w.Code != 200 {
		t.Fatalf("adult after the kid: %d %s", w.Code, w.Body)
	}
}

// x/available is false without a usable Lampac URL, and while the admin
// setting is off.
func TestAvailable_OffWithoutLampac(t *testing.T) {
	avail := func(e *env) bool {
		t.Helper()
		w := e.do("GET", "/api/addons/online/x/available", "", mama)
		var out struct {
			Available *bool `json:"available"`
		}
		if json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || out.Available == nil {
			t.Fatalf("available: %d %s", w.Code, w.Body)
		}
		return *out.Available
	}
	if avail(newEnv(t, nil)) {
		t.Fatal("no Lampac: available")
	}
	e := newEnv(t, fakeOnline(t, false))
	if !avail(e) {
		t.Fatal("configured and on: not available")
	}
	e.enabled.Store(false)
	if avail(e) {
		t.Fatal("off: available")
	}
	// a rating-limited profile: available all the same (the rating is per title)
	e.enabled.Store(true)
	if w := e.do("GET", "/api/addons/online/x/available", "", teen); w.Code != 200 || !strings.Contains(w.Body.String(), `"available":true`) {
		t.Fatalf("12+: %d %s", w.Code, w.Body)
	}
}

func TestOnlineSourcesAndBrowse(t *testing.T) {
	svc := fakeOnline(t, false)
	e := newEnv(t, svc)
	w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", "", admin)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out struct {
		Sources []struct{ ID, Name, Quality, Ref string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Sources) != 1 || out.Sources[0].Name != "alpha" || out.Sources[0].Quality != "1080p" {
		t.Fatalf("%s", w.Body)
	}
	b := e.do("GET", "/api/addons/online/x/browse?ref="+queryEscape(out.Sources[0].Ref), "", admin)
	if b.Code != 200 || !strings.Contains(b.Body.String(), `"level":"movie"`) || !strings.Contains(b.Body.String(), `"qualities":["1080p"]`) {
		t.Fatalf("browse %d %s", b.Code, b.Body)
	}
	for _, leak := range []string{"secret", "127.0.0.1", "cdn.example", "uid="} {
		if strings.Contains(w.Body.String()+b.Body.String(), leak) {
			t.Fatalf("client output contains %q", leak)
		}
	}
	for _, q := range []string{"kind=game&tmdbId=1", "kind=movie&tmdbId=0", "kind=movie&tmdbId=x", "kind=movie&tmdbId=999"} {
		if w := e.do("GET", "/api/addons/online/x/sources?"+q, "", admin); w.Code != 400 || errOf(w) != "Неизвестный фильм или сериал" {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/api/addons/online/x/browse?ref=forged", "", admin); w.Code != 400 || errOf(w) != "Неверная ссылка" {
		t.Fatalf("forged: %d %s", w.Code, w.Body)
	}
	rc := online.RefContext{Kind: "movie", TMDBID: 438631, Source: "alpha"}
	for p, want := range map[string]struct {
		code int
		msg  string
	}{"/lite/fail": {502, "Источник не ответил"}, "/lite/empty": {404, "Ничего не нашлось в этом источнике"}} {
		w := e.do("GET", "/api/addons/online/x/browse?ref="+queryEscape(svc.Refs.Mint(p, rc)), "", admin)
		if w.Code != want.code || errOf(w) != want.msg {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", "", nil); w.Code != 403 {
		t.Fatalf("no profile: %d", w.Code)
	}
}

func TestOnlineSourceWithoutEvents(t *testing.T) {
	// /lite/events of the title hangs: a remembered source that the last
	// events (of another title) listed as shown and non-rch still answers
	// at once; nothing else does
	svc := fakeOnline(t, true)
	svc.SourcesBudget = 300 * time.Millisecond
	e := newEnv(t, svc)
	if w := e.do("GET", "/api/addons/online/x/source?kind=movie&tmdbId=438631&source=alpha", "", admin); w.Code == 200 {
		t.Fatalf("nothing seen yet: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=2", "", admin); w.Code != 200 {
		t.Fatalf("sources: %d %s", w.Code, w.Body)
	}
	for _, id := range []string{"gamma", "events", "beta"} { // rch, an endpoint, never listed
		if w := e.do("GET", "/api/addons/online/x/source?kind=movie&tmdbId=438631&source="+id, "", admin); w.Code == 200 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body)
		}
	}
	w := e.do("GET", "/api/addons/online/x/source?kind=movie&tmdbId=438631&source=alpha", "", admin)
	var out struct{ Source struct{ ID, Ref string } }
	if json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || out.Source.ID != "alpha" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if b := e.do("GET", "/api/addons/online/x/browse?ref="+queryEscape(out.Source.Ref), "", admin); b.Code != 200 || !strings.Contains(b.Body.String(), `"level":"movie"`) {
		t.Fatalf("browse %d %s", b.Code, b.Body)
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("leak: %s", w.Body)
	}
	for q, code := range map[string]int{"kind=game&tmdbId=1&source=alpha": 400, "kind=movie&tmdbId=438631&source=..%2Fx": 404,
		"kind=movie&tmdbId=1&source=alpha": 200} {
		if w := e.do("GET", "/api/addons/online/x/source?"+q, "", admin); w.Code != code {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/api/addons/online/x/source?kind=movie&tmdbId=1&source=alpha", "", teen); w.Code != 403 || errOf(w) != "Недоступно для этого профиля" {
		t.Fatalf("teen: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/source?kind=movie&tmdbId=438631&source=alpha", "", kid); w.Code != 403 {
		t.Fatalf("kid: %d %s", w.Code, w.Body)
	}
}

func TestOnlineKidForbidden(t *testing.T) {
	e := newEnv(t, fakeOnline(t, false))
	w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", "", admin)
	var out struct{ Sources []struct{ Ref string } }
	if json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || len(out.Sources) == 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// a valid ref minted for an adult is refused too
	for _, p := range []string{"/api/addons/online/x/sources?kind=movie&tmdbId=438631", "/api/addons/online/x/browse?ref=" + queryEscape(out.Sources[0].Ref),
		"/api/addons/online/x/admin/status"} {
		if w := e.do("GET", p, "", kid); w.Code != 403 || errOf(w) != "Недоступно в детском профиле" {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	// playback too (screen time needs no rule here: a kid never streams online)
	if w := e.do("POST", "/api/addons/online/x/play", `{"ref":"x"}`, kid); w.Code != 403 {
		t.Fatalf("kid play: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/media/abc/def", "", kid); w.Code != 403 {
		t.Fatalf("kid stream: %d %s", w.Code, w.Body)
	}
}

func TestOnlineRatingGate(t *testing.T) {
	e := newEnv(t, fakeOnline(t, false))
	for id, want := range map[string]int{"1": 403, "2": 403, "3": 200} {
		w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId="+id, "", teen)
		if w.Code != want {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body)
		}
		if want == 403 && errOf(w) != "Недоступно для этого профиля" {
			t.Fatalf("%s: %s", id, w.Body)
		}
	}
	// browse re-checks the ref's title: an adult's ref for an 18+ title
	w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=1", "", admin)
	var out struct{ Sources []struct{ Ref string } }
	if json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || len(out.Sources) == 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/browse?ref="+queryEscape(out.Sources[0].Ref), "", teen); w.Code != 403 || errOf(w) != "Недоступно для этого профиля" {
		t.Fatalf("browse: %d %s", w.Code, w.Body)
	}
	// the rating is per title: the way stays offered
	if w := e.do("GET", "/api/addons/online/x/available", "", teen); !strings.Contains(w.Body.String(), `"available":true`) {
		t.Fatalf("12+ profile: %s", w.Body)
	}
}

func TestOnlineSourcesUnavailable(t *testing.T) {
	svc := fakeOnline(t, true)
	svc.SourcesBudget = 200 * time.Millisecond
	e := newEnv(t, svc)
	w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", "", admin)
	if w.Code != 503 || errOf(w) != "Онлайн-источники сейчас недоступны" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestOnlineOffWithoutConfig(t *testing.T) {
	e := newEnv(t, nil)
	for _, p := range []string{"/api/addons/online/x/sources?kind=movie&tmdbId=438631", "/api/addons/online/x/progress?kind=movie&tmdbId=3",
		"/api/addons/online/x/media/abc/def"} {
		if w := e.do("GET", p, "", admin); w.Code != 404 {
			t.Fatalf("no Lampac %s: %d", p, w.Code)
		}
	}
	if w := e.do("POST", "/api/addons/online/x/play", `{"ref":"x"}`, admin); w.Code != 404 {
		t.Fatalf("no Lampac play: %d", w.Code)
	}
	if w := e.do("GET", "/api/addons/online/x/admin/status", "", admin); w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("no Lampac status: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/admin/status", "", mama); w.Code != 403 {
		t.Fatalf("no Lampac status, user: %d", w.Code)
	}

	e = newEnv(t, fakeOnline(t, false))
	e.enabled.Store(false)
	for _, p := range []string{"/api/addons/online/x/sources?kind=movie&tmdbId=438631", "/api/addons/online/x/browse?ref=x"} {
		if w := e.do("GET", p, "", admin); w.Code != 404 {
			t.Fatalf("%s off: %d", p, w.Code)
		}
	}
	// the admin status answers whatever the setting: off is not «not configured»
	if w := e.do("GET", "/api/addons/online/x/admin/status", "", admin); w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":true`) {
		t.Fatalf("status off: %d %s", w.Code, w.Body)
	}
}

func TestOnlineStatusAdminOnly(t *testing.T) {
	e := newEnv(t, fakeOnline(t, false))
	w := e.do("GET", "/api/addons/online/x/admin/status", "", admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":true`) || !strings.Contains(w.Body.String(), `"reachable":true`) ||
		!strings.Contains(w.Body.String(), `"sources":1`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/admin/status", "", mama); w.Code != 403 {
		t.Fatalf("user: %d", w.Code)
	}
}

// TestOnlineWaitsForTheHelper: on the helper's compose address the routes
// stay 404 and the status says «not configured» with the block to add
// while the helper does not answer; once it answers, all is on.
func TestOnlineWaitsForTheHelper(t *testing.T) {
	found := false
	h := addonkit.Helper{Name: "lampac", Image: "img:1", Port: 9118, Health: "/"}
	e := newEnv(t, fakeOnline(t, false), func(d *Deps) {
		d.Usable = func() bool { return found }
		d.Helper = func() addonkit.HelperStatus {
			if found {
				return addonkit.HelperStatus{Name: "lampac", Source: addonkit.HelperCompose, Address: h.Address()}
			}
			return addonkit.HelperStatus{Name: "lampac", Source: addonkit.HelperNone, Address: h.Address(), Compose: h.Compose()}
		}
	})
	if w := e.do("GET", "/api/addons/online/x/sources?kind=movie&tmdbId=438631", "", admin); w.Code != 404 {
		t.Fatalf("sources without the helper: %d", w.Code)
	}
	if w := e.do("GET", "/api/addons/online/x/available", "", admin); !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("available: %s", w.Body)
	}
	var st struct {
		Configured bool
		Helper     addonkit.HelperStatus
	}
	w := e.do("GET", "/api/addons/online/x/admin/status", "", admin)
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.Configured || st.Helper.Source != addonkit.HelperNone ||
		!strings.Contains(st.Helper.Compose, "image: img:1") {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	found = true
	if w := e.do("GET", "/api/addons/online/x/available", "", admin); !strings.Contains(w.Body.String(), `"available":true`) {
		t.Fatalf("available once found: %s", w.Body)
	}
	w = e.do("GET", "/api/addons/online/x/admin/status", "", admin)
	st.Helper = addonkit.HelperStatus{}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || !st.Configured || st.Helper.Source != addonkit.HelperCompose || st.Helper.Compose != "" {
		t.Fatalf("status once found: %d %s", w.Code, w.Body)
	}
}
