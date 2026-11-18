package online

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
)

const (
	// maxPlaylist bounds an upstream playlist the proxy rewrites. Real
	// media playlists of a film with long signed segment URLs (~3.4 KB
	// each, ~3000 segments) reach 5–6 MB, so 4 MiB is too little.
	maxPlaylist = 16 << 20
	// maxBlobURL bounds a URL the proxy hands out: its blob (+28 bytes,
	// base64) and path stay under ~6 KB, inside a front proxy's 8 KB
	// request line (nginx large_client_header_buffers 4 8k).
	maxBlobURL = 4096
	// maxLongURL bounds any URL the proxy hands out; one over maxBlobURL
	// (Lampac's own /proxy/ URLs reach ~6.4 KB) is kept in its session
	// and its blob seals a short reference.
	maxLongURL = 16 << 10
	// maxLongBytes bounds the long URLs one session keeps.
	maxLongBytes = 4 << 20
	// maxTokenBytes bounds the playlist URLs one session keeps for its
	// tokens (a film's media playlist of ~1400 URLs of 3.7 KB is ~5 MB),
	// and maxTokenTotal those of all sessions; past them a playlist is
	// rewritten with sealed blobs, as before tokens.
	maxTokenBytes = 32 << 20
	maxTokenTotal = 192 << 20
	// tokenPrefix starts a token (never a base64url blob).
	tokenPrefix = "~"
	// rewritesAtOnce bounds the playlist rewrites of the whole server.
	rewritesAtOnce = 4
	// relayWriteTimeout: a client that takes no bytes for this long is
	// dropped (its upstream connection with it).
	relayWriteTimeout = 60 * time.Second
	// livenessTick: how often a long relay marks its session used.
	livenessTick = 10 * time.Second
	// rootTTL: how long the root playlist a probe read is kept for the
	// session's first request of it.
	rootTTL = 30 * time.Second
)

// StreamBase is the client path the proxy's sessions are under by
// default: the addon's /api/media/… as the core's proxy serves it to the app
// (/api/addons/online/x/media/<sid>/…, the contract's media prefix, where
// the core lets a media key in for GET and HEAD).
const StreamBase = "/api/addons/online/x/media/"

// Proxy relays online streams: a session holds what Resolve found (the
// URL, the headers the source needs, the subtitles) and clients fetch
// <Base><sid>/<blob>. The blob is base64url(nonce + AES-256-GCM
// of the URL), with the sid as associated data: clients cannot read it,
// and it opens only on its own session. A session serves only URLs it
// handed out (the root and the URIs of the playlists it rewrote). The
// URIs of a rewritten playlist are short tokens, <Base><sid>/~<n>:
// the session keeps their URLs (within maxTokenBytes, else they are
// sealed blobs too), and a token opens only one of those. Playlists are
// rewritten (gzipped when the client takes it); everything else is
// streamed through with Range.
type Proxy struct {
	Key  []byte
	HTTP *http.Client // the upstream client (newUpstreamClient)
	// Lampac is the sidecar: URLs on its origin are fetched with its auth
	// query (nil: none are).
	Lampac *Lampac
	Now    func() time.Time
	Log    *slog.Logger
	// Subs converts the subtitles a play answer did not wait for, on
	// their first fetch (nil: the Service's Subs).
	Subs *Subs
	// Base is the client path the sessions are under, ending in "/"
	// ("": StreamBase). Every URL the proxy hands out (play URLs,
	// subtitles, the URIs of rewritten playlists) is <Base><sid>/….
	Base string

	perUser, total int // 0: 2 and 6

	once     sync.Once
	aead     cipher.AEAD
	rewrites chan struct{}

	mu         sync.Mutex
	sessions   map[string]*session
	limit      keyedLimit
	tokenTotal int // the token bytes of all sessions
}

