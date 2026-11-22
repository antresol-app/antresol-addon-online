package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// offersFake makes the play fake's Lampac serve the film 438631 from three
// sources: plain (one voice-over, no subtitles, 1080p in its name), many (five
// voice-overs, subtitles in four languages) and slow (one voice-over, after
// `slow`). It counts the requests of each level.
type offersFake struct {
	slow atomic.Int64 // ns
	mu   sync.Mutex
	hits map[string]int
}

func (o *offersFake) count(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits[path]
}

const offersFile = `{"method":"play","translate":"%s","url":"http://cdn.example/v720/index.m3u8","quality":{"720p":"http://cdn.example/v720/index.m3u8"},` +
	`"headers":{"Referer":"https://player.example/"}%s}`

func offersLampac(f *playFake) *offersFake {
	o := &offersFake{hits: map[string]int{}}
	h := func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/lite/events" && r.URL.Query().Get("id") == "438631" {
			var evs []string
			for _, s := range []string{"plain - 1080p", "many", "slow - 480p"} {
				id := strings.Fields(s)[0]
				evs = append(evs, fmt.Sprintf(`{"name":"%s","url":"http://lampac.test:9118/lite/%s","show":true,"balanser":"%s"}`, s, id, id))
			}
			w.Write([]byte("[" + strings.Join(evs, ",") + "]"))
			return true
		}
		id, ok := strings.CutPrefix(r.URL.Path, "/lite/")
		if !ok || (id != "plain" && id != "many" && id != "slow") {
			return false
		}
		o.mu.Lock()
		o.hits[r.URL.Path]++
		o.mu.Unlock()
		switch id {
		case "plain":
			w.Write([]byte(`{"type":"movie","data":[` + fmt.Sprintf(offersFile, "Дубляж", "") + `]}`))
		case "many":
			var voices []string
			for i := 1; i <= 5; i++ {
				voices = append(voices, fmt.Sprintf(`{"method":"link","url":"http://lampac.test:9118/lite/many?t=%d","active":%t,"name":"Озвучка %d"}`, i, i == 1, i))
			}
			subs := `,"subtitles":[{"label":"Русские","url":"http://cdn.example/ru.srt"},{"label":"English","url":"http://cdn.example/en.srt"},` +
				`{"label":"German","url":"http://cdn.example/de.srt"},{"label":"French","url":"http://cdn.example/fr.srt"}]`
			w.Write([]byte(`{"type":"movie","voice":[` + strings.Join(voices, ",") + `],"data":[` + fmt.Sprintf(offersFile, "Озвучка 1", subs) + `]}`))
		case "slow":
			time.Sleep(time.Duration(o.slow.Load()))
			w.Write([]byte(`{"type":"movie","data":[` + fmt.Sprintf(offersFile, "Оригинал", "") + `]}`))
		}
		return true
	}
	f.extra.Store(&h)
	return o
}

func newOffersEnv(t *testing.T, opts ...envOpt) (*appEnv, *offersFake) {
	t.Helper()
	f := newPlayFake(t)
	o := offersLampac(f)
	e := &appEnv{f: f}
	e.env = newEnv(t, f.svc, opts...)
	return e, o
}

// A source lists its first three voice-overs and its first three subtitle
// languages, then «+N»; the quality of its name comes first; the labels are
// in the profile's language.
func TestApp_Step_SourceRowsListThree(t *testing.T) {
	e, _ := newOffersEnv(t)
	src := e.step(t, call("movie", 438631, "", ""))
	if labels(src) != "plain|many|slow" {
		t.Fatalf("sources: %+v", src)
	}
	plain, many, slow := src.Rows[0], src.Rows[1], src.Rows[2]
	if plain.Detail != "Дубляж" || strings.Join(plain.Badges, "|") != "1080p" {
		t.Fatalf("no subtitles: %+v", plain)
	}
	if many.Detail != "Озвучка 1, Озвучка 2, Озвучка 3 +2" || strings.Join(many.Badges, "|") != "720p|Субтитры: RU, EN, DE +1" {
		t.Fatalf("many: %+v", many)
	}
	if slow.Detail != "Оригинал" || strings.Join(slow.Badges, "|") != "480p" {
		t.Fatalf("slow: %+v", slow)
	}
	en := &addonkit.User{ID: 1, UID: "uid-admin", Name: "Admin", IsAdmin: true, Lang: "en"}
	w := e.post("/step", call("movie", 438631, "", ""), en)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Subtitles: RU, EN, DE +1"`) {
		t.Fatalf("en: %d %s", w.Code, w.Body)
	}
}

