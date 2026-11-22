package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/api"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	_ "github.com/antresol-app/antresol-addon-online/internal/strings"
	"github.com/antresol-app/antresol/addonkit"
)

// appStep is a step of the app protocol (POST /api/app/step) over the addon's
// routes and this fake as its Lampac.
type appStep struct {
	Title string
	Rows  []struct {
		Label, Detail string
		Badges        []string
	}
}

// appOver is the addon's routes over the fake as its Lampac (the source step
// waits `wait` for the sources' offers; 0: its default).
func appOver(t *testing.T, base string, wait time.Duration) (*http.ServeMux, *api.API) {
	t.Helper()
	l, err := online.NewLampac(base + "/?uid=e2e")
	if err != nil {
		t.Fatal(err)
	}
	svc := &online.Service{L: l, Refs: online.NewRefs([]byte("0123456789abcdef0123456789abcdef"), l.Base),
		Titles: func(_ context.Context, kind string, _ int) (online.TitleCtx, error) {
			return online.TitleCtx{Title: "Фильм", Year: 2024, Serial: kind == "series"}, nil
		}}
	mux := http.NewServeMux()
	a := api.Register(mux, api.Deps{Online: svc, OfferWait: wait})
	t.Cleanup(a.Wait)
	return mux, a
}

func doStep(t *testing.T, h http.Handler, kind string, id int) (appStep, time.Duration) {
	t.Helper()
	u, _ := json.Marshal(addonkit.User{ID: 1, UID: "uid-admin", Name: "Админ", IsAdmin: true, Lang: "ru"})
	body := `{"way":"watch","kind":"` + kind + `","tmdbId":` + itoa(id) + `,"ref":""}`
	r := httptest.NewRequest("POST", "/api/app/step", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(addonkit.UserHeader, string(u))
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, r)
	took := time.Since(start)
	var s appStep
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatalf("step: %d %s", w.Code, w.Body)
	}
	return s, took
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func row(s appStep, i int) string {
	r := s.Rows[i]
	return r.Label + " | " + r.Detail + " | " + strings.Join(r.Badges, " / ")
}

// The source step of the app says, for each of the fake's sources, which
// voice-overs and subtitles it has: a film's three voice-overs and the Russian
// subtitles of the first; a series' one voice-over and its English subtitles
// (from its first season).
func TestAppStep_SourceRowsOverTheFake(t *testing.T) {
	srv, _ := start(t)
	app, _ := appOver(t, srv.URL, 0)
	film, took := doStep(t, app, "movie", 4242)
	if len(film.Rows) != 3 {
		t.Fatalf("%+v", film)
	}
	for i, want := range []string{
		"Alpha | Дубляж, Многоголосый, Оригинал | 1080p / Субтитры: RU",
		"Beta | Дубляж, Многоголосый, Оригинал | 720p / Субтитры: RU",
		"Gamma | Дубляж, Многоголосый, Оригинал | 1080p / Субтитры: RU", // the best its files name
	} {
		if got := row(film, i); got != want {
			t.Errorf("film row %d: %q, want %q", i, got, want)
		}
	}
	t.Logf("film (%v):\n%s\n%s\n%s", took, row(film, 0), row(film, 1), row(film, 2))
	if took > 2*time.Second {
		t.Errorf("the step took %v", took)
	}

	series, took := doStep(t, app, "series", 4343)
	for i, want := range []string{
		"Alpha | Дубляж | 1080p / Субтитры: EN",
		"Beta | Дубляж | 720p / Субтитры: EN",
		"Gamma | Дубляж | 1080p / Субтитры: EN",
	} {
		if got := row(series, i); got != want {
			t.Errorf("series row %d: %q, want %q", i, got, want)
		}
	}
	t.Logf("series (%v):\n%s\n%s\n%s", took, row(series, 0), row(series, 1), row(series, 2))
	if took > 2*time.Second {
		t.Errorf("the step took %v", took)
	}
}

// A source that answers slowly has no badges yet (its row stays as it was),
// the step does not wait for it beyond its budget, and the next step, once it
// answered, has them at once.
func TestAppStep_SlowSourceOverTheFake(t *testing.T) {
	srv, _ := start(t)
	post(t, srv.URL+"/_fake/delay?prefix=/lite/beta&ms=1200")
	app, a := appOver(t, srv.URL, 400*time.Millisecond)
	first, took := doStep(t, app, "movie", 4242)
	t.Logf("first (%v):\n%s\n%s\n%s", took, row(first, 0), row(first, 1), row(first, 2))
	if took > time.Second || row(first, 1) != "Beta | 720p | " {
		t.Errorf("the slow source's row: %q after %v", row(first, 1), took)
	}
	if !strings.Contains(row(first, 0), "Дубляж") || !strings.Contains(row(first, 2), "Дубляж") {
		t.Errorf("the others are ready: %q %q", row(first, 0), row(first, 2))
	}
	a.Wait() // the slow lookup ends in the background
	second, took := doStep(t, app, "movie", 4242)
	t.Logf("second (%v):\n%s", took, row(second, 1))
	if row(second, 1) != "Beta | Дубляж, Многоголосый, Оригинал | 720p / Субтитры: RU" || took > 300*time.Millisecond {
		t.Errorf("the slow source is cached now: %q after %v", row(second, 1), took)
	}
}