type session struct {
	id            string
	user          int64
	r             Resolved
	rootSite      string // the site of the root URL: Cookie goes only there
	created, last time.Time
	inflight      int // requests being served
	seen          map[[16]byte]struct{}
	ctx           context.Context // ended by Close, replace and prune
	cancel        context.CancelFunc
	rewrite       chan struct{} // one playlist rewrite at a time
	long          []string      // URLs over maxBlobURL, by reference
	longIdx       map[[16]byte]int
	longBytes     int
	// tok are the URLs of the session's rewritten playlists, by token
	// ("~" + base36 index): a token opens only a URL this session's
	// upstream playlists named.
	tok      []string
	tokIdx   map[[16]byte]int
	tokBytes int
	// tokHeld: token bytes a rewrite in flight reserved in tokenTotal
	// before it started and has not used yet.
	tokHeld int
	// dropped: the session ended (p.mu held): a rewrite still in flight
	// mints no more tokens, and its bytes were given back once, by drop.
	dropped bool
	// root is the probe's read of the root playlist, served once in
	// place of a second fetch until rootUntil.
	root      *rootRead
	rootUntil time.Time
}

// longRef is the sealed form of a reference to session.long[n].
const longRef = "\x00long:"

// base is Base, else StreamBase.
func (p *Proxy) base() string {
	if p.Base != "" {
		return p.Base
	}
	return StreamBase
}

func (p *Proxy) init() {
	p.once.Do(func() {
		k := sha256.Sum256(append([]byte("vault-online-blob\x00"), p.Key...))
		block, err := aes.NewCipher(k[:])
		if err != nil {
			panic(err) // a 32-byte key never fails
		}
		p.aead, err = cipher.NewGCM(block)
		if err != nil {
			panic(err)
		}
		p.rewrites = make(chan struct{}, rewritesAtOnce)
	})
}

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Proxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Proxy) caps() (perUser, total int) {
	perUser, total = p.perUser, p.total
	if perUser <= 0 {
		perUser = sessionsPerUser
	}
	if total <= 0 {
		total = sessionsTotal
	}
	return perUser, total
}

func (p *Proxy) up() upstream { return upstream{http: p.HTTP, lampac: p.Lampac} }

// live: the session serves a request now or had one within sessionActive.
func (s *session) live(now time.Time) bool { return s.inflight > 0 || now.Sub(s.last) < sessionActive }

// gone: idle for sessionIdle with nothing in flight.
func (s *session) gone(now time.Time) bool { return s.inflight == 0 && now.Sub(s.last) >= sessionIdle }

// drop ends a session and its requests in flight (p.mu held).
func (p *Proxy) drop(id string) {
	if s := p.sessions[id]; s != nil {
		s.cancel()
		s.dropped = true
		p.tokenTotal -= s.tokBytes + s.tokHeld
		s.tok, s.tokIdx, s.tokBytes, s.tokHeld = nil, nil, 0, 0
		s.root = nil
		delete(p.sessions, id)
	}
}

// prune drops idle sessions (p.mu held).
func (p *Proxy) prune(now time.Time) {
	for id, s := range p.sessions {
		if s.gone(now) {
			p.drop(id)
		}
	}
}

// plan is what opening a session of u would replace; ErrBusy when the
// server already relays its six (p.mu held).
func (p *Proxy) plan(u int64, replace string, now time.Time) (map[string]bool, error) {
	perUser, total := p.caps()
	p.prune(now)
	drop := map[string]bool{}
	if s := p.sessions[replace]; replace != "" && s != nil && s.user == u {
		drop[replace] = true
	}
	var mine []*session
	for _, s := range p.sessions {
		if s.user == u && !drop[s.id] {
			mine = append(mine, s)
		}
	}
	for len(mine) >= perUser {
		oldest := 0
		for i, s := range mine {
			if s.created.Before(mine[oldest].created) {
				oldest = i
			}
		}
		drop[mine[oldest].id] = true
		mine = append(mine[:oldest], mine[oldest+1:]...)
	}
	active := 0
	for id, s := range p.sessions {
		if !drop[id] && s.live(now) {
			active++
		}
	}
	if active >= total {
		return nil, ErrBusy
	}
	return drop, nil
}

// CanOpen says whether Open would succeed now (checked before the work of
// resolving a title).
func (p *Proxy) CanOpen(u int64, replace string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.plan(u, replace, p.now())
	return err
}

