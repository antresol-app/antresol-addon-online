package online

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/sync/singleflight"
)

var (
	// ErrOff: Lampac is not configured or «Смотреть онлайн» is off (404).
	ErrOff = errors.New("online streaming is off")
	// ErrUnknownTitle: not a movie or series with a TMDB id.
	ErrUnknownTitle = errors.New("Неизвестный фильм или сериал")
	// ErrTitleUnavailable: the title's details (and rating) cannot be read.
	ErrTitleUnavailable = errors.New("TMDB сейчас недоступен. Попробуйте позже")
)

const (
	defaultSourcesBudget = 15 * time.Second
	// DefaultSourcesBudget is defaultSourcesBudget, for «Настройки».
	DefaultSourcesBudget = defaultSourcesBudget
	defaultLevelBudget   = 20 * time.Second
	// probeTMDB is the title the admin status lists the sources of.
	probeTMDB = 438631
	// maxCachedLevel: bigger level answers are not kept.
	maxCachedLevel = 512 << 10
)

// TitleCtx is what the adapter needs to know of a TMDB title: the card
// Lampac searches by and the rating the gate checks.
type TitleCtx struct {
	Title, OriginalTitle, OriginalLanguage, Rating, IMDb string
	Year                                                 int
	Serial                                               bool
	// Poster is the TMDB poster's URL ("": none), for «Продолжить
	// просмотр»; it goes to clients only through the image cache.
	Poster string
	// Fallback: the library's card, after a TMDB read failed (cached for
	// titleMissTTL, not a day).
	Fallback bool
}

// Source is one working online source of a title.
type Source struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Quality string `json:"quality"`
	Ref     string `json:"ref"`
}

// StatusDTO is the admin status of the sidecar.
type StatusDTO struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	Sources    int    `json:"sources"`
	Version    string `json:"version"`
}

// Service is «Смотреть онлайн» on top of Lampac.
type Service struct {
	L *Lampac
	// Titles reads a TMDB title (ErrUnknownTitle when there is none).
	Titles func(ctx context.Context, kind string, id int) (TitleCtx, error)
	Refs   *Refs
	// Enabled is the admin setting «Смотреть онлайн» (nil: on).
	Enabled func() bool
	Log     *slog.Logger

	// Prefs is the admin's choice of sources (nil: all, in Lampac's
	// order), read on every list.
	Prefs func() SourcePrefs

	// SourcesBudget bounds a source list (ids and events; 15 s), and
	// LevelBudget one level of a source (20 s). Zero: the defaults.
	SourcesBudget, LevelBudget time.Duration

	// Upstream fetches streams, probes and subtitles (nil: the guarded
	// newUpstreamClient for the Lampac origin).
	Upstream *http.Client
	// Proxy relays the streams of play answers; Subs converts their
	// subtitles to WebVTT (nil: none are offered).
	Proxy *Proxy
	Subs  *Subs

	once   sync.Once
	titles *ttlCache[titleKey, TitleCtx]
	// titleMiss remembers failed title reads for a while (a TMDB that is
	// down is not asked again by every request), and flights coalesce
	// concurrent reads of one title.
	titleMiss *ttlCache[titleKey, error]
	tmu       sync.Mutex
	flights   map[titleKey]*titleFlight
	ids       *ttlCache[titleKey, [2]string]
	events    *ttlCache[titleKey, []Source]
	levels    *ttlCache[string, []byte]
	calls     *ttlCache[string, []byte]
	up        upstream

	qmu     sync.Mutex
	quality map[qualityKey]string // the quality a profile last asked for, per title

	sf singleflight.Group // one Lampac request per level or call at a time

	kmu   sync.Mutex
	known map[string]bool // balancer → listed shown and non-rch by the last events naming it
}

type titleKey struct {
	kind string
	id   int
}

func (s *Service) init() {
	s.once.Do(func() {
		s.titles = newTTLCache[titleKey, TitleCtx](2000, 24*time.Hour)
		s.titleMiss = newTTLCache[titleKey, error](2000, titleMissTTL)
		s.flights = map[titleKey]*titleFlight{}
		s.ids = newTTLCache[titleKey, [2]string](2000, 24*time.Hour)
		// a title's sources change slowly, and asking again costs 3–10 s;
		// refs stay valid (a gone source fails its browse, which says so)
		s.events = newTTLCache[titleKey, []Source](500, eventsTTL)
		s.levels = newTTLCache[string, []byte](2000, 5*time.Minute)
		s.calls = newTTLCache[string, []byte](500, 2*time.Minute)
		c := s.Upstream
		if c == nil {
			c = upstreamFor(s.L)
		}
		s.up = upstream{http: c, lampac: s.L}
		if s.Proxy != nil && s.Proxy.Subs == nil {
			s.Proxy.Subs = s.Subs // before the first session opens
		}
	})
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) on() bool { return s != nil && s.L != nil && (s.Enabled == nil || s.Enabled()) }

