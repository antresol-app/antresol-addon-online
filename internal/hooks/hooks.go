// Package hooks answers the core's hooks for «Смотреть онлайн»: the
// library scan's seeding of online positions and the clean-up of deleted
// profiles (library.afterScan), «Продолжить» on Home (home.continue), the
// tracking scanner of kind online (tracking.scan:online), the profiles'
// online watch states (tracking.watched) and the «ТВ» titles outside the
// library (channels.ready, channels.candidates, channels.resolve,
// channels.release).
package hooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/antresol-app/antresol-addon-online/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol-addon-online/internal/store"
	"github.com/antresol-app/antresol/addonkit"
)

// Names are the hooks Register answers (the manifest declares them).
var Names = []string{"library.afterScan", "home.continue", "tracking.scan:online", "tracking.watched",
	"channels.ready", "channels.candidates", "channels.resolve", "channels.release"}

// Channels are the «ТВ» titles outside the library (*online.ChannelExtras).
type Channels interface {
	Ready(ctx context.Context) bool
	Candidates(ctx context.Context, r addonkit.ChannelRules, limit int) ([]addonkit.ChannelTitle, error)
	Resolve(ctx context.Context, ch int64, t addonkit.ChannelTitle) (online.Stream, error)
	// Release ends the stream of a path Resolve gave (false: not open).
	Release(path string) bool
}

// Deps are the hooks' parts.
type Deps struct {
	// Progress is the online watch progress (boot.Booted.Progress): its
	// snapshot answers home.continue, SeedLibrary runs after a scan.
	Progress *online.ProgressStore
	// DB is the addon's database (the watch states, the clean-up).
	DB *sql.DB
	// Users is the server's profiles (GET /users): a tracking.watched
	// userId's uid, and which uids are still profiles.
	Users online.UserLister
	// Available: «Смотреть онлайн» is on and Lampac configured (nil:
	// yes); off, «Продолжить» has no cards (their player would not play).
	Available func() bool
	// Scanner is the tracking scanner of kind online (nil: no Lampac; a
	// scan keeps the stored rows).
	Scanner online.Scanner
	// Channels (nil: no Lampac or TMDB) are the «ТВ» titles outside the
	// library.
	Channels Channels
	Log      *slog.Logger
}

// Register answers the hooks (Names) on m.
func Register(m *addonkit.HookMux, d Deps) {
	h := &hooks{d: d}
	m.Handle("library.afterScan", h.afterScan)
	m.Handle("home.continue", h.homeContinue)
	m.Handle("tracking.scan:online", h.scan)
	m.Handle("tracking.watched", h.watched)
	m.Handle("channels.ready", h.channelsReady)
	m.Handle("channels.candidates", h.channelsCandidates)
	m.Handle("channels.resolve", h.channelsResolve)
	m.Handle("channels.release", h.channelsRelease)
}

type hooks struct {
	d Deps
	// seeding: one library.afterScan at a time; an overlapping call sets
	// again, and the running one makes one more pass after its own (the
	// seed is idempotent)
	seeding sync.Mutex
	again   atomic.Bool
}

type profile struct {
	uid string
	kid bool
}

func (h *hooks) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

// errBadBody: a hook body that is not the hook's shape (a failure: the
// core logs it and does without the answer).
var errBadBody = errors.New("online hooks: a body not of the hook's shape")

// afterScan: library.afterScan, {} → 204. The online positions of titles
// the library has now are seeded into its progress (online.SeedLibrary:
// each at most once; a row the core refuses is counted, marked and not
// sent again), then the rows of every uid GET /users no longer lists (a
// deleted profile) go; a /users error skips that until the next scan.
func (h *hooks) afterScan(ctx context.Context, _ *addonkit.User, _ json.RawMessage) (any, error) {
	h.again.Store(true)
	if !h.seeding.TryLock() {
		return nil, nil // the running one makes another pass for it
	}
	defer h.seeding.Unlock()
	var seedErr error
	for h.again.Swap(false) && ctx.Err() == nil {
		seedErr = nil
		if h.d.Progress != nil {
			seedErr = h.d.Progress.SeedLibrary(ctx)
		}
		h.dropDeleted(ctx)
	}
	if seedErr != nil {
		return nil, seedErr
	}
	return nil, nil
}

// dropDeleted deletes the rows of the uids GET /users no longer lists
// (store.DropProfilesExcept: an empty list deletes nothing).
func (h *hooks) dropDeleted(ctx context.Context) {
	if h.d.Users == nil || h.d.DB == nil {
		return
	}
	us, err := h.d.Users.Users(ctx)
	if err != nil {
		h.log().Warn("online: profiles not read, deleted profiles' rows kept until the next scan", "err", err)
		return
	}
	uids := make([]string, 0, len(us))
	for _, u := range us {
		uids = append(uids, u.UID)
	}
	n, err := store.DropProfilesExcept(ctx, h.d.DB, uids)
	if err != nil {
		h.log().Warn("online: deleted profiles' rows not dropped", "err", err)
		return
	}
	if n > 0 {
		h.log().Info("online: rows of deleted profiles dropped", "rows", n)
	}
}