// Open starts a session of user u for r. It replaces the session replace
// when u owns it, and u's oldest session when u already has two (their
// requests in flight end). With six active sessions on the server (one
// serving a request, or with a request within a minute; other than those
// it replaces) it is ErrBusy, and nothing is replaced.
func (p *Proxy) Open(u int64, r Resolved, replace string) (string, error) {
	p.init()
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[string]*session{}
	}
	drop, err := p.plan(u, replace, now)
	if err != nil {
		return "", err
	}
	for id := range drop {
		p.drop(id)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{id: base64.RawURLEncoding.EncodeToString(b[:]), user: u, r: r, created: now, last: now,
		seen: map[[16]byte]struct{}{}, ctx: ctx, cancel: cancel, rewrite: make(chan struct{}, 1)}
	if r.root != nil {
		s.root, s.rootUntil = r.root, now.Add(rootTTL)
		// the read (up to 16 MiB) goes after rootTTL even when the player
		// never asks for the root (takeRoot drops it then too)
		time.AfterFunc(rootTTL, func() {
			p.mu.Lock()
			s.root = nil
			p.mu.Unlock()
		})
	}
	s.r.root = nil
	s.rootSite = r.CookieSite
	if s.rootSite == "" {
		s.rootSite = siteOfURL(r.URL)
	}
	p.sessions[s.id] = s
	s.seen[urlID(s.id, p.up().clean(r.URL))] = struct{}{}
	return s.id, nil
}

// Close ends u's session sid and its requests in flight (the player's
// close); false: not u's.
func (p *Proxy) Close(u int64, sid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.sessions[sid]; s != nil && s.user == u && !s.gone(p.now()) {
		p.drop(sid)
		return true
	}
	return false
}

