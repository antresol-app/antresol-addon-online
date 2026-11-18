package online

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionCaps(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }))
	defer cdn.Close()
	now := time.Unix(1_800_000_000, 0)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String()),
		Now: func() time.Time { return now }}
	r := Resolved{Kind: "mp4", URL: "http://cdn.example:" + portOf(cdn) + "/v.mp4"}
	tick := func() { now = now.Add(time.Second) }

	// a user's third session replaces the oldest
	a1, _ := p.Open(1, r, "")
	tick()
	a2, _ := p.Open(1, r, "")
	tick()
	a3, err := p.Open(1, r, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := get(p, 1, p.URL(a1, r.URL, "")); w.Code != 404 {
		t.Fatalf("oldest session kept: %d", w.Code)
	}
	for _, s := range []string{a2, a3} {
		if w := get(p, 1, p.URL(s, r.URL, "")); w.Code != 200 {
			t.Fatalf("session %s: %d", s, w.Code)
		}
	}
	// replaceSession: only one's own
	b1, err := p.Open(2, r, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(1, r, a3); err != nil {
		t.Fatal(err)
	}
	if w := get(p, 1, p.URL(a3, r.URL, "")); w.Code != 404 {
		t.Fatalf("replaced session kept: %d", w.Code)
	}
	if _, err := p.Open(2, r, a2); err != nil {
		t.Fatal(err)
	}
	if w := get(p, 1, p.URL(a2, r.URL, "")); w.Code != 200 {
		t.Fatalf("another user replaced my session: %d", w.Code)
	}
	// 1: a2 a4; 2: b1 b2; 3: c1 c2 → 6 active; a 7th is refused
	for i := 0; i < 2; i++ {
		if _, err := p.Open(3, r, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.CanOpen(4, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("CanOpen: %v", err)
	}
	if _, err := p.Open(4, r, ""); !errors.Is(err, ErrBusy) || ErrBusy.Error() != "Сейчас смотрят слишком много онлайн-видео. Попробуйте позже." {
		t.Fatalf("7th session: %v", err)
	}
	// replacing one's own oldest at the cap is no new session
	if _, err := p.Open(1, r, ""); err != nil {
		t.Fatalf("own replacement at the cap: %v", err)
	}
	// sessions idle for a minute no longer count as active
	now = now.Add(61 * time.Second)
	keep := p.URL(b1, r.URL, "")
	if w := get(p, 2, keep); w.Code != 200 {
		t.Fatalf("b1: %d", w.Code)
	}
	if _, err := p.Open(4, r, ""); err != nil {
		t.Fatalf("after a minute idle: %v", err)
	}
	// a session idle for 31 minutes is gone
	now = now.Add(31 * time.Minute)
	if w := get(p, 2, keep); w.Code != 404 {
		t.Fatalf("idle 31 min: %d", w.Code)
	}
	// closing: only the owner
	c, err := p.Open(5, r, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Close(6, c) || !p.Close(5, c) || p.Close(5, c) {
		t.Fatal("close")
	}
}

// slowCDN streams video forever (a chunk every 20 ms) until the client
// goes; started counts the streams.
func slowCDN(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var started atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		started.Add(1)
		for {
			if _, err := w.Write(make([]byte, 1024)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &started
}

func TestInFlightRelaysKeepSessionsAndCount(t *testing.T) {
	cdn, started := slowCDN(t)
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String()), Now: clock}
	// on failure too: end every relay before the server closes
	t.Cleanup(func() {
		p.mu.Lock()
		for id := range p.sessions {
			p.drop(id)
		}
		p.mu.Unlock()
	})
	r := Resolved{Kind: "mp4", URL: "http://cdn.example:" + portOf(cdn) + "/v.mp4"}
	// six users, each one long MP4 range request in flight
	var sids []string
	done := make(chan struct{}, 6)
	for u := int64(1); u <= 6; u++ {
		sid, err := p.Open(u, r, "")
		if err != nil {
			t.Fatal(err)
		}
		sids = append(sids, sid)
		path := p.URL(sid, r.URL, "")
		go func() { get(p, u, path); done <- struct{}{} }()
	}
	waitFor(t, func() bool { return started.Load() == 6 })
	// a minute without new requests: still relaying, so still active
	advance(61 * time.Second)
	if _, err := p.Open(7, r, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("7th session while 6 relay: %v", err)
	}
	// 31 minutes into one request: the session is kept (a seek works)
	advance(31 * time.Minute)
	if _, err := p.Open(7, r, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("relaying sessions pruned: %v", err)
	}
	if !p.Close(1, sids[0]) {
		t.Fatal("a relaying session was dropped as idle")
	}
	// Close ended its relay
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the relay in flight")
	}
	// replacing ends the old session's relay too
	if _, err := p.Open(2, r, sids[1]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("replace did not end the relay in flight")
	}
	for _, sid := range sids[2:] {
		p.mu.Lock()
		p.drop(sid)
		p.mu.Unlock()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}

// stallWriter is a ResponseWriter whose writes record the deadline set
// before them.
type stallWriter struct {
	httptest.ResponseRecorder
	deadlines []time.Time
}

func (s *stallWriter) SetWriteDeadline(t time.Time) error {
	s.deadlines = append(s.deadlines, t)
	return nil
}

func TestRelaySetsWriteDeadline(t *testing.T) {
	w := &stallWriter{ResponseRecorder: *httptest.NewRecorder()}
	p := &Proxy{Key: proxyKey}
	p.relay(w, strings.NewReader(strings.Repeat("x", 100<<10)), &session{})
	if len(w.deadlines) < 4 || w.deadlines[len(w.deadlines)-1] != (time.Time{}) {
		t.Fatalf("deadlines %v", w.deadlines)
	}
	for _, d := range w.deadlines[:len(w.deadlines)-1] {
		if time.Until(d) < 50*time.Second || time.Until(d) > relayWriteTimeout {
			t.Fatalf("deadline %v", time.Until(d))
		}
	}
}

// The total cap holds on use too: a session idle past a minute that comes
// back while six others are active is refused, not a seventh stream.
func TestSessionCapOnResume(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }))
	defer cdn.Close()
	now := time.Unix(1_800_000_000, 0)
	p := &Proxy{Key: proxyKey, HTTP: testNet(t, "lampac.test:9", []string{"cdn.example"}, cdn.Listener.Addr().String()),
		Now: func() time.Time { return now }}
	r := Resolved{Kind: "mp4", URL: "http://cdn.example:" + portOf(cdn) + "/v.mp4"}
	paused, err := p.Open(1, r, "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // paused: no longer active
	var active []string
	for u := int64(2); u <= 7; u++ {
		sid, err := p.Open(u, r, "")
		if err != nil {
			t.Fatalf("user %d: %v", u, err)
		}
		active = append(active, sid)
	}
	if w := get(p, 1, p.URL(paused, r.URL, "")); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), ErrBusy.Error()) {
		t.Fatalf("resumed over the cap: %d %s", w.Code, w.Body.String())
	}
	// an active one goes on; once one is closed, the paused one resumes
	if w := get(p, 2, p.URL(active[0], r.URL, "")); w.Code != 200 {
		t.Fatalf("active: %d", w.Code)
	}
	if !p.Close(3, active[1]) {
		t.Fatal("close")
	}
	if w := get(p, 1, p.URL(paused, r.URL, "")); w.Code != 200 {
		t.Fatalf("resume under the cap: %d", w.Code)
	}
}