func budget(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// titleMissTTL is how long a failed title read is remembered, and
// titleFetchTimeout bounds one read (it outlives a caller that gives up).
const (
	titleMissTTL      = time.Minute
	titleFetchTimeout = 15 * time.Second
)

type titleFlight struct {
	done chan struct{}
	t    TitleCtx
	err  error
}

// title reads (and caches for a day) a title's card and rating. A failed
// read is remembered for a minute; concurrent reads of one title share
// one TMDB call, which runs on even when its callers stop waiting.
func (s *Service) title(ctx context.Context, kind string, id int) (TitleCtx, error) {
	if !validKind(kind) || id <= 0 {
		return TitleCtx{}, ErrUnknownTitle
	}
	k := titleKey{kind, id}
	if t, ok := s.titles.get(ctx, k); ok {
		return t, nil
	}
	if err, ok := s.titleMiss.get(ctx, k); ok {
		return TitleCtx{}, err
	}
	if s.Titles == nil {
		return TitleCtx{}, ErrTitleUnavailable
	}
	s.tmu.Lock()
	f, ok := s.flights[k]
	if !ok {
		f = &titleFlight{done: make(chan struct{})}
		s.flights[k] = f
		go s.fetchTitle(context.WithoutCancel(ctx), k, f)
	}
	s.tmu.Unlock()
	select {
	case <-f.done:
		return f.t, f.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return TitleCtx{}, ctx.Err()
		}
		// too slow: later readers do not wait for this read either (an
		// answer that comes still fills the cache, which is read first)
		s.titleMiss.put(k, ErrTitleUnavailable)
		return TitleCtx{}, ErrTitleUnavailable
	}
}

func (s *Service) fetchTitle(ctx context.Context, k titleKey, f *titleFlight) {
	ctx, cancel := context.WithTimeout(ctx, titleFetchTimeout)
	defer cancel()
	t, err := s.Titles(ctx, k.kind, k.id)
	switch {
	case errors.Is(err, ErrUnknownTitle):
		f.err = ErrUnknownTitle
	case err != nil:
		s.log().Warn("online: title", "kind", k.kind, "tmdb", k.id, "err", err)
		f.err = ErrTitleUnavailable
	default:
		t.Serial = k.kind == "series"
		f.t = t
	}
	switch {
	case f.err != nil:
		s.titleMiss.put(k, f.err)
	case f.t.Fallback:
		s.titles.putFor(k, f.t, titleMissTTL)
	default:
		s.titles.put(k, f.t)
	}
	s.tmu.Lock()
	delete(s.flights, k)
	s.tmu.Unlock()
	close(f.done)
}

// Title is a title's card and rating (cached for a day), for the online
// entries of «Продолжить просмотр».
func (s *Service) Title(ctx context.Context, kind string, id int) (TitleCtx, error) {
	if !s.on() {
		return TitleCtx{}, ErrOff
	}
	s.init()
	return s.title(ctx, kind, id)
}

// Gate is the kid and rating gate of a title for u (the progress routes);
// it answers the title it read.
func (s *Service) Gate(ctx context.Context, u User, kind string, id int) (TitleCtx, error) {
	if !s.on() {
		return TitleCtx{}, ErrOff
	}
	if u.IsKid {
		return TitleCtx{}, ErrKid
	}
	s.init()
	t, err := s.title(ctx, kind, id)
	if err != nil {
		return TitleCtx{}, err
	}
	return t, Allowed(u, t.Rating)
}

// card is the Lampac card of a title, with the IMDb id from TMDB and the
// KP id from Lampac (a failing lookup only loses the KP id).
func (s *Service) card(ctx context.Context, kind string, id int, t TitleCtx) Q {
	q := Q{TMDBID: id, IMDb: t.IMDb, Title: t.Title, OriginalTitle: t.OriginalTitle, OriginalLanguage: t.OriginalLanguage,
		Year: t.Year, Serial: t.Serial}
	k := titleKey{kind, id}
	if v, ok := s.ids.get(ctx, k); ok {
		q.IMDb, q.KP = v[0], v[1]
		return q
	}
	imdb, kp, err := s.L.ExternalIDs(ctx, id, t.Serial, t.IMDb)
	if err != nil {
		s.log().Warn("online: external ids", "lampac", redactLampac(s.L.Base.String()), "tmdb", id, "err", err)
		return q
	}
	s.ids.put(k, [2]string{imdb, kp})
	q.IMDb, q.KP = imdb, kp
	return q
}

var balanserRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Sources lists the working sources of a title: gate, ids, /lite/events
// (within SourcesBudget), the show:true and non-rch ones in Lampac's
// order. A list is cached for 10 minutes.
func (s *Service) Sources(ctx context.Context, u User, kind string, tmdbID int) ([]Source, error) {
	if !s.on() {
		return nil, ErrOff
	}
	s.init()
	if u.IsKid {
		return nil, ErrKid
	}
	ctx = ForUser(ctx, u.ID) // its Lampac calls share the profile's slots
	ctx, cancel := context.WithTimeout(ctx, budget(s.SourcesBudget, defaultSourcesBudget))
	defer cancel()
	t, err := s.title(ctx, kind, tmdbID)
	if err != nil {
		return nil, err
	}
	if err := Allowed(u, t.Rating); err != nil {
		return nil, err
	}
	k := titleKey{kind, tmdbID}
	if v, ok := s.events.get(ctx, k); ok {
		return s.prefs().arrange(v), nil
	}
	q := s.card(ctx, kind, tmdbID, t)
	// the title page's background warm-up and the picker may ask at once;
	// the flight fills the cache itself, so a warm-up or a request that
	// went away still leaves the list for the next one
	out, err := sharedDo(s, ctx, fmt.Sprintf("events %s %d", kind, tmdbID), func(ctx context.Context) ([]Source, error) {
		evs, err := s.L.Events(ctx, q)
		if err != nil {
			return nil, err
		}
		out := s.sourcesOf(evs, kind, tmdbID, q.values().Encode())
		if len(out) > 0 {
			s.events.put(k, out)
		}
		return out, nil
	})
	if errors.Is(err, ErrNothing) {
		return []Source{}, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	if err != nil {
		s.log().Warn("online: events", "lampac", redactLampac(s.L.Base.String()), "tmdb", tmdbID, "err", err)
		if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrAuth) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return s.prefs().arrange(out), nil
}

// sourcesOf are the sources of a title's events: the show:true and
// non-rch ones, with their refs. It remembers which balancers they list
// as such (Source accepts only those).
func (s *Service) sourcesOf(evs []Event, kind string, tmdbID int, card string) []Source {
	out := []Source{}
	s.kmu.Lock()
	if s.known == nil {
		s.known = map[string]bool{}
	}
	for _, e := range evs {
		if balanserRe.MatchString(e.Balanser) && len(s.known) < 1000 {
			s.known[e.Balanser] = e.Show && !e.Rch
		}
	}
	s.kmu.Unlock()
	for _, e := range evs {
		if !e.Show || e.Rch || !balanserRe.MatchString(e.Balanser) {
			continue
		}
		p, err := s.Refs.clean(e.URL)
		if err != nil {
			s.log().Warn("online: source link refused", "source", e.Balanser, "err", err)
			continue
		}
		ref, err := s.Refs.mint(withCard(p, card), RefContext{Kind: kind, TMDBID: tmdbID, Source: e.Balanser})
		if err != nil {
			continue
		}
		name, quality := cutQuality(e.Name)
		if name == "" {
			name = e.Balanser
		}
		out = append(out, Source{ID: e.Balanser, Name: name, Quality: quality, Ref: ref})
	}
	return out
}

// knownSource: the last events that named balancer id listed it shown
// and not rch.
func (s *Service) knownSource(id string) bool {
	s.kmu.Lock()
	defer s.kmu.Unlock()
	return s.known[id]
}

// Source is one source of a title without its list: the ref of
// /lite/<id> with the title card, minted as Sources mints it (for a
// resume from «Продолжить просмотр», which knows the source and need not
// wait 3–10 s for /lite/events). The gate is that of Sources. A cached
// list answers from itself (ErrNothing: the source is not in it). Else a
// balancer the last events naming it listed as shown and non-rch gets
// its ref at once (one that is gone for this title fails its browse,
// and the client falls back to the list); any other id waits for the
// title's own list, as Sources.
func (s *Service) Source(ctx context.Context, u User, kind string, tmdbID int, id string) (Source, error) {
	if !s.on() {
		return Source{}, ErrOff
	}
	s.init()
	if u.IsKid {
		return Source{}, ErrKid
	}
	if !balanserRe.MatchString(id) || s.sourceOff(id) {
		return Source{}, ErrNothing
	}
	ctx = ForUser(ctx, u.ID)
	ctx, cancel := context.WithTimeout(ctx, budget(s.SourcesBudget, defaultSourcesBudget))
	defer cancel()
	t, err := s.title(ctx, kind, tmdbID)
	if err != nil {
		return Source{}, err
	}
	if err := Allowed(u, t.Rating); err != nil {
		return Source{}, err
	}
	if v, ok := s.events.get(ctx, titleKey{kind, tmdbID}); ok {
		for _, src := range v {
			if src.ID == id {
				return src, nil
			}
		}
		return Source{}, ErrNothing
	}
	if !s.knownSource(id) {
		srcs, err := s.Sources(ctx, u, kind, tmdbID)
		if err != nil {
			return Source{}, err
		}
		for _, src := range srcs {
			if src.ID == id {
				return src, nil
			}
		}
		return Source{}, ErrNothing
	}
	card := s.card(ctx, kind, tmdbID, t).values().Encode()
	ref, err := s.Refs.mint(withCard("/lite/"+id, card), RefContext{Kind: kind, TMDBID: tmdbID, Source: id})
	if err != nil {
		return Source{}, ErrNothing
	}
	return Source{ID: id, Name: id, Ref: ref}, nil
}

// cardKeys are the query keys of a title card (Lampac's own client adds
// them to every link it follows; its deeper links lack some).
var cardKeys = []string{"id", "source", "imdb_id", "kinopoisk_id", "title", "original_title", "original_language", "year", "serial"}

// withCard adds to p the card keys of card (an encoded query) that p does
// not have; p's own values win.
func withCard(p, card string) string {
	base, q, _ := strings.Cut(p, "?")
	have := map[string]bool{}
	for _, kv := range strings.Split(q, "&") {
		k, _, _ := strings.Cut(kv, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		have[strings.ToLower(k)] = true
	}
	cv, _ := url.ParseQuery(card)
	parts := []string{}
	if q != "" {
		parts = append(parts, q)
	}
	for _, k := range cardKeys {
		if v := cv.Get(k); v != "" && !have[k] {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	if len(parts) == 0 {
		return base
	}
	return base + "?" + strings.Join(parts, "&")
}

// cardOf is the card part of a ref path's query.
func cardOf(p string) string {
	_, q, _ := strings.Cut(p, "?")
	v, _ := url.ParseQuery(q)
	out := url.Values{}
	for _, k := range cardKeys {
		if x := v.Get(k); x != "" {
			out.Set(k, x)
		}
	}
	return out.Encode()
}

// Browse opens one level of a source: the ref is checked and its title
// gated again, then Lampac is asked (within LevelBudget; cached 5
// minutes). Each child gets a ref that carries the context forward. A
// file ref gives its level with only that file. A «pick» level with
// exactly one candidate within a year of the title is followed.
func (s *Service) Browse(ctx context.Context, u User, ref string) (Level, error) {
	if !s.on() {
		return Level{}, ErrOff
	}
	s.init()
	if u.IsKid {
		return Level{}, ErrKid
	}
	ctx = ForUser(ctx, u.ID) // its Lampac calls share the profile's slots
	p, rc, err := s.Refs.Open(ref)
	if err != nil {
		return Level{}, ErrBadRef
	}
	ctx, cancel := context.WithTimeout(ctx, budget(s.LevelBudget, defaultLevelBudget))
	defer cancel()
	t, err := s.title(ctx, rc.Kind, rc.TMDBID)
	if err != nil {
		return Level{}, err
	}
	if err := Allowed(u, t.Rating); err != nil {
		return Level{}, err
	}
	lv, err := s.level(ctx, p, rc)
	if err != nil {
		return Level{}, err
	}
	if lv.Level == "pick" && rc.File == 0 {
		if c, ok := autoPick(lv.Candidates, t.Year); ok {
			if next, err := s.level(ctx, c.path, rc); err == nil {
				return next, nil
			} else if !errors.Is(err, ErrNothing) {
				return Level{}, err
			}
		}
	}
	return lv, nil
}

// autoPick is the one candidate within a year of the title, if exactly one.
func autoPick(cs []Candidate, year int) (Candidate, bool) {
	if year <= 0 {
		return Candidate{}, false
	}
	var hit []Candidate
	for _, c := range cs {
		if c.Year > 0 && c.Year >= year-1 && c.Year <= year+1 {
			hit = append(hit, c)
		}
	}
	if len(hit) != 1 {
		return Candidate{}, false
	}
	return hit[0], true
}

// level fetches, parses and signs the level at p.
func (s *Service) level(ctx context.Context, p string, rc RefContext) (Level, error) {
	b, err := s.levelBody(ctx, p)
	if err != nil {
		return Level{}, err
	}
	lv, err := parseLevel(b, rc)
	if err != nil {
		return Level{}, err
	}
	localizeEpisodes(ctx, lv.Episodes)
	return s.sign(lv, p, rc)
}

// localizeEpisodes shows the made-up names of episodes («Серия 3») in the
// request's language (after parseLevel: the item ids keep the Russian).
func localizeEpisodes(ctx context.Context, eps []Episode) {
	for i, e := range eps {
		if e.Name == fmt.Sprintf(episodeName, e.Episode) {
			eps[i].Name = i18n.T(ctx, episodeName, e.Episode)
		}
	}
}

// levelBody is Lampac's answer at p (cached unless bypassed). A slow
// source is that source failing: a timeout here is ErrSourceFailed.
func (s *Service) levelBody(ctx context.Context, p string) ([]byte, error) {
	if b, ok := s.levels.get(ctx, p); ok {
		return b, nil
	}
	b, err := s.shared(ctx, "level "+p, func(ctx context.Context) ([]byte, error) {
		b, err := s.L.Get(ctx, p)
		if err == nil && len(b) <= maxCachedLevel {
			s.levels.put(p, b) // in the flight: kept when every caller left
		}
		return b, err
	})
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	if err != nil {
		if errors.Is(err, errTimeout) {
			err = fmt.Errorf("%w: timeout", ErrSourceFailed) // no longer ErrUnavailable
		}
		s.log().Info("online: level", "lampac", redactLampac(s.L.Base.String()), "source", sourceOf(p), "err", err)
		return nil, err
	}
	return b, nil
}

// eventsTTL: how long a title's list of sources is kept.
const eventsTTL = 6 * time.Hour

// shared runs fetch once for concurrent callers of the same key (the row
// badges and the chosen source ask for one level at once). The fetch keeps
// the first caller's deadline but not its cancellation, so one caller
// leaving does not fail the others; each stops waiting at its own end.
func (s *Service) shared(ctx context.Context, key string, fetch func(context.Context) ([]byte, error)) ([]byte, error) {
	return sharedDo(s, ctx, key, fetch)
}

// sharedDo is shared for any answer (a title's events too).
func sharedDo[T any](s *Service, ctx context.Context, key string, fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	ch := s.sf.DoChan(key, func() (any, error) {
		fctx := context.WithoutCancel(ctx)
		if d, ok := ctx.Deadline(); ok {
			var cancel context.CancelFunc
			fctx, cancel = context.WithDeadline(fctx, d)
			defer cancel()
		}
		return fetch(fctx)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return zero, r.Err
		}
		return r.Val.(T), nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return zero, fmt.Errorf("%w: %w", ErrUnavailable, errTimeout) // as the Lampac client says it
		}
		return zero, ctx.Err()
	}
}

// sourceOf is the source name of a /lite/<source>… path (for logs).
func sourceOf(p string) string {
	rest := strings.TrimPrefix(p, "/lite/")
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// sign gives every child of lv its ref. Links from the answer must be
// /lite/ paths on the Lampac origin (others are dropped) and get the
// card keys they lack; files and episodes point back to p.
func (s *Service) sign(lv Level, p string, rc RefContext) (Level, error) {
	card := cardOf(p)
	child := func(raw string, c RefContext) (string, string, bool) {
		cp, err := s.Refs.clean(raw)
		if err != nil {
			s.log().Info("online: link refused", "source", rc.Source, "err", err)
			return "", "", false
		}
		cp = withCard(cp, card)
		ref, err := s.Refs.mint(cp, c)
		return cp, ref, err == nil
	}
	activeVoice := rc.Voice
	// episodes of a level opened without a voice point at the active
	// voice's link: a re-fetch plays that voice even when Lampac's
	// default changes
	episodesPath := p
	voices := lv.Voices[:0:0]
	for _, v := range lv.Voices {
		c := rc
		c.Voice, c.Episode, c.File = v.Name, 0, 0
		if cp, ref, ok := child(v.path, c); ok {
			v.path, v.Ref = cp, ref
			voices = append(voices, v)
			if v.Active && rc.Voice == "" {
				activeVoice, episodesPath = v.Name, cp
			}
		}
	}
	lv.Voices = voices
	seasons := lv.Seasons[:0:0]
	for _, se := range lv.Seasons {
		c := rc
		c.Season, c.Episode, c.File = se.Number, 0, 0
		if cp, ref, ok := child(se.path, c); ok {
			se.path, se.Ref = cp, ref
			seasons = append(seasons, se)
		}
	}
	lv.Seasons = seasons
	cands := lv.Candidates[:0:0]
	for _, cd := range lv.Candidates {
		c := rc
		c.File = 0
		if cp, ref, ok := child(cd.path, c); ok {
			cd.path, cd.Ref = cp, ref
			cands = append(cands, cd)
		}
	}
	lv.Candidates = cands
	for i := range lv.Files {
		c := rc
		c.Voice, c.File, c.ID, c.L = first(lv.Files[i].Voice, rc.Voice), i+1, lv.ids[i], lv.loose[i]
		ref, err := s.Refs.mint(p, c)
		if err != nil {
			return Level{}, ErrBadRef
		}
		lv.Files[i].Ref = ref
	}
	for i := range lv.Episodes {
		c := rc
		c.Season, c.Episode, c.Voice, c.File, c.ID, c.L = lv.Episodes[i].Season, lv.Episodes[i].Episode, activeVoice, i+1, lv.ids[i], lv.loose[i]
		ref, err := s.Refs.mint(episodesPath, c)
		if err != nil {
			return Level{}, ErrBadRef
		}
		lv.Episodes[i].Ref = ref
	}
	if rc.File > 0 {
		return only(lv, rc)
	}
	if (lv.Level == "seasons" && len(lv.Seasons) == 0) || (lv.Level == "pick" && len(lv.Candidates) == 0) {
		return Level{}, ErrNothing
	}
	return lv, nil
}

// only keeps the file (or episode) a file ref points to (matched by its
// identity); the play data stay whole, so lv.PlayItemFor(c) still finds it.
func only(lv Level, c RefContext) (Level, error) {
	n := lv.match(c)
	switch {
	case n == 0:
		return Level{}, ErrNothing
	case lv.Level == "movie":
		lv.Files = lv.Files[n-1 : n]
	case lv.Level == "episodes":
		lv.Episodes = lv.Episodes[n-1 : n]
	default:
		return Level{}, ErrNothing
	}
	return lv, nil
}

// Status lists the sources of a fixed probe title (admin settings).
func (s *Service) Status(ctx context.Context) (StatusDTO, error) {
	// whatever the admin setting: off is not «not configured»
	if s == nil || s.L == nil {
		return StatusDTO{}, ErrOff
	}
	s.init()
	ctx, cancel := context.WithTimeout(ctx, budget(s.SourcesBudget, defaultSourcesBudget))
	defer cancel()
	st := StatusDTO{Configured: true}
	t, err := s.title(ctx, "movie", probeTMDB)
	if err != nil {
		t = TitleCtx{}
	}
	evs, err := s.L.Events(ctx, s.card(ctx, "movie", probeTMDB, t))
	switch {
	case err == nil, errors.Is(err, ErrNothing):
		st.Reachable = true
		for _, e := range evs {
			if e.Show && !e.Rch && balanserRe.MatchString(e.Balanser) {
				st.Sources++
			}
		}
	default:
		s.log().Warn("online: status", "lampac", redactLampac(s.L.Base.String()), "err", err)
	}
	st.Version = s.L.Server()
	return st, nil
}

// Resolved is what a file ref plays: the stream URL (hls or mp4) that
// answered, the request headers its source needs, its subtitles, and the
// qualities to choose from. Never sent to clients: the proxy relays it.
type Resolved struct {
	Kind      string // hls | mp4
	URL       string
	Headers   http.Header
	Subtitles []Sub
	Qualities []string
	Quality   string
	Ctx       RefContext
	// CookieSite is the source's own site (that of its default stream):
	// its Cookie header goes there only.
	CookieSite string
	// root: the playlist at URL as the probe reads it (nil: not kept),
	// for the proxy's first request.
	root *rootRead
}

const (
	resolveBudget = 45 * time.Second
	probeTimeout  = 10 * time.Second
	probeBytes    = 512
	// rootReadTime bounds reading the rest of a root playlist once the
	// probe answered (playback does not wait for it).
	rootReadTime = 3 * time.Second
)

type maxHeightKey struct{}

// WithMaxHeight caps the quality Resolve picks by default (the remote
// quality cap, in lines; 0: none).
func WithMaxHeight(ctx context.Context, h int) context.Context {
	return context.WithValue(ctx, maxHeightKey{}, h)
}

func maxHeightOf(ctx context.Context) int { h, _ := ctx.Value(maxHeightKey{}).(int); return h }

type qualityKey struct {
	user int64
	kind string
	tmdb int
}

// Resolve finds what a file ref plays now. The ref is checked and its
// title gated again; the level is fetched again (fresh with bypass, after
// a stale link) and the file found by its identity; a call is asked
// (its answer cached 2 minutes unless bypassed). The quality is the one
// asked for, else the one asked for last on this title, else the best
// within the remote cap (WithMaxHeight), else the best; each of its
// " or " URLs, then the other qualities, is probed by its first 512
// bytes, in that order but staggered (race): an HLS playlist or an MP4
// plays, DASH is ErrUnsupported.
func (s *Service) Resolve(ctx context.Context, u User, ref, quality string, bypass bool) (Resolved, error) {
	if !s.on() {
		return Resolved{}, ErrOff
	}
	s.init()
	if u.IsKid {
		return Resolved{}, ErrKid
	}
	ctx = ForUser(ctx, u.ID) // its Lampac calls share the profile's slots
	p, rc, err := s.Refs.Open(ref)
	if err != nil || rc.File == 0 {
		return Resolved{}, ErrBadRef
	}
	ctx, cancel := context.WithTimeout(ctx, resolveBudget)
	defer cancel()
	t, err := s.title(ctx, rc.Kind, rc.TMDBID)
	if err != nil {
		return Resolved{}, err
	}
	if err := Allowed(u, t.Rating); err != nil {
		return Resolved{}, err
	}
	if bypass {
		ctx = Bypass(ctx)
	}
	lctx, lcancel := context.WithTimeout(ctx, budget(s.LevelBudget, defaultLevelBudget))
	b, err := s.levelBody(lctx, p)
	lcancel()
	if err != nil {
		return Resolved{}, err
	}
	lv, err := parseLevel(b, rc)
	if err != nil {
		return Resolved{}, err
	}
	item, _, err := lv.PlayItemFor(rc)
	if err != nil {
		return Resolved{}, err
	}
	if item.Method == "call" {
		v, err := s.call(ctx, item.URLs[0], rc)
		if err != nil {
			return Resolved{}, err
		}
		if len(v.Headers) == 0 {
			v.Headers = item.Headers
		}
		if len(v.Subtitles) == 0 {
			v.Subtitles = item.Subtitles
		}
		if v.SubtitlesCall == "" {
			v.SubtitlesCall = item.SubtitlesCall
		}
		item = v
	}
	if unsupported(item) {
		return Resolved{}, ErrUnsupported
	}
	// subtitles behind a call are asked beside the probes, not before them
	var subsCh chan []Sub
	if len(item.Subtitles) == 0 && item.SubtitlesCall != "" {
		subsCh = make(chan []Sub, 1)
		go func() { subsCh <- s.subtitlesCall(ctx, item.SubtitlesCall, rc) }()
	}
	h := sanitizeHeaders(item.Headers)
	quals := item.Qualities
	if len(quals) == 0 {
		quals = []QualityURLs{{URLs: item.URLs}}
	}
	site := ""
	if len(item.URLs) > 0 {
		site = siteOfURL(item.URLs[0])
	} else if len(quals[0].URLs) > 0 {
		site = siteOfURL(quals[0].URLs[0])
	}
	qk := qualityKey{u.ID, rc.Kind, rc.TMDBID}
	asked := indexOf(quals, quality)
	sawDASH, failed := false, 0
	var cands []candidate
	for _, i := range qualityOrder(quals, asked, indexOf(quals, s.remembered(qk)), maxHeightOf(ctx)) {
		for _, raw := range quals[i].URLs {
			if isMPD(raw) {
				sawDASH = true
				continue
			}
			if n := len(s.up.clean(raw)); n > maxLongURL { // too long to hand out
				failed++
				s.log().Info("online: probe", "source", rc.Source, "quality", quals[i].Name, "err", "URL too long", "len", n)
				continue
			}
			cands = append(cands, candidate{raw: raw, quality: i})
		}
	}
	won, pr, dash, bad, err := s.race(ctx, cands, func(c candidate) http.Header { return forSite(h, site, c.raw) },
		func(c candidate, err error) {
			s.log().Info("online: probe", "source", rc.Source, "quality", quals[c.quality].Name, "err", err)
		})
	if err != nil {
		return Resolved{}, err
	}
	if won >= 0 {
		if asked >= 0 {
			s.remember(qk, quality)
		}
		c := cands[won]
		subs := item.Subtitles
		if subsCh != nil {
			// a slow answer is not waited for: it fills the cache for the next play
			select {
			case subs = <-subsCh:
			case <-time.After(subsCallWait):
			}
		}
		return Resolved{Kind: pr.kind, URL: c.raw, Headers: h, Subtitles: subs, Qualities: names(item.Qualities),
			Quality: quals[c.quality].Name, Ctx: rc, CookieSite: site, root: pr.root}, nil
	}
	sawDASH, failed = sawDASH || dash, failed+bad
	if sawDASH && failed == 0 {
		return Resolved{}, ErrUnsupported
	}
	return Resolved{}, fmt.Errorf("%w: no stream answered", ErrSourceFailed)
}

// candidate is a stream URL Resolve may play: an " or " URL of quality
// (an index into the qualities).
type candidate struct {
	raw     string
	quality int
}

// probeStagger is how long a probe has before the next candidate is
// probed beside it (a dead CDN costs this, not probeTimeout).
var probeStagger = 1500 * time.Millisecond

// race probes cands in order, staggered: the next one starts when the
// previous fails, or after probeStagger without an answer. The first that
// plays wins unless an earlier one is still being probed; then the
// earlier ones get up to probeStagger more, and the earliest that plays
// wins. The rest are cancelled. It answers the winner (-1: none) and its
// kind, whether one was DASH, and how many failed otherwise (an ended
// budget counts as one); err only for a caller that went away.
func (s *Service) race(ctx context.Context, cands []candidate, headers func(candidate) http.Header,
	logFail func(candidate, error)) (won int, pr probed, dash bool, failed int, err error) {
	if len(cands) == 0 {
		return -1, probed{}, false, 0, nil
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel() // the losers stop
	type result struct {
		i   int
		pr  probed
		err error
	}
	results := make(chan result, len(cands))
	next := 0
	start := func() {
		i := next
		next++
		go func() {
			pr, err := s.probe(rctx, cands[i].raw, headers(cands[i]))
			results <- result{i, pr, err}
		}()
	}
	pending := map[int]bool{}
	ok := map[int]probed{} // candidates that play
	best := func() int {   // the earliest that plays, -1 while one before it is pending
		for i := 0; i < next; i++ {
			if _, good := ok[i]; good {
				return i
			}
			if pending[i] {
				return -1
			}
		}
		return -1
	}
	stagger := time.NewTimer(probeStagger)
	defer stagger.Stop()
	var grace <-chan time.Time
	pending[next] = true
	start()
	for {
		if b := best(); b >= 0 {
			ok[b].keepRoot()
			return b, ok[b], dash, failed, nil
		}
		if len(pending) == 0 && next >= len(cands) {
			return -1, probed{}, dash, failed, nil
		}
		select {
		case r := <-results:
			delete(pending, r.i)
			switch {
			case r.err == nil:
				ok[r.i] = r.pr
				if grace == nil {
					grace = time.After(probeStagger)
				}
			case ctx.Err() != nil:
				return gone(ctx, dash, failed)
			case errors.Is(r.err, ErrUnsupported):
				dash = true
			default:
				failed++
				logFail(cands[r.i], r.err)
			}
			if r.err != nil && len(ok) == 0 && next < len(cands) { // a failure: the next one at once
				pending[next] = true
				start()
				stagger.Reset(probeStagger)
			}
		case <-stagger.C:
			if len(ok) == 0 && next < len(cands) {
				pending[next] = true
				start()
				stagger.Reset(probeStagger)
			}
		case <-grace:
			for i := 0; i < next; i++ { // the earlier ones had their time
				if _, good := ok[i]; good {
					ok[i].keepRoot()
					return i, ok[i], dash, failed, nil
				}
			}
		case <-ctx.Done():
			return gone(ctx, dash, failed)
		}
	}
}

// gone is race's answer once ctx ended: the caller went away (its error),
// or the budget ran out (nothing answered).
func gone(ctx context.Context, dash bool, failed int) (int, probed, bool, int, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return -1, probed{}, dash, failed, ctx.Err()
	}
	return -1, probed{}, dash, failed + 1, nil
}

// call asks a call URL (only ever through Lampac.Get), cached 2 minutes.
func (s *Service) call(ctx context.Context, raw string, rc RefContext) (PlayItem, error) {
	b, ok := s.calls.get(ctx, raw)
	if !ok {
		var err error
		b, err = s.shared(ctx, "call "+raw, func(ctx context.Context) ([]byte, error) {
			b, err := s.L.Get(ctx, raw)
			if err == nil {
				s.calls.put(raw, b) // in the flight: kept when every caller left
			}
			return b, err
		})
		if errors.Is(err, context.Canceled) {
			return PlayItem{}, err
		}
		if err != nil {
			if errors.Is(err, errTimeout) {
				err = fmt.Errorf("%w: timeout", ErrSourceFailed)
			}
			s.log().Info("online: call", "lampac", redactLampac(s.L.Base.String()), "source", rc.Source, "err", err)
			return PlayItem{}, err
		}
	}
	return parseVideo(b)
}

// subsCallBudget bounds a subtitles_call; subsCallWait is how long a
// play that already has its stream waits for one.
const subsCallBudget = 8 * time.Second

var subsCallWait = 2 * time.Second

// subtitlesCall asks a source's subtitles_call (a Lampac link, only ever
// through Lampac.Get; cached as a call) for the subtitles of what plays.
// Any failure is no subtitles: they never fail a play.
func (s *Service) subtitlesCall(ctx context.Context, raw string, rc RefContext) []Sub {
	ctx, cancel := context.WithTimeout(ctx, subsCallBudget)
	defer cancel()
	key := "subs " + raw
	b, ok := s.calls.get(ctx, key)
	if !ok {
		var err error
		b, err = s.shared(ctx, key, func(ctx context.Context) ([]byte, error) {
			b, err := s.L.Get(ctx, raw)
			if err == nil {
				s.calls.put(key, b)
			}
			return b, err
		})
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.log().Info("online: subtitles call", "source", rc.Source, "err", err)
			}
			return nil
		}
	}
	return parseSubs(b)
}

// probed is what a probe found: the kind ("hls" or "mp4") and, for a
// playlist answered with 200, its read (the proxy serves its first
// request from it).
type probed struct {
	kind string
	root *rootRead
}

// rootRead is a root playlist being read after its probe answered: b
// (nil: not kept) and its final URL once done is closed.
type rootRead struct {
	done chan struct{}
	b    []byte
	base *url.URL
	// keep detaches the read from the probe's context (the race's: the
	// losers' reads end with it); false when that already ended.
	keep func() bool
}

// keepRoot detaches the winner's root read from the race (nil: none).
func (pr probed) keepRoot() {
	if pr.root != nil && pr.root.keep != nil {
		pr.root.keep()
	}
}

// probe reads the first bytes of raw: "hls" or "mp4", ErrUnsupported for
// DASH, an error otherwise. It answers as soon as the first bytes say
// which; the rest of a playlist answered with 200 is read in the
// background (within maxPlaylist and rootReadTime; otherwise it is only
// not kept), while ctx lasts unless the read is kept.
func (s *Service) probe(ctx context.Context, raw string, h http.Header) (probed, error) {
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	unlink := context.AfterFunc(ctx, bcancel)
	background := false
	defer func() {
		if !background {
			unlink()
			bcancel()
		}
	}()
	hh := h.Clone()
	if !m3u8Path(raw) { // a playlist is asked whole: one source answers a ranged one with an empty 200
		hh.Set("Range", fmt.Sprintf("bytes=0-%d", probeBytes-1))
	}
	resp, err := s.up.get(bctx, raw, hh)
	if err != nil {
		return probed{}, err
	}
	defer func() {
		if !background {
			resp.Body.Close()
		}
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return probed{}, fmt.Errorf("%w: HTTP %d", ErrSourceFailed, resp.StatusCode)
	}
	head := make([]byte, probeBytes)
	n, rerr := io.ReadFull(resp.Body, head)
	head = head[:n]
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case isPlaylist(head):
		pr := probed{kind: "hls"}
		if resp.StatusCode == http.StatusOK && n == probeBytes {
			rr := &rootRead{done: make(chan struct{}), base: resp.Request.URL, keep: unlink}
			background = true
			go func() {
				defer close(rr.done)
				defer bcancel()
				defer resp.Body.Close()
				t := time.AfterFunc(rootReadTime, bcancel)
				defer t.Stop()
				rest, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylist+1-probeBytes))
				if b := append(head, rest...); err == nil && len(b) <= maxPlaylist {
					rr.b = b
				}
			}()
			pr.root = rr
		} else if resp.StatusCode == http.StatusOK && errors.Is(rerr, io.ErrUnexpectedEOF) {
			rr := &rootRead{done: make(chan struct{}), b: head, base: resp.Request.URL} // all of it
			close(rr.done)
			pr.root = rr
		}
		return pr, nil
	case bytes.Contains(head, []byte("<MPD")) || mt == "application/dash+xml":
		return probed{}, ErrUnsupported
	case bytes.Contains(head, []byte("ftyp")) || bytes.Contains(head, []byte("moov")) || mt == "video/mp4":
		return probed{kind: "mp4"}, nil
	}
	return probed{}, fmt.Errorf("%w: not a stream", ErrSourceFailed)
}