// urlID is how a session remembers a URL it handed out.
func urlID(sid, raw string) [16]byte {
	h := sha256.New()
	h.Write([]byte(sid))
	h.Write([]byte{0})
	h.Write([]byte(raw))
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// seal is the blob of raw on session sid.
func (p *Proxy) seal(sid, raw string) string {
	p.init()
	nonce := make([]byte, p.aead.NonceSize(), p.aead.NonceSize()+len(raw)+p.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(p.aead.Seal(nonce, nonce, []byte(raw), []byte(sid)))
}

// open is the URL of a blob of session sid.
func (p *Proxy) open(sid, blob string) (string, bool) {
	p.init()
	if len(blob) > 8<<10 {
		return "", false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(blob)
	ns := p.aead.NonceSize()
	if err != nil || len(b) < ns+p.aead.Overhead() {
		return "", false
	}
	raw, err := p.aead.Open(nil, b[:ns], b[ns:], []byte(sid))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// URL is the client path of raw in session sid (with ?mk= when mk is
// set); the session may serve it from now on.
func (p *Proxy) URL(sid, raw, mk string) string {
	path, _ := p.path(sid, p.up().clean(raw))
	return path + mkQuery(mk)
}

// SubURL is the client path of subtitle n of session sid.
func (p *Proxy) SubURL(sid string, n int, mk string) string {
	return p.base() + sid + "/subs/" + strconv.Itoa(n) + ".vtt" + mkQuery(mk)
}

// path seals raw for sid and lets the session serve it. A URL over
// maxBlobURL is kept in the session (at most maxLongBytes of them) and
// its blob seals a reference; false when it does not fit.
func (p *Proxy) path(sid, raw string) (string, bool) {
	if len(raw) > maxLongURL {
		return "", false
	}
	id := urlID(sid, raw)
	sealed := raw
	p.mu.Lock()
	s := p.sessions[sid]
	if s != nil && len(raw) > maxBlobURL {
		n, ok := s.longIdx[id]
		if !ok {
			if s.longBytes+len(raw) > maxLongBytes {
				p.mu.Unlock()
				return "", false
			}
			if s.longIdx == nil {
				s.longIdx = map[[16]byte]int{}
			}
			n = len(s.long)
			s.long = append(s.long, raw)
			s.longIdx[id] = n
			s.longBytes += len(raw)
		}
		sealed = longRef + strconv.Itoa(n)
	}
	if s != nil && len(s.seen) < maxSeen {
		s.seen[id] = struct{}{}
	}
	p.mu.Unlock()
	if s == nil && len(raw) > maxBlobURL {
		return "", false
	}
	return p.base() + sid + "/" + p.seal(sid, sealed), true
}

// token is the client path of raw (a URL of a playlist of session s) by
// token; false when the session was dropped, or the URL fits neither the
// bytes held for the rewrite nor the session's and the server's token
// bytes (p.mu held).
func (p *Proxy) token(s *session, raw string) (string, bool) {
	if s.dropped {
		return "", false
	}
	id := urlID(s.id, raw)
	n, ok := s.tokIdx[id]
	if !ok {
		switch {
		case len(s.tok) >= maxSeen:
			return "", false
		case s.tokHeld >= len(raw):
			s.tokHeld -= len(raw) // already in tokenTotal
		case s.tokBytes+s.tokHeld+len(raw) > maxTokenBytes || p.tokenTotal+len(raw) > maxTokenTotal:
			return "", false
		default:
			p.tokenTotal += len(raw)
		}
		if s.tokIdx == nil {
			s.tokIdx = map[[16]byte]int{}
		}
		n = len(s.tok)
		s.tok = append(s.tok, raw)
		s.tokIdx[id] = n
		s.tokBytes += len(raw)
	}
	return p.base() + s.id + "/" + tokenPrefix + strconv.FormatInt(int64(n), 36), true
}

// tokenLen is the longest token path of session s with n more URLs.
func (p *Proxy) tokenLen(s *session, n int) int {
	return len(p.base()) + len(s.id) + 1 + len(tokenPrefix) + len(strconv.FormatInt(int64(len(s.tok)+n), 36))
}

// pathLen is the length of path(sid, raw) for a URL of n bytes (for the
// rewrite's bounds).
func (p *Proxy) pathLen(sid string, n int) int {
	p.init()
	if n > maxBlobURL {
		n = len(longRef) + 8
	}
	return len(p.base()) + len(sid) + 1 + base64.RawURLEncoding.EncodedLen(p.aead.NonceSize()+n+p.aead.Overhead())
}

// find is u's live session sid (not marked used).
func (p *Proxy) find(sid string, u int64) (*session, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[sid]
	if s == nil || s.user != u {
		return nil, false
	}
	if s.gone(p.now()) {
		p.drop(sid)
		return nil, false
	}
	return s, true
}

// begin and end bracket a request of s: in flight it is active and never
// idles out. A session that is not active (idle past a minute) becomes
// active again only under the total cap: false, ErrBusy, otherwise.
func (p *Proxy) begin(s *session) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !s.live(now) {
		_, total := p.caps()
		active := 0
		for _, o := range p.sessions {
			if o != s && o.live(now) {
				active++
			}
		}
		if active >= total {
			return false
		}
	}
	s.inflight++
	s.last = now
	return true
}

func (p *Proxy) end(s *session) {
	p.mu.Lock()
	s.inflight--
	s.last = p.now()
	p.mu.Unlock()
}

func (p *Proxy) alive(s *session) {
	p.mu.Lock()
	s.last = p.now()
	p.mu.Unlock()
}

// verify opens a blob of session s: sealed for s, of a URL s handed out.
func (p *Proxy) verify(s *session, blob string) (string, bool) {
	if t, isTok := strings.CutPrefix(blob, tokenPrefix); isTok {
		n, err := strconv.ParseInt(t, 36, 32)
		p.mu.Lock()
		defer p.mu.Unlock()
		if err != nil || n < 0 || int(n) >= len(s.tok) || t != strconv.FormatInt(n, 36) {
			return "", false
		}
		return s.tok[n], true
	}
	raw, ok := p.open(s.id, blob)
	if !ok {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ref, isRef := strings.CutPrefix(raw, longRef); isRef {
		n, err := strconv.Atoi(ref)
		if err != nil || n < 0 || n >= len(s.long) {
			return "", false
		}
		raw = s.long[n]
	}
	_, seen := s.seen[urlID(s.id, raw)]
	return raw, seen
}

// Serve answers GET (and HEAD) <Base><sid>/<blob> for the user of
// r's context (a session or a media key of the session's owner).
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, sid, blob string) {
	u, _ := UserFrom(r.Context())
	s, ok := p.find(sid, u.ID)
	if !ok {
		writeError(w, r, http.StatusNotFound, ErrNoSession.Error())
		return
	}
	raw, ok := p.verify(s, blob) // before the session counts as used
	if !ok {
		writeError(w, r, http.StatusForbidden, ErrBadRef.Error())
		return
	}
	if !p.limit.acquire(sid) {
		writeError(w, r, http.StatusTooManyRequests, errTooManyStreams.Error())
		return
	}
	defer p.limit.release(sid)
	if !p.begin(s) {
		writeError(w, r, http.StatusTooManyRequests, ErrBusy.Error())
		return
	}
	defer p.end(s)
	// the request ends with the session (Close, replace, prune)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()

	up := p.up()
	if body, base, ok := p.takeRoot(ctx, s, raw, r.Method); ok {
		p.playlist(ctx, w, r, s, bytes.NewReader(body), base, up.isLampac(base))
		return
	}
	h := s.r.Headers.Clone()
	if h == nil {
		h = http.Header{}
	}
	h = forSite(h, s.rootSite, raw)
	m3u8 := m3u8Path(raw)
	if !m3u8 { // a playlist is read whole
		for _, k := range []string{"Range", "If-Range"} {
			if v := r.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
	}
	if v := r.Header.Get("Accept"); v != "" && h.Get("Accept") == "" {
		h.Set("Accept", v)
	}
	resp, err := up.get(ctx, raw, h)
	if err != nil {
		if ctx.Err() != nil {
			return // the player went away, or the session ended
		}
		p.log().Info("online: proxy", "err", err)
		writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		expired(w, r)
		return
	case http.StatusRequestedRangeNotSatisfiable:
		if cr := resp.Header.Get("Content-Range"); validHeader(cr) {
			w.Header().Set("Content-Range", cr)
		}
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	default:
		p.log().Info("online: proxy", "status", resp.StatusCode)
		writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
		return
	}
	fromLampac := up.isLampac(resp.Request.URL)
	br := bufio.NewReaderSize(resp.Body, 32<<10)
	head, _ := br.Peek(64)
	if isPlaylist(head) {
		if resp.StatusCode != http.StatusOK {
			p.log().Info("online: proxy", "refused", "partial playlist")
			writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
			return
		}
		p.playlist(ctx, w, r, s, br, resp.Request.URL, fromLampac)
		return
	}
	if m3u8 {
		// a .m3u8 that is not a playlist (an error page with 200)
		writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
		return
	}
	if fromLampac && !lampacMedia(resp.Header.Get("Content-Type"), head, up.secret()) {
		// Lampac's own answers (JSON with the uid in its links, pages)
		// are never relayed: only media
		p.log().Info("online: proxy", "refused", "lampac non-media answer")
		writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
		return
	}
	wh := w.Header()
	wh.Set("Content-Type", streamType(resp.Header.Get("Content-Type")))
	wh.Set("X-Content-Type-Options", "nosniff")
	wh.Set("Cache-Control", "no-store")
	if resp.ContentLength >= 0 && !resp.Uncompressed {
		wh.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	for _, k := range []string{"Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(k); validHeader(v) {
			wh.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		p.relay(w, br, s)
	}
}

// playlist rewrites a playlist answer and streams it out: one rewrite per
// session and rewritesAtOnce on the server at a time, bounded in URIs and
// in output. URIs on the Lampac origin are kept only in a playlist that
// came from Lampac (a source's playlist cannot make Vault call Lampac).
func (p *Proxy) playlist(ctx context.Context, w http.ResponseWriter, r *http.Request, s *session, body io.Reader,
	base *url.URL, fromLampac bool) {
	for _, sem := range []chan struct{}{s.rewrite, p.rewrites} {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
	}
	b, err := io.ReadAll(io.LimitReader(body, maxPlaylist+1))
	if err != nil || len(b) > maxPlaylist {
		p.log().Info("online: proxy", "refused", "playlist over 16 MiB or unread")
		writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
		return
	}
	up := p.up()
	if fromLampac && echoes(b, up.secret()) {
		p.log().Info("online: proxy", "refused", "lampac playlist echoes the uid")
		writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
		return
	}
	mk, _ := ViaMediaKey(r.Context())
	q := mkQuery(mk)
	relative := relativeURIs(r.Context())
	// a master playlist lists playlists (relayExt), a media one segments
	master := relative && (bytes.Contains(b, []byte("#EXT-X-STREAM-INF")) || bytes.Contains(b, []byte("#EXT-X-MEDIA:")))
	allowed := func(abs *url.URL, raw string) (string, bool) {
		if up.isLampac(abs) && !fromLampac {
			return "", false
		}
		c := up.clean(raw)
		return c, len(c) <= maxLongURL
	}
	// the bounds first (nothing is written yet), then the session's room:
	// sealed blobs as the bound, tokens when the URLs fit the token bytes
	longSum, sealedSum, newTok := 0, 0, 0
	seen := map[[16]byte]bool{}
	sizeOf := func(abs *url.URL, raw string) (int, bool) {
		c, ok := allowed(abs, raw)
		if !ok {
			return 0, false
		}
		if len(c) > maxBlobURL {
			longSum += len(c)
		}
		if id := urlID(s.id, c); !seen[id] {
			seen[id] = true
			p.mu.Lock()
			_, have := s.tokIdx[id]
			p.mu.Unlock()
			if !have {
				newTok += len(c)
			}
		}
		n := p.pathLen(s.id, len(c)) + len(q)
		sealedSum += n
		return n, true
	}
	count, size := measureM3U8(b, base, sizeOf)
	if count > maxPlaylistURIs || size > maxRewritten {
		p.log().Info("online: proxy", "refused", "playlist over the rewrite bounds", "uris", count)
		writeError(w, r, http.StatusBadGateway, ErrSourceFailed.Error())
		return
	}
	// tokens hold their bytes in tokenTotal before the rewrite starts (the
	// other rewrites of the server cannot take them meanwhile); what is
	// left over is given back after it
	p.mu.Lock()
	tokens := !s.dropped && s.tokBytes+s.tokHeld+newTok <= maxTokenBytes && p.tokenTotal+newTok <= maxTokenTotal &&
		len(s.tok)+len(seen) <= maxSeen
	if tokens {
		s.tokHeld += newTok
		p.tokenTotal += newTok
		defer func() {
			p.mu.Lock()
			if !s.dropped { // drop gave back all it held
				p.tokenTotal -= s.tokHeld
				s.tokHeld = 0
			}
			p.mu.Unlock()
		}()
	}
	full := !tokens && (len(s.seen)+count > maxSeen || s.longBytes+longSum > maxLongBytes)
	tlen := p.tokenLen(s, len(seen))
	p.mu.Unlock()
	if full {
		// the session handed out all it may (URLs, or long URLs kept by
		// reference): the player starts a new one
		expired(w, r)
		return
	}
	if tokens && size+count*(tlen+len(q))-sealedSum > maxRewritten { // cannot be: tokens are shorter
		tokens = false
	}
	h := w.Header()
	h.Set("Content-Type", "application/vnd.apple.mpegurl")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Vary", "Accept-Encoding")
	gz := acceptsGzip(r)
	if gz {
		h.Set("Content-Encoding", "gzip")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	bw := &deadlineWriter{w: w, rc: http.NewResponseController(w)}
	defer bw.rc.SetWriteDeadline(time.Time{})
	var out io.Writer = bw
	var zw *gzip.Writer
	if gz {
		zw, _ = gzip.NewWriterLevel(bw, gzip.BestSpeed)
		out = zw
	}
	buf := bufio.NewWriterSize(out, 32<<10)
	rewriteLines(b, base, func(abs *url.URL, raw string) (string, bool) {
		c, ok := allowed(abs, raw)
		if !ok {
			return "", false
		}
		var path string
		if tokens {
			p.mu.Lock()
			path, ok = p.token(s, c)
			p.mu.Unlock()
		}
		if !ok || !tokens {
			path, ok = p.path(s.id, c)
		}
		if relative {
			// the core's relay reads it: a URI relative to the playlist's
			// folder (<Base><sid>/) stays under the stream's route; ffmpeg's
			// HLS demuxer opens only names with a media extension
			path = strings.TrimPrefix(path, p.base()+s.id+"/") + relayExt(abs, master)
		}
		return path + q, ok
	}, func(x string) bool {
		buf.WriteString(x)
		return bw.err == nil && s.ctx.Err() == nil // a dropped session's rewrite stops
	})
	buf.Flush()
	if zw != nil {
		zw.Close()
	}
}

// acceptsGzip: the client takes a gzip body (Accept-Encoding, q not 0).
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

// takeRoot is the root playlist the probe read, for a GET of the root URL
// within rootTTL of the session's start; it is served once (and dropped
// at any request after rootTTL). A read still under way is waited for
// (at most rootReadTime: no longer than fetching it again).
func (p *Proxy) takeRoot(ctx context.Context, s *session, raw, method string) ([]byte, *url.URL, bool) {
	p.mu.Lock()
	rr := s.root
	switch {
	case rr == nil:
	case !p.now().Before(s.rootUntil):
		s.root, rr = nil, nil
	case method != http.MethodGet || raw != p.up().clean(s.r.URL):
		rr = nil
	default:
		s.root = nil
	}
	p.mu.Unlock()
	if rr == nil {
		return nil, nil, false
	}
	select {
	case <-rr.done:
	case <-ctx.Done():
		return nil, nil, false
	}
	return rr.b, rr.base, rr.b != nil
}

// relay copies an upstream body to the client: each write must go through
// within relayWriteTimeout, and the session is marked used while bytes
// flow.
func (p *Proxy) relay(w http.ResponseWriter, src io.Reader, s *session) {
	dw := &deadlineWriter{w: w, rc: http.NewResponseController(w)}
	defer dw.rc.SetWriteDeadline(time.Time{})
	buf := make([]byte, 32<<10)
	tick := p.now()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dw.Write(buf[:n]); werr != nil {
				return
			}
			if now := p.now(); now.Sub(tick) >= livenessTick {
				p.alive(s)
				tick = now
			}
		}
		if err != nil {
			return
		}
	}
}

// deadlineWriter renews the connection's write deadline before each write
// (where the server supports it) and keeps the first error.
type deadlineWriter struct {
	w   io.Writer
	rc  *http.ResponseController
	err error
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	d.rc.SetWriteDeadline(time.Now().Add(relayWriteTimeout)) // unsupported (tests): no deadline
	n, err := d.w.Write(b)
	if err == nil {
		err = d.rc.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	d.err = err
	return n, err
}

// expired answers a stale link: the player resolves the title again.
func expired(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Vault-Online", "expired")
	writeError(w, r, http.StatusGone, ErrSourceFailed.Error())
}

// ServeSub answers GET <Base><sid>/subs/<n>.vtt.
func (p *Proxy) ServeSub(w http.ResponseWriter, r *http.Request, sid string, n int) {
	u, _ := UserFrom(r.Context())
	s, ok := p.find(sid, u.ID)
	if !ok || n < 0 || n >= len(s.r.Subtitles) || (s.r.Subtitles[n].File == "" && p.Subs == nil) {
		writeError(w, r, http.StatusNotFound, ErrNoSession.Error())
		return
	}
	if !p.limit.acquire(sid) {
		writeError(w, r, http.StatusTooManyRequests, errTooManyStreams.Error())
		return
	}
	defer p.limit.release(sid)
	if !p.begin(s) {
		writeError(w, r, http.StatusTooManyRequests, ErrBusy.Error())
		return
	}
	defer p.end(s)
	file := s.r.Subtitles[n].File
	if file == "" { // not converted when the play answered: now (or the warm-up's)
		var err error
		if file, err = p.Subs.Shared(r.Context(), s.r.Subtitles[n], SubHeaders(s.r, s.r.Subtitles[n])); err != nil {
			if r.Context().Err() == nil {
				p.log().Info("online: subtitle", "source", s.r.Ctx.Source, "err", err)
			}
			writeError(w, r, http.StatusNotFound, ErrNoSession.Error())
			return
		}
	}
	f, err := os.Open(file)
	if err != nil {
		writeError(w, r, http.StatusNotFound, ErrNoSession.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "subtitles.vtt", time.Time{}, f)
}

// m3u8Path: the URL names a playlist.
func m3u8Path(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".m3u")
}

// isPlaylist: the body starts as an HLS playlist.
func isPlaylist(head []byte) bool {
	head = bytes.TrimPrefix(head, []byte("\xEF\xBB\xBF"))
	return bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n"), []byte("#EXTM3U"))
}

// lampacMedia: a non-playlist answer of the Lampac origin that may be
// relayed: not JSON, script, markup or plain text, and not holding the
// uid.
func lampacMedia(ct string, head []byte, secret string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	if strings.Contains(mt, "json") || strings.Contains(mt, "javascript") || strings.Contains(mt, "xml") || strings.Contains(mt, "html") {
		return false
	}
	// text: JSON, markup or plain text (media is binary: TS, MP4, AAC,
	// and a range from the middle of a file may start with any byte)
	text := bytes.IndexFunc(head, func(r rune) bool { return r < ' ' && r != '\t' && r != '\r' && r != '\n' }) < 0
	t := bytes.TrimLeft(head, " \t\r\n")
	if text && (strings.HasPrefix(mt, "text/") || (len(t) > 0 && (t[0] == '{' || t[0] == '[' || t[0] == '<'))) {
		return false
	}
	return secret == "" || !bytes.Contains(head, []byte(secret))
}

// streamType is the Content-Type a relayed body is served with: media
// types as given, anything else (HTML, SVG, scripts) as bytes, never
// rendered on Vault's origin.
func streamType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "application/octet-stream"
	}
	switch {
	case strings.HasPrefix(mt, "video/"), strings.HasPrefix(mt, "audio/"), mt == "application/mp4":
		return mt
	}
	return "application/octet-stream"
}

// siteOf is a host's site, for the Cookie rule: an IP as it is, a name
// by its last two labels.
func siteOf(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if net.ParseIP(host) != nil {
		return host
	}
	parts := strings.Split(host, ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}

// siteOfURL is the site of raw's host ("": not a URL).
func siteOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return siteOf(u.Hostname())
}

// forSite is h for a request to raw: without Cookie unless raw is on
// site (the source's cookie is for its own site only).
func forSite(h http.Header, site, raw string) http.Header {
	if h.Get("Cookie") == "" || (site != "" && siteOfURL(raw) == site) {
		return h
	}
	h = h.Clone()
	h.Del("Cookie")
	return h
}

func validHeader(v string) bool {
	return v != "" && len(v) < 256 && !strings.ContainsAny(v, "\r\n\x00")
}

// writeError is the API's error answer: {"error":"…"}, msg (Russian, a
// catalog key) in the request's language.
func writeError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": i18n.Localize(r.Context(), msg)})
}

// relayExts are the extensions ffmpeg's HLS demuxer opens (its
// allowed_extensions and allowed_segment_extensions, the ones a source
// serves) that a relayed URI may keep.
var relayExts = map[string]bool{".m3u8": true, ".ts": true, ".m4s": true, ".mp4": true, ".m4v": true, ".m4a": true,
	".aac": true, ".ac3": true, ".eac3": true, ".mp3": true, ".vtt": true, ".webvtt": true, ".mpegts": true, ".cmfv": true, ".cmfa": true}

// relayExt is the extension a URI of a playlist the core's relay reads
// gets (ffmpeg refuses a segment or a playlist without one, as of its
// extension_picky): the upstream URL's own when ffmpeg takes it, else
// .m3u8 in a master playlist (its URIs are playlists) and .ts in a media
// one. The proxy's names never hold a dot, so cutRelayExt takes it off.
func relayExt(u *url.URL, master bool) string {
	if e := strings.ToLower(path.Ext(u.Path)); relayExts[e] {
		return e
	}
	if master {
		return ".m3u8"
	}
	return ".ts"
}

// cutRelayExt is a relayed name without the extension relayExt gave it
// (a name with another one is left as it is: no such blob or token).
func cutRelayExt(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 && relayExts[strings.ToLower(name[i:])] {
		return name[:i]
	}
	return name
}