// The step answers within its budget: a source that has not answered yet
// keeps its row as it was (the quality of its name), and what the lookup
// finds after the step has answered is there at the next one, which asks the
// source no more.
func TestApp_Step_SourceBudget(t *testing.T) {
	e, o := newOffersEnv(t, func(d *Deps) { d.OfferWait = 250 * time.Millisecond })
	o.slow.Store(int64(900 * time.Millisecond))
	e.step(t, call("movie", 438631, "", "")) // the title's list is known: what follows is the lookups'
	start := time.Now()
	src := e.step(t, call("movie", 438631, "", ""))
	took := time.Since(start)
	if took < 200*time.Millisecond || took > 700*time.Millisecond {
		t.Fatalf("the step took %v for a 250 ms budget over a 900 ms source", took)
	}
	if r := src.Rows[2]; r.Label != "slow" || r.Detail != "480p" || r.Badges != nil {
		t.Fatalf("the slow source, not ready: %+v", r)
	}
	if r := src.Rows[1]; r.Detail == "" || len(r.Badges) != 2 {
		t.Fatalf("a ready source: %+v", r)
	}
	e.api.Wait() // the lookup that was in flight ends
	start = time.Now()
	src = e.step(t, call("movie", 438631, "", ""))
	if took := time.Since(start); took > 150*time.Millisecond {
		t.Fatalf("the second step took %v: the lookups were cached", took)
	}
	if r := src.Rows[2]; r.Detail != "Оригинал" || strings.Join(r.Badges, "|") != "480p" {
		t.Fatalf("the slow source, ready: %+v", r)
	}
	if n := o.count("/lite/slow"); n != 1 {
		t.Fatalf("the slow source was asked %d times", n)
	}
}

// «Сразу» goes on from the last choices without looking at the other sources:
// a slow one does not hold it, and only the remembered source is asked.
func TestApp_Run_QuickDoesNotLookUpSources(t *testing.T) {
	first, _ := newOffersEnv(t)
	src := first.step(t, call("movie", 438631, "", ""))
	files := first.step(t, call("movie", 438631, src.Rows[0].Ref, ""))
	a, w := first.run(t, call("movie", 438631, files.Rows[0].Ref, ""), admin)
	if w.Code != 200 || a.Play == nil {
		t.Fatalf("play: %d %s", w.Code, w.Body)
	}
	first.putApp(t, admin, a.Play.Session, `"positionMs":30000,"durationMs":100000`)

	// a service that knows nothing of the title yet, over the same progress
	f := newPlayFake(t)
	o := offersLampac(f)
	o.slow.Store(int64(1500 * time.Millisecond))
	e := &appEnv{f: f}
	e.env = newEnv(t, f.svc, func(d *Deps) { d.Progress = first.api.d.Progress })
	start := time.Now()
	b, w := e.run(t, call("movie", 438631, "", `,"quick":true`), admin)
	if w.Code != 200 || b.Play == nil || b.Play.StartSec != 30 {
		t.Fatalf("quick: %d %s", w.Code, w.Body)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the quick run took %v", took)
	}
	if o.count("/lite/plain") == 0 || o.count("/lite/many") != 0 || o.count("/lite/slow") != 0 {
		t.Fatalf("sources asked: plain %d, many %d, slow %d", o.count("/lite/plain"), o.count("/lite/many"), o.count("/lite/slow"))
	}
}