// One profile holds at most callsPerUser of Lampac's call slots: its
// further calls wait, another profile's go through.
func TestLampacCallsPerUser(t *testing.T) {
	var cur, peak atomic.Int32
	release := make(chan struct{})
	l := fakeLampac(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		cur.Add(-1)
		w.Write([]byte(`{"ok":1}`))
	}))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Get(ForUser(context.Background(), 1), "/lite/x") }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for cur.Load() < callsPerUser && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := cur.Load(); n != callsPerUser {
		t.Fatalf("one profile holds %d slots", n)
	}
	wg.Add(1)
	go func() { defer wg.Done(); l.Get(ForUser(context.Background(), 2), "/lite/y") }()
	for cur.Load() < callsPerUser+1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := cur.Load(); n != callsPerUser+1 {
		t.Fatalf("another profile waits: %d", n)
	}
	close(release)
	wg.Wait()
	if peak.Load() > maxCalls {
		t.Fatalf("peak %d", peak.Load())
	}
}

// ffmpeg subtitle conversions are capped server-wide: with every slot
// taken a conversion waits, then gives up without running ffmpeg.
func TestSubtitleConversionsCapped(t *testing.T) {
	s := &Subs{Dir: t.TempDir(), FFmpeg: filepath.Join(t.TempDir(), "no-ffmpeg")}
	for range maxConversions {
		if !convSlots.tryAcquire() {
			t.Fatal("slots taken")
		}
	}
	defer func() {
		for range maxConversions {
			convSlots.release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.convert(ctx, "srt", "1\n00:00:01,000 --> 00:00:02,000\nx\n")
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("%v", err)
	}
}
