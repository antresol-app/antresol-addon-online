package online

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLampacSendsAuthQueryAndUA(t *testing.T) {
	var calls atomic.Int32
	l := fakeLampac(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if q.Get("uid") != "secret" || q.Get("rjson") != "true" || len(q["rjson"]) != 1 || len(q["uid"]) != 1 {
			t.Errorf("query %q", r.URL.RawQuery)
		}
		if r.UserAgent() != UserAgent {
			t.Errorf("UA %q", r.UserAgent())
		}
		if r.Host != "lampac.test:9118" {
			t.Errorf("host %q", r.Host)
		}
		switch r.URL.Path {
		case "/lite/redirect":
			http.Redirect(w, r, "http://elsewhere.example/lite/x", http.StatusFound)
		case "/lite/self":
			// Lampac's own redirect: same origin, a new query
			http.Redirect(w, r, "/lite/alpha?uri=x&s=1&uid=secret", http.StatusFound)
		case "/lite/admin-redirect":
			http.Redirect(w, r, "/admin", http.StatusFound)
		case "/lite/big":
			w.Write([]byte(`{"type":"movie","data":["`))
			w.Write(bytes.Repeat([]byte("a"), 5<<20))
			w.Write([]byte(`"]}`))
		case "/externalids":
			w.Write(fixture(t, "externalids.json"))
		default:
			w.Write(fixture(t, "movie-alpha.json"))
		}
	}))
	ctx := context.Background()
	// a link from an answer: wrong-case rjson and a foreign uid are replaced
	if _, err := l.Get(ctx, "/lite/alpha?rjson=False&uid=other&s=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Get(ctx, "http://lampac.test:9118/lite/alpha?id=1"); err != nil {
		t.Fatal(err)
	}
	imdb, kp, err := l.ExternalIDs(ctx, 438631, false, "")
	if err != nil || imdb != "tt1160419" || kp != "123" {
		t.Fatalf("ids %q %q %v", imdb, kp, err)
	}
	if _, err := l.Get(ctx, "/lite/redirect"); !errors.Is(err, ErrSourceFailed) {
		t.Fatalf("redirect: %v", err)
	}
	if _, err := l.Get(ctx, "/lite/big"); !errors.Is(err, ErrSourceFailed) {
		t.Fatalf("big: %v", err)
	}
	if _, err := l.Get(ctx, "/lite/admin-redirect"); !errors.Is(err, ErrSourceFailed) {
		t.Fatalf("redirect off /lite/: %v", err)
	}
	if b, err := l.Get(ctx, "/lite/self"); err != nil || !bytes.Contains(b, []byte(`"type":"movie"`)) {
		t.Fatalf("own redirect: %v", err)
	}
	if calls.Load() != 8 {
		t.Fatalf("calls %d", calls.Load())
	}
	// never another origin, never outside /lite/ and /externalids
	for _, p := range []string{"http://evil.example/lite/x", "/admin", "/proxy/abc", "/lite/../admin", "https://lampac.test:9118/lite/x"} {
		if _, err := l.Get(ctx, p); err == nil {
			t.Errorf("%s: no error", p)
		}
	}
	if calls.Load() != 8 {
		t.Fatalf("a refused path reached Lampac: %d", calls.Load())
	}
}

func TestLampacErrorBodies(t *testing.T) {
	l := fakeLampac(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lite/accsdb":
			w.Write(fixture(t, "err-accsdb.json"))
		case "/lite/rch":
			w.Write(fixture(t, "rch.json"))
		case "/lite/html":
			w.Write([]byte(`<div class="videos__line"></div>`))
		case "/lite/empty":
			w.Write([]byte(` {} `))
		case "/lite/503":
			w.WriteHeader(503)
			w.Write([]byte("search"))
		}
	}))
	for p, want := range map[string]error{"/lite/accsdb": ErrSourceFailed, "/lite/rch": ErrSourceFailed, "/lite/html": ErrSourceFailed,
		"/lite/empty": ErrNothing, "/lite/503": ErrSourceFailed} {
		if _, err := l.Get(context.Background(), p); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", p, err, want)
		}
	}
}

