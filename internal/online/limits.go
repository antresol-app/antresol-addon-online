package online

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Caps of online playback: Vault relays every byte, so the number of
// streams it relays at once is bounded.
const (
	// sessionsPerUser: a profile's third session replaces its oldest.
	sessionsPerUser = 2
	// sessionsTotal: active sessions on the server (a 7th is ErrBusy).
	sessionsTotal = 6
	// sessionActive: a session with a request this recent is active.
	sessionActive = 60 * time.Second
	// sessionIdle: a session is dropped this long after its last request.
	sessionIdle = 30 * time.Minute
	// streamsPerSession is the cap of concurrent proxy requests of one
	// session: the same as a media key's on /api/torrent-stream (AVPlayer
	// opens a few range requests at a time).
	streamsPerSession = 6
	// maxSeen bounds the URLs one session has handed out.
	maxSeen = 200_000
	// callsPerUser: one profile's Lampac calls at a time (of maxCalls),
	// so one profile's slow sources never hold every slot.
	callsPerUser = 2
	// maxConversions: ffmpeg subtitle conversions at a time, server-wide.
	maxConversions = 2
)

var (
	// ErrBusy: the server relays as many online videos as it may (429).
	ErrBusy = errors.New("Сейчас смотрят слишком много онлайн-видео. Попробуйте позже.")
	// ErrUnsupported: only DASH (or nothing playable) at that quality (415).
	ErrUnsupported = errors.New("Этот формат не поддерживается, выберите другое качество")
	// ErrNoSession: an unknown, closed, idle or someone else's session (404).
	ErrNoSession = errors.New("Сеанс не найден")
	// errTooManyStreams: the session holds streamsPerSession requests (429).
	errTooManyStreams = errors.New("Слишком много одновременных потоков")
)

// keyedLimit caps concurrent requests per key (here: a session), like the
// media-key cap of /api/torrent-stream.
type keyedLimit struct {
	mu sync.Mutex
	n  map[string]int
}

// acquire takes a slot for key; false when the key already holds max.
func (l *keyedLimit) acquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil {
		l.n = map[string]int{}
	}
	if l.n[key] >= streamsPerSession {
		return false
	}
	l.n[key]++
	return true
}

func (l *keyedLimit) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[key]--; l.n[key] <= 0 {
		delete(l.n, key)
	}
}

type userKey struct{}

// ForUser marks ctx as the work of profile id: its Lampac calls share
// that profile's callsPerUser slots.
func ForUser(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, userKey{}, id)
}

func userOf(ctx context.Context) (int64, bool) { id, ok := ctx.Value(userKey{}).(int64); return id, ok }

// userSlots caps concurrent work per profile (callsPerUser each).
type userSlots struct {
	mu   sync.Mutex
	m    map[int64]chan struct{}
	refs map[int64]int
}

// acquire waits for one of id's slots (ctx.Err() when ctx ends first).
func (u *userSlots) acquire(ctx context.Context, id int64) (release func(), err error) {
	u.mu.Lock()
	if u.m == nil {
		u.m, u.refs = map[int64]chan struct{}{}, map[int64]int{}
	}
	ch := u.m[id]
	if ch == nil {
		ch = make(chan struct{}, callsPerUser)
		u.m[id] = ch
	}
	u.refs[id]++
	u.mu.Unlock()
	unref := func() {
		u.mu.Lock()
		if u.refs[id]--; u.refs[id] <= 0 {
			delete(u.refs, id)
			delete(u.m, id)
		}
		u.mu.Unlock()
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch; unref() }, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}

// semaphore is a fixed number of slots.
type semaphore chan struct{}

func (s semaphore) acquire(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s semaphore) tryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s semaphore) release() { <-s }

// convSlots are the server's ffmpeg subtitle conversions.
var convSlots = make(semaphore, maxConversions)