// requestHeaders are the headers a source may ask the proxy to send.
var requestHeaders = map[string]bool{"Referer": true, "Origin": true, "User-Agent": true, "Cookie": true, "Accept": true, "Accept-Language": true}

// sanitizeHeaders keeps the allowed headers with valid values (no CR, LF,
// NUL), and sends UserAgent when the source names none.
func sanitizeHeaders(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		name := http.CanonicalHeaderKey(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if !requestHeaders[name] || v == "" || len(v) > maxCarriedLen || strings.ContainsAny(v, "\r\n\x00") || !httpguts.ValidHeaderFieldValue(v) {
			continue
		}
		h.Set(name, v)
	}
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", UserAgent)
	}
	return h
}

func indexOf(q []QualityURLs, name string) int {
	if name == "" {
		return -1
	}
	for i, x := range q {
		if strings.EqualFold(x.Name, name) {
			return i
		}
	}
	return -1
}

// qualityOrder is the order qualities are tried in (q is best first): the
// one asked for, else remembered, else the best within maxHeight, else
// the best; then the lower ones, then the higher ones (closest first).
func qualityOrder(q []QualityURLs, asked, remembered, maxHeight int) []int {
	pick := asked
	if pick < 0 {
		pick = remembered
	}
	if pick < 0 {
		pick = 0
		if maxHeight > 0 {
			for i, x := range q {
				if h := qualityHeight(x.Name); h <= maxHeight {
					pick = i
					break
				}
			}
		}
	}
	out := []int{pick}
	for i := pick + 1; i < len(q); i++ {
		out = append(out, i)
	}
	for i := pick - 1; i >= 0; i-- {
		out = append(out, i)
	}
	return out
}

const maxRemembered = 10000

func (s *Service) remembered(k qualityKey) string {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	return s.quality[k]
}

func (s *Service) remember(k qualityKey, q string) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	if s.quality == nil || len(s.quality) >= maxRemembered {
		s.quality = map[qualityKey]string{}
	}
	s.quality[k] = q
}
