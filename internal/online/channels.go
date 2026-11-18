package online

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-online/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-online/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol/addonkit"
)

// ChannelExtras are «ТВ» channels' titles outside the library (the
// channels.* hooks: ready, candidates, resolve, release): films of the
// channel's genres and years from TMDB /discover that the library does
// not have, played from Lampac's HTTP balancers through the stream proxy. Never anything torrent: sources
// that look like torrent modules are not used, and a stream that looks
// like a magnet, a .torrent or TorrServer is refused (refusedStream), so
// a channel can never make this server give data to a swarm.
//
// A stream is a route of the addon's socket, ChannelPrefix<token>/<blob>
// (channels.resolve's path), that the core's own loopback relay reads for
// its ffmpeg: the proxy keeps its guards (every fetch through the guarded
// upstream client, playlists rewritten to itself, their URIs relative so
// that they stay under the stream's folder). The token is random per
// stream and is the only way to it; it goes on Release
// (channels.release).
type ChannelExtras struct {
	Online *Service
	TMDB   ChannelCatalog
	// Library is the core's library (its films' TMDB ids are left out;
	// nil: none are).
	Library ChannelLibrary
	// On is the admin setting «Смотреть онлайн» (nil: on).
	On  func(ctx context.Context) bool
	Log *slog.Logger

	// Sources: how many sources of a title are tried (default 2: one
	// fallback).
	Sources int

	mu      sync.Mutex
	streams map[string]chStream // token → the stream
}

// chStream is one open stream: its proxy session, the session's owner
// (the channel's identity) and its release.
type chStream struct {
	sid     string
	user    int64
	release func()
}

// ChannelCatalog is what ChannelExtras reads of TMDB (*tmdb.Client).
type ChannelCatalog interface {
	GenreNames(ctx context.Context, kind string) (map[int]string, error)
	DiscoverMovies(ctx context.Context, q tmdb.DiscoverQuery) ([]tmdb.DiscoverResult, error)
	Movie(ctx context.Context, id int) (tmdb.Details, error)
}

// ChannelLibrary is what ChannelExtras reads of the library
// (*addonkit.Client, permission library.read).
type ChannelLibrary interface {
	// ItemIDsByTMDB maps the TMDB ids the library has to their item ids.
	ItemIDsByTMDB(ctx context.Context, kind string, ids []int64) (map[int64]int64, error)
}

// Stream is a channel title's stream: Path is its route on the addon's
// socket (ServeHTTP answers it); Release ends it (the channels.release
// hook).
type Stream struct {
	Path    string
	Release func()
}

// ChannelPrefix is the socket route of the channels' streams:
// ChannelPrefix<token>/<blob>.
const ChannelPrefix = "/ch/"

const (
	// channelUserBase: a channel's streams are opened as profile
	// -(channelUserBase + channel), never a real one; its proxy sessions
	// (two: the one on air and the next one resolved ahead) and Lampac
	// slots are its own.
	channelUserBase = int64(1) << 40
	// discoverPages bounds the /discover pages one ask reads.
	discoverPages = 5
	// minRuntime, maxRuntime: a film outside them (shorts, unknown,
	// marathons) is not scheduled.
	minRuntime, maxRuntime = 40, 240
	// channelMaxHeight: streams are picked up to 1080p.
	channelMaxHeight = 1080
	// minVotes: only titles enough people rated (a known film resolves).
	minVotes = 150
)

// ErrRefusedStream: a stream or source that looks like a torrent.
var ErrRefusedStream = errors.New("online: a torrent stream is never played by a channel")

func (x *ChannelExtras) log() *slog.Logger {
	if x.Log != nil {
		return x.Log
	}
	return slog.Default()
}

// Ready: Lampac is configured, «Смотреть онлайн» is on, the proxy and
// TMDB are there.
func (x *ChannelExtras) Ready(ctx context.Context) bool {
	return x != nil && x.Online.on() && x.Online.Proxy != nil && x.TMDB != nil && (x.On == nil || x.On(ctx))
}

