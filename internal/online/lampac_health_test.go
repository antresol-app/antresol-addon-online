package online

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// Every Lampac call keeps its outcome for «Состояние»: an answer is up
// (a source's failure behind it too), the account refused is auth, an
// unreachable Lampac is down; Timeout bounds a call.
func TestLampacHealthAndTimeout(t *testing.T) {
	l := fakeLampac(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lite/accsdb":
			w.Write(fixture(t, "err-accsdb.json"))
		case "/lite/401":
			w.WriteHeader(401)
		case "/lite/503":
			w.WriteHeader(503)
		case "/lite/slow":
			time.Sleep(300 * time.Millisecond)
			w.Write([]byte(`[]`))
		default:
			w.Write(fixture(t, "movie-alpha.json"))
		}
	}))
	h := &addonkit.HelperHealth{}
	l.Health = h
	ctx := context.Background()
	last := func() (bool, string) { ok, p, _, live, _ := h.Last(); _ = live; return ok, p }

	if _, err := l.Get(ctx, "/lite/alpha"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := last(); !ok {
		t.Fatal("an answer is up")
	}
	if _, err := l.Get(ctx, "/lite/accsdb"); !errors.Is(err, ErrAuth) || !errors.Is(err, ErrSourceFailed) {
		t.Fatalf("accsdb: %v", err)
	}
	if ok, p := last(); ok || p != addonkit.HelperProblemAuth {
		t.Fatalf("accsdb: %v %q", ok, p)
	}
	if _, err := l.Get(ctx, "/lite/503"); !errors.Is(err, ErrSourceFailed) {
		t.Fatal(err)
	}
	if ok, _ := last(); !ok {
		t.Fatal("a source's 503 is Lampac answering")
	}
	if _, err := l.Get(ctx, "/lite/401"); !errors.Is(err, ErrAuth) {
		t.Fatalf("401: %v", err)
	}
	if ok, p := last(); ok || p != addonkit.HelperProblemAuth {
		t.Fatalf("401: %v %q", ok, p)
	}
	l.Timeout = 50 * time.Millisecond
	if _, err := l.Get(ctx, "/lite/slow"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("slow: %v", err)
	}
	if ok, p := last(); ok || p != addonkit.HelperProblemTimeout {
		t.Fatalf("slow: %v %q", ok, p)
	}
	// a request the caller gave up says nothing of Lampac
	l.Timeout = 0
	h.Record(nil)
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	l.Get(cctx, "/lite/slow")
	if ok, _ := last(); !ok {
		t.Fatal("a cancelled call was recorded")
	}
	// down
	d, _ := NewLampac("http://127.0.0.1:1/?uid=secret")
	d.Health = h
	d.Get(ctx, "/lite/x")
	if ok, p := last(); ok || p != addonkit.HelperProblemRefused {
		t.Fatalf("down: %v %q", ok, p)
	}
}

// The admin's choice: the sources off are left out, the chosen order
// comes first, the rest keep Lampac's order.
func TestSourcePrefsArrange(t *testing.T) {
	in := []Source{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	got := SourcePrefs{Order: []string{"c", "zz", "a"}, Off: []string{"b"}}.arrange(in)
	ids := ""
	for _, s := range got {
		ids += s.ID
	}
	if ids != "cad" || in[0].ID != "a" {
		t.Fatalf("%q (in %v)", ids, in)
	}
	if got := (SourcePrefs{}).arrange(in); len(got) != 4 {
		t.Fatal(got)
	}
}

// The admin's choice applies to every list, cached or not, and to a
// remembered source; LampacSources lists every non-rch source.
func TestSourcesWithPrefs(t *testing.T) {
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/externalids":
			w.Write([]byte(`{"imdb_id":"tt1160419"}`))
		case "/lite/events":
			w.Write([]byte(`[{"name":"Alpha - 1080p","url":"http://lampac.test:9118/lite/alpha","balanser":"alpha","show":true},
				{"name":"Beta","url":"http://lampac.test:9118/lite/beta","balanser":"beta","show":true},
				{"name":"Gamma","url":"http://lampac.test:9118/lite/gamma","balanser":"gamma","show":false},
				{"name":"Rch","url":"http://lampac.test:9118/lite/rch","balanser":"rch","show":true,"rch":true}]`))
		}
	}), dune)
	var p SourcePrefs
	s.Prefs = func() SourcePrefs { return p }
	ids := func() string {
		got, err := s.Sources(context.Background(), adult(), "movie", 438631)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, x := range got {
			out += x.ID + " "
		}
		return out
	}
	if got := ids(); got != "alpha beta " {
		t.Fatalf("%q", got)
	}
	p = SourcePrefs{Order: []string{"beta"}, Off: []string{"alpha"}}
	if got := ids(); got != "beta " { // from the cache
		t.Fatalf("%q", got)
	}
	if _, err := s.Source(context.Background(), adult(), "movie", 438631, "alpha"); !errors.Is(err, ErrNothing) {
		t.Fatalf("an off source: %v", err)
	}
	all, err := s.LampacSources(context.Background())
	if err != nil || len(all) != 3 || all[0].ID != "alpha" || all[0].Name != "Alpha" || !all[0].Found || all[2].Found {
		t.Fatalf("%+v %v", all, err)
	}
}