func TestLampacUnavailable(t *testing.T) {
	l, err := NewLampac("http://127.0.0.1:1/?uid=secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Get(context.Background(), "/lite/events?id=1")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("uid in error: %v", err)
	}
}

func TestRedactLampac(t *testing.T) {
	if got := redactLampac("http://lampac.test:9118/?uid=secret"); got != "http://lampac.test:9118" {
		t.Fatalf("%q", got)
	}
	if got := redactLampac("::bad"); strings.Contains(got, "bad") {
		t.Fatalf("%q", got)
	}
	// a failing call logs no uid
	var buf bytes.Buffer
	l, err := NewLampac("http://127.0.0.1:1/?uid=secret")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{L: l, Refs: NewRefs([]byte("k"), l.Base), Log: slog.New(slog.NewTextHandler(&buf, nil)),
		Titles: func(context.Context, string, int) (TitleCtx, error) {
			return TitleCtx{Title: "Альфа", Year: 2021, Rating: "16+"}, nil
		}}
	if _, err := s.Sources(context.Background(), adult(), "movie", 438631); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if buf.Len() == 0 || strings.Contains(buf.String(), "secret") {
		t.Fatalf("log: %q", buf.String())
	}
}

func TestConfigRejectsBadLampacURL(t *testing.T) {
	for _, s := range []string{"ftp://x", "http://", "nonsense", ""} {
		if _, err := NewLampac(s); err == nil {
			t.Errorf("%q: no error", s)
		}
	}
}

// Transport errors keep only a category: net/http puts upstream text in
// some (a malformed Location carrying the uid Lampac echoes into links).
func TestNoUIDInTransportErrors(t *testing.T) {
	l := fakeLampac(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lite/badloc":
			w.Header().Set("Location", "http://[::1/lite/x?uid=secret")
			w.WriteHeader(http.StatusFound)
		case "/lite/events", "/lite/alpha":
			w.Header().Set("Location", "http://[::1/lite/events?uid=secret")
			w.WriteHeader(http.StatusFound)
		case "/lite/reset":
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Write([]byte("HTTP/1.1 200 OK\r\nX-Bad uid=secret\r\n\r\n"))
			c.Close()
		default:
			w.Write([]byte(`{}`))
		}
	}))
	for _, p := range []string{"/lite/badloc", "/lite/reset"} {
		_, err := l.Get(context.Background(), p)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "uid") {
			t.Fatalf("%s: %v", p, err)
		}
	}
	var buf bytes.Buffer
	s := &Service{L: l, Refs: NewRefs([]byte("k"), l.Base), Log: slog.New(slog.NewTextHandler(&buf, nil)),
		Titles: func(context.Context, string, int) (TitleCtx, error) {
			return TitleCtx{Title: "Альфа", Year: 2021}, nil
		}}
	_, err := s.Sources(context.Background(), adult(), "movie", 1)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("sources: %v", err)
	}
	_, err = s.Browse(context.Background(), adult(), s.Refs.Mint("/lite/alpha", RefContext{Kind: "movie", TMDBID: 1}))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("browse: %v", err)
	}
	if st, _ := s.Status(context.Background()); st.Reachable {
		t.Fatal("status: reachable")
	}
	if buf.Len() == 0 || strings.Contains(buf.String(), "secret") || strings.Contains(buf.String(), "uid=") {
		t.Fatalf("log: %q", buf.String())
	}
}

// A client going away is context.Canceled (not «unavailable»); a
// saturated Vault is not a source failing.
func TestLampacCanceledAndBusy(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	l := fakeLampac(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := l.Get(ctx, "/lite/x"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("canceled: %v", err)
	}
	l.init()
	for range maxCalls {
		l.sem <- struct{}{}
	}
	defer func() {
		for range maxCalls {
			<-l.sem
		}
	}()
	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	_, err := l.Get(short, "/lite/x")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, errTimeout) {
		t.Fatalf("busy: %v", err)
	}
}