// Candidates are up to limit films for rules: TMDB /discover of the
// rules' genres (any of them; none: every genre) and years, popular
// first, not in the library, with a runtime TMDB knows and within the
// channel's age limit. Only a channel with its extras on is asked (the
// core checks it).
func (x *ChannelExtras) Candidates(ctx context.Context, r addonkit.ChannelRules, limit int) ([]addonkit.ChannelTitle, error) {
	if !x.Ready(ctx) || limit <= 0 {
		return nil, nil
	}
	q := tmdb.DiscoverQuery{YearFrom: r.YearFrom, YearTo: r.YearTo, MinVotes: minVotes}
	if len(r.Genres) > 0 {
		names, err := x.TMDB.GenreNames(ctx, "movie")
		if err != nil && len(names) == 0 {
			return nil, err
		}
		want := map[string]bool{}
		for _, g := range r.Genres {
			want[i18n.GenreKey(g)] = true
		}
		for id, n := range names {
			if want[i18n.GenreKey(n)] {
				q.GenreIDs = append(q.GenreIDs, id)
			}
		}
		if len(q.GenreIDs) == 0 {
			return nil, nil // genres TMDB has none of: nothing from outside
		}
		sortInts(q.GenreIDs)
	}
	var out []addonkit.ChannelTitle
	seen := map[int]bool{}
	for page := 1; page <= discoverPages && len(out) < limit; page++ {
		q.Page = page
		rs, err := x.TMDB.DiscoverMovies(ctx, q)
		if err != nil {
			if len(out) > 0 {
				break
			}
			return nil, err
		}
		if len(rs) == 0 {
			break
		}
		have, err := x.inLibrary(ctx, rs)
		if err != nil {
			return nil, err
		}
		for _, d := range rs {
			if len(out) >= limit {
				break
			}
			if have[d.ID] || seen[d.ID] {
				continue
			}
			seen[d.ID] = true
			det, err := x.TMDB.Movie(ctx, d.ID)
			if err != nil {
				if ctx.Err() != nil {
					return out, nil // what was found in time
				}
				continue
			}
			if det.Adult || det.Runtime < minRuntime || det.Runtime > maxRuntime {
				continue
			}
			if !library.RatingAllowed(r.MaxRating, library.Item{Kind: library.KindMovie, Rating: det.Rating}) {
				continue
			}
			t := addonkit.ChannelTitle{Key: "tmdb:movie:" + strconv.Itoa(d.ID), Kind: library.KindMovie, TMDBID: int64(d.ID),
				Title: either(det.Title, d.Title), Year: either(det.Year, d.Year), Overview: either(det.Overview, d.Overview),
				Rating: det.Rating, Genres: det.Genres, Poster: either(det.Poster, d.Poster), Backdrop: either(det.Backdrop, d.Backdrop),
				RuntimeMs: int64(det.Runtime) * 60_000}
			out = append(out, t)
		}
	}
	return out, nil
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// inLibrary are the TMDB ids of rs the library's films have.
func (x *ChannelExtras) inLibrary(ctx context.Context, rs []tmdb.DiscoverResult) (map[int]bool, error) {
	out := map[int]bool{}
	if x.Library == nil {
		return out, nil
	}
	ids := make([]int64, len(rs))
	for i, d := range rs {
		ids[i] = int64(d.ID)
	}
	m, err := x.Library.ItemIDsByTMDB(ctx, library.KindMovie, ids)
	if err != nil {
		return nil, err
	}
	for id := range m {
		out[int(id)] = true
	}
	return out, nil
}

// channelUser is the profile channel ch's streams are opened as: not a
// kid, no age limit of its own (the candidates were chosen within the
// channel's).
func channelUser(ch int64) User {
	return User{ID: -(channelUserBase + ch), Name: "channel " + strconv.FormatInt(ch, 10)}
}

// Resolve finds a stream of t for channel ch: the title's sources (not
// torrent ones), the first two in Lampac's order (one fallback), the
// film of each (a «pick» level by its year), its first playable file up
// to 1080p; the stream opens a proxy session read through the relay.
func (x *ChannelExtras) Resolve(ctx context.Context, ch int64, t addonkit.ChannelTitle) (Stream, error) {
	if !x.Ready(ctx) {
		return Stream{}, ErrOff
	}
	if t.Kind != library.KindMovie || t.TMDBID <= 0 {
		return Stream{}, ErrUnknownTitle
	}
	u := channelUser(ch)
	ctx = WithMaxHeight(ctx, channelMaxHeight)
	srcs, err := x.Online.Sources(ctx, u, "movie", int(t.TMDBID))
	if err != nil {
		return Stream{}, err
	}
	tries := x.Sources
	if tries <= 0 {
		tries = 2
	}
	var errs []error
	for _, src := range srcs {
		if tries == 0 {
			break
		}
		if refusedSource(src.ID) || refusedSource(src.Name) {
			x.log().Info("online: channel source refused (torrent)", "source", src.ID)
			continue
		}
		tries--
		st, err := x.play(ctx, u, src, t.Year)
		if err == nil {
			return st, nil
		}
		if ctx.Err() != nil {
			return Stream{}, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("%s: %w", src.ID, err))
	}
	if len(errs) == 0 {
		return Stream{}, ErrNothing
	}
	return Stream{}, errors.Join(errs...)
}

// play resolves src's film and opens it for the relay.
func (x *ChannelExtras) play(ctx context.Context, u User, src Source, year int) (Stream, error) {
	lv, err := x.Online.Browse(ctx, u, src.Ref)
	if err != nil {
		return Stream{}, err
	}
	if lv.Level == "pick" {
		var next string
		for _, c := range lv.Candidates {
			if year > 0 && c.Year > 0 && c.Year >= year-1 && c.Year <= year+1 {
				next = c.Ref
				break
			}
		}
		if next == "" {
			return Stream{}, ErrNothing
		}
		if lv, err = x.Online.Browse(ctx, u, next); err != nil {
			return Stream{}, err
		}
	}
	if lv.Level != "movie" {
		return Stream{}, ErrNothing // a series, or nothing playable
	}
	var errs []error
	for _, f := range lv.Files {
		if f.Unsupported {
			continue
		}
		res, err := x.Online.Resolve(ctx, u, f.Ref, "", false)
		if err != nil {
			if ctx.Err() != nil {
				return Stream{}, ctx.Err()
			}
			errs = append(errs, err)
			if len(errs) >= 2 {
				break
			}
			continue
		}
		if refusedStream(res.URL) {
			x.log().Warn("online: channel stream refused (torrent)", "source", src.ID)
			errs = append(errs, ErrRefusedStream)
			continue
		}
		return x.open(u, res)
	}
	if len(errs) == 0 {
		return Stream{}, ErrNothing
	}
	return Stream{}, errors.Join(errs...)
}

// open starts a proxy session of res and hands out its socket route.
func (x *ChannelExtras) open(u User, res Resolved) (Stream, error) {
	p := x.Online.Proxy
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Stream{}, err
	}
	token := hex.EncodeToString(b[:])
	sid, err := p.Open(u.ID, res, "")
	if err != nil {
		return Stream{}, err
	}
	blob, ok := strings.CutPrefix(p.URL(sid, res.URL, ""), p.base()+sid+"/")
	if !ok || blob == "" || strings.Contains(blob, "/") {
		p.Close(u.ID, sid)
		return Stream{}, ErrNothing
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			p.Close(u.ID, sid)
			x.mu.Lock()
			delete(x.streams, token)
			x.mu.Unlock()
		})
	}
	x.mu.Lock()
	if x.streams == nil {
		x.streams = map[string]chStream{}
	}
	x.streams[token] = chStream{sid: sid, user: u.ID, release: release}
	x.mu.Unlock()
	// the root's name says what it is, as its URIs do (relayExt): ffmpeg
	// takes an HLS input by its extension or its type
	if res.Kind == "hls" {
		blob += ".m3u8"
	}
	return Stream{Path: ChannelPrefix + token + "/" + blob, Release: release}, nil
}

