package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/hooks"
	"github.com/antresol-app/antresol/addonkit"
)

func serve(t *testing.T, ready *addonkit.Ready, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if ready == nil {
		ready = &addonkit.Ready{}
	}
	rec := httptest.NewRecorder()
	h, _ := newHandler(addonkit.Env{}, nil, ready)
	h.ServeHTTP(rec, r)
	return rec
}

func TestHealth(t *testing.T) {
	ready := &addonkit.Ready{}
	for _, want := range []bool{false, true} {
		rec := serve(t, ready, httptest.NewRequest("GET", "/health", nil))
		var got map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got["ready"] != want {
			t.Fatalf("health: %d %s, want ready %v", rec.Code, rec.Body, want)
		}
		ready.Set()
	}
}

// Until the services are built the hooks answer 404 (the core fails the
// operation closed).
func TestHooksNotFoundBeforeRegistered(t *testing.T) {
	rec := serve(t, nil, httptest.NewRequest("POST", "/hooks/library.afterScan", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("hook: %d %s, want 404", rec.Code, rec.Body)
	}
}

// Until the services are built the app's routes answer 503 «Расширение
// запускается» with Retry-After, in the profile's language; then they are
// the routes set.
func TestAPIStartingUntilBooted(t *testing.T) {
	api := &lateHandler{}
	r := httptest.NewRequest("GET", "/api/available", nil)
	r.Header.Set(addonkit.UserHeader, `{"id":1,"uid":"u","lang":"en"}`)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "The addon is starting") {
		t.Fatalf("before: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	api.set(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, r)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("after: %d", rec.Code)
	}
}

// A boot that failed leaves the app's routes 503 for good (until the next
// start): they say the addon is not ready, not that it is starting, and
// ask for no retry.
func TestAPINotReadyAfterFailedBoot(t *testing.T) {
	api := &lateHandler{}
	api.fail()
	r := httptest.NewRequest("GET", "/api/available", nil)
	r.Header.Set(addonkit.UserHeader, `{"id":1,"uid":"u","lang":"en"}`)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" ||
		!strings.Contains(rec.Body.String(), "The addon is not ready") || strings.Contains(rec.Body.String(), "starting") {
		t.Fatalf("en: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	r.Header.Set(addonkit.UserHeader, `{"id":1,"uid":"u","lang":"ru"}`)
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), "Расширение не готово") {
		t.Fatalf("ru: %s", rec.Body)
	}
}

// TestManifestHooksAreServed: the manifest declares exactly the hooks
// internal/hooks answers.
func TestManifestHooksAreServed(t *testing.T) {
	raw, err := os.ReadFile("antresol-addon.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Hooks []string `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Sorted(slices.Values(m.Hooks)), slices.Sorted(slices.Values(hooks.Names))) {
		t.Fatalf("manifest %v, served %v", m.Hooks, hooks.Names)
	}
}