// continueCards is how many cards home.continue gives (the core looks at
// an addon's 40 newest).
const continueCards = 40

// homeContinue: home.continue, {} → {cards}: the titles the header's
// profile was watching online, newest first, from the stored snapshot
// alone (no Lampac or TMDB call fits the core's 500 ms). A title saved
// without a snapshot is left out; each card is gated by the header's
// maxRating (an unrated title is left out for a limited profile). None
// for a kid or without a profile, and none while «Смотреть онлайн» is
// off. The card opens the addon's player where the profile stopped.
func (h *hooks) homeContinue(ctx context.Context, u *addonkit.User, _ json.RawMessage) (any, error) {
	out := addonkit.ContinueAnswer{Cards: []addonkit.ContinueCard{}}
	if u == nil || u.IsKid || u.UID == "" || h.d.Progress == nil || (h.d.Available != nil && !h.d.Available()) {
		return out, nil
	}
	es, err := h.d.Progress.Continue(ctx, *u, continueCards)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if e.Title == "" || online.Allowed(*u, e.Rating) != nil {
			continue
		}
		out.Cards = append(out.Cards, card(u.Lang, e))
	}
	return out, nil
}

// card is the «Продолжить» card of an entry.
func card(lang string, e online.Entry) addonkit.ContinueCard {
	c := addonkit.ContinueCard{Key: e.Kind + ":" + strconv.Itoa(e.TMDBID), Kind: e.Kind, TMDBID: int64(e.TMDBID),
		Title: e.Title, Year: e.Year, Poster: e.Poster, Rating: e.Rating, ResumeSec: float64(e.PositionMs) / 1000,
		UpdatedAt: e.UpdatedAt}
	if e.DurationMs > 0 {
		c.Progress = min(float64(e.PositionMs)/float64(e.DurationMs), 1)
	}
	q := url.Values{}
	if e.Kind == "series" {
		c.Season, c.Episode = e.Season, e.Episode
		c.Subtitle = episodeLabel(lang, e.Season, e.Episode)
		q.Set("s", strconv.Itoa(e.Season))
		q.Set("e", strconv.Itoa(e.Episode))
	}
	if t := e.PositionMs / 1000; t > 0 {
		q.Set("t", strconv.FormatInt(t, 10))
	}
	c.Href = "/addons/online/play/" + e.Kind + "/" + strconv.Itoa(e.TMDBID)
	if len(q) > 0 {
		c.Href += "?" + encodeOrdered(q, "s", "e", "t")
	}
	return c
}

// encodeOrdered is q's query in the order of keys (url.Values sorts them).
func encodeOrdered(q url.Values, keys ...string) string {
	s := ""
	for _, k := range keys {
		if v := q.Get(k); v != "" {
			if s != "" {
				s += "&"
			}
			s += k + "=" + url.QueryEscape(v)
		}
	}
	return s
}

// episodeLabel is the core's «Сезон 1, серия 2» (season 0: a special).
func episodeLabel(lang string, season, ep int) string {
	if season == 0 {
		return i18n.TL(lang, "Спецвыпуск %d", ep)
	}
	return i18n.TL(lang, "Сезон %d, серия %d", season, ep)
}

// errBadScan: a scan body without its title (a failure: the core keeps
// the rows).
var errBadScan = errors.New("online hooks: a scan request without its title")

// sourcesAnswer is a scan's answer with sources: the list always present,
// so none is {"sources": []} (the kind's rows go), never a bare {}.
type sourcesAnswer struct {
	Sources []addonkit.TrackingSource `json:"sources"`
}

// scan: tracking.scan:online, {title, episodes} → the title's sources of
// kind online, or {unchanged: true} when the scanner did not look (its
// cadence) or there is none (no Lampac); an error is a failure (the core
// keeps the rows).
func (h *hooks) scan(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var req addonkit.ScanRequest
	if err := json.Unmarshal(in, &req); err != nil || req.Title.ID <= 0 {
		return nil, errBadScan
	}
	if h.d.Scanner == nil {
		return addonkit.ScanAnswer{Unchanged: true}, nil
	}
	srcs, err := h.d.Scanner.Scan(ctx, coreadapt.ToTitle(req.Title), coreadapt.ToEpisodes(req.Episodes))
	if errors.Is(err, online.ErrUnchanged) {
		return addonkit.ScanAnswer{Unchanged: true}, nil
	}
	if err != nil {
		return nil, err
	}
	out := sourcesAnswer{Sources: make([]addonkit.TrackingSource, len(srcs))}
	for i, s := range srcs {
		out.Sources[i] = coreadapt.FromSource(s)
	}
	return out, nil
}