// streamOf is the open stream of a socket route ChannelPrefix<token>/…,
// and the rest after the token.
func (x *ChannelExtras) streamOf(path string) (chStream, string, bool) {
	rest, ok := strings.CutPrefix(path, ChannelPrefix)
	if !ok {
		return chStream{}, "", false
	}
	token, rest, ok := strings.Cut(rest, "/")
	if !ok {
		return chStream{}, "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for t, st := range x.streams {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return st, rest, true
		}
	}
	return chStream{}, "", false
}

// Release ends the stream of a route Resolve gave (channels.release);
// false when it is not open.
func (x *ChannelExtras) Release(path string) bool {
	st, _, ok := x.streamOf(path)
	if ok {
		st.release()
	}
	return ok
}

// ServeHTTP answers the core's relay: GET/HEAD ChannelPrefix<token>/<blob>
// of an open stream, as the channel's identity, a playlist's URIs
// relative to it; 404 for any other route, or while the extras are off.
func (x *ChannelExtras) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	st, blob, ok := x.streamOf(r.URL.Path)
	blob = cutRelayExt(blob)
	if !ok || blob == "" || strings.Contains(blob, "/") || !x.Ready(r.Context()) {
		http.NotFound(w, r)
		return
	}
	ctx := withRelativeURIs(WithUser(r.Context(), User{ID: st.user}))
	x.Online.Proxy.Serve(w, r.WithContext(ctx), st.sid, blob)
}