// watched: tracking.watched, {userId, kind, tmdbId} → {episodes}: every
// episode of the title (a movie: 0, 0) the profile finished online
// (watched) or stopped in (positionSec). 204 for a profile GET /users does
// not list, or a kid's (online is never a kid's).
func (h *hooks) watched(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var req addonkit.WatchedRequest
	if err := json.Unmarshal(in, &req); err != nil || req.UserID <= 0 || req.TMDBID <= 0 ||
		(req.Kind != "movie" && req.Kind != "series") {
		return nil, errBadBody
	}
	p, ok, err := h.profileOf(ctx, req.UserID)
	if err != nil {
		return nil, err
	}
	if !ok || p.kid || h.d.DB == nil {
		return nil, nil
	}
	fin, err := online.WatchedFunc(h.d.DB)(ctx, p.uid, tracking.Title{Kind: req.Kind, TMDBID: req.TMDBID})
	if err != nil {
		return nil, err
	}
	states, err := online.WatchStateFunc(h.d.DB)(ctx, p.uid, req.Kind, req.TMDBID)
	if err != nil {
		return nil, err
	}
	for k := range fin {
		st := states[k]
		st.Watched, st.PositionSec = true, 0
		states[k] = st
	}
	keys := make([][2]int, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]int) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})
	out := addonkit.WatchedAnswer{Episodes: make([]addonkit.WatchedEpisode, 0, len(keys))}
	for _, k := range keys {
		st := states[k]
		out.Episodes = append(out.Episodes, addonkit.WatchedEpisode{Season: k[0], Number: k[1], Watched: st.Watched, PositionSec: st.PositionSec})
	}
	return out, nil
}

// profileOf is profile id's uid, read from GET /users on every call (a
// deleted profile's id may be given to a new one: a mapping kept across
// calls could hand the new profile the old one's states); ok false when
// the server does not list it.
func (h *hooks) profileOf(ctx context.Context, id int64) (profile, bool, error) {
	if h.d.Users == nil {
		return profile{}, false, nil
	}
	us, err := h.d.Users.Users(ctx)
	if err != nil {
		return profile{}, false, err
	}
	for _, u := range us {
		if u.ID == id && u.UID != "" {
			return profile{uid: u.UID, kid: u.IsKid}, true, nil
		}
	}
	return profile{}, false, nil
}

// errNoChannels: channels.resolve while the addon has no channel extras
// (a failure: the core leaves the title out for a day).
var errNoChannels = errors.New("online hooks: no titles outside the library (no Lampac or TMDB)")

// channelsReady: channels.ready, {} → {ready}.
func (h *hooks) channelsReady(ctx context.Context, _ *addonkit.User, _ json.RawMessage) (any, error) {
	return addonkit.ChannelReady{Ready: h.d.Channels != nil && h.d.Channels.Ready(ctx)}, nil
}

// channelsCandidates: channels.candidates, {rules, limit} → {titles}.
func (h *hooks) channelsCandidates(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var req addonkit.ChannelCandidates
	if err := json.Unmarshal(in, &req); err != nil {
		return nil, errBadBody
	}
	out := addonkit.ChannelCandidatesAnswer{Titles: []addonkit.ChannelTitle{}}
	if h.d.Channels == nil {
		return out, nil
	}
	ts, err := h.d.Channels.Candidates(ctx, req.Rules, req.Limit)
	if err != nil {
		return nil, err
	}
	if ts != nil {
		out.Titles = ts
	}
	return out, nil
}

// channelsResolve: channels.resolve, {channelId, title} → {path}: the
// stream's route on this socket (ChannelPrefix<token>/…), which the core's
// relay reads; no headers (the route needs none).
func (h *hooks) channelsResolve(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var req addonkit.ChannelResolve
	if err := json.Unmarshal(in, &req); err != nil || req.ChannelID <= 0 {
		return nil, errBadBody
	}
	if h.d.Channels == nil {
		return nil, errNoChannels
	}
	st, err := h.d.Channels.Resolve(ctx, req.ChannelID, req.Title)
	if err != nil {
		return nil, err
	}
	return addonkit.ChannelStream{Path: st.Path}, nil
}

// channelsRelease: channels.release, {token, path} → 204: the stream of
// path ends (its proxy session closes); one not open is no error.
func (h *hooks) channelsRelease(_ context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var req addonkit.ChannelRelease
	if err := json.Unmarshal(in, &req); err != nil {
		return nil, errBadBody
	}
	if h.d.Channels != nil && req.Path != "" {
		h.d.Channels.Release(req.Path)
	}
	return nil, nil
}