// Close ends every open stream (the addon is stopping).
func (x *ChannelExtras) Close() {
	x.mu.Lock()
	var all []func()
	for _, st := range x.streams {
		all = append(all, st.release)
	}
	x.mu.Unlock()
	for _, rel := range all {
		rel()
	}
}

// torrentWords name Lampac's torrent modules and trackers.
var torrentWords = regexp.MustCompile(`(?i)torr|^ts$|^ts[_-]|jac(kett|red)?\b|magnet|btih|tracker|rutor|kinozal|nnm|megapeer|bitru|toloka|pirate|tparse|anilibria_?t`)

// refusedSource: a source id or name that looks like a torrent module.
func refusedSource(s string) bool { return s != "" && torrentWords.MatchString(s) }

var infoHash = regexp.MustCompile(`(?i)(^|[/=:])[0-9a-f]{40}([/&?.]|$)`)

// refusedStream: a stream URL a channel never plays — not plain http(s),
// a magnet or BitTorrent info hash anywhere in it (also once decoded,
// e.g. inside a proxy's query), a .torrent, or TorrServer (its port, its
// name, its /stream?link= and /play/<hash> routes, Lampac's /ts).
func refusedStream(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return true
	}
	all := strings.ToLower(raw)
	if dec, err := url.QueryUnescape(all); err == nil {
		all += " " + strings.ToLower(dec)
	}
	for _, w := range []string{"magnet:", "urn:btih", ".torrent", "torrserver", "xt=urn"} {
		if strings.Contains(all, w) {
			return true
		}
	}
	p := strings.ToLower(u.Path)
	if u.Port() == "8090" || strings.HasPrefix(p, "/ts/") || p == "/ts" || strings.HasPrefix(p, "/torrents") ||
		(strings.HasPrefix(p, "/stream") && u.Query().Has("link")) || (strings.HasPrefix(p, "/play/") && infoHash.MatchString(p)) {
		return true
	}
	return u.Query().Has("link") && infoHash.MatchString(u.Query().Get("link"))
}

func either[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
}
