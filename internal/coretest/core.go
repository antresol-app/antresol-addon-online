// Package coretest is an in-memory core for the online addon's tests (the
// pattern of the other first-party addons'): the library's movies and series
// episodes by TMDB id, the profiles' watch progress with the core's rules
// (an older position is ignored), the profiles, and the tracked titles'
// sources. Its methods are those of *addonkit.Client the addon calls, so
// a test sets the core up as data and reads back what the addon wrote.
package coretest

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
	"github.com/antresol-app/antresol/addonkit"
)

// Core is the fake core. Profiles answers Users (a kid profile's
// progress and episode calls are refused with 400, an unlisted id's too,
// as the core's); Hidden are the item ids every profile is refused (over
// its rating limit: 404 on SaveProgress, left out of Progress and
// EpisodeItems); Saves records every SaveProgress call the core took;
// Fail, when set, is the error of every library and progress call. Use a
// *Core; the zero value is ready.
type Core struct {
	Profiles []addonkit.User
	Hidden   map[int64]bool
	// NotVideo are the item ids SaveProgress refuses with 400 (the core's
	// «not a movie or an episode»).
	NotVideo map[int64]bool
	Saves    []addonkit.ProgressSave
	Fail     error
	// UsersFail, when set, is the error of Users.
	UsersFail error
	// Now is the core's clock for a save without At (nil: time.Now).
	Now func() time.Time

	mu       sync.Mutex
	next     int64
	movies   map[int64]int64                     // TMDB id → item id
	episodes map[int64]map[[2]int]int64          // series TMDB id → {season, episode} → item id
	progress map[[2]int64]addonkit.ProgressEntry // {profile, item}
	sources  map[int64][]tracking.Source         // tracked title id → its sources
}

func (c *Core) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// profileErr is the core's 400 for a kid or unknown profile (with no
// Profiles set, every id is an adult's).
func (c *Core) profileErr(userID int64) error {
	if len(c.Profiles) == 0 {
		return nil
	}
	// a profile's separate history is taken as the profile, as the core does
	for _, u := range addonkit.HistoryProfiles(c.Profiles) {
		if u.ID == userID {
			if u.IsKid {
				return &addonkit.APIError{Status: http.StatusBadRequest, Message: "kid profile"}
			}
			return nil
		}
	}
	return &addonkit.APIError{Status: http.StatusBadRequest, Message: "no such profile"}
}

func (c *Core) id() int64 {
	c.next++
	return c.next
}

// AddMovie puts a movie with TMDB id tmdbID in the library; its item id.
func (c *Core) AddMovie(tmdbID int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.movies == nil {
		c.movies = map[int64]int64{}
	}
	if id, ok := c.movies[tmdbID]; ok {
		return id
	}
	id := c.id()
	c.movies[tmdbID] = id
	return id
}

// AddEpisode puts episode e of season s of the series with TMDB id tmdbID
// in the library (the series too); the episode's item id.
func (c *Core) AddEpisode(tmdbID int64, s, e int) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.episodes == nil {
		c.episodes = map[int64]map[[2]int]int64{}
	}
	m := c.episodes[tmdbID]
	if m == nil {
		m = map[[2]int]int64{{-1, -1}: c.id()} // the series item
		c.episodes[tmdbID] = m
	}
	if id, ok := m[[2]int{s, e}]; ok {
		return id
	}
	id := c.id()
	m[[2]int{s, e}] = id
	return id
}

// ItemByTMDB is GET /library/tmdb/{kind}/{tmdbId}.
func (c *Core) ItemByTMDB(_ context.Context, kind string, tmdbID int64) (addonkit.Item, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return addonkit.Item{}, false, c.Fail
	}
	switch kind {
	case "movie":
		if id, ok := c.movies[tmdbID]; ok {
			return addonkit.Item{ID: id, Kind: "movie", TMDBID: tmdbID}, true, nil
		}
	case "series":
		if m, ok := c.episodes[tmdbID]; ok {
			return addonkit.Item{ID: m[[2]int{-1, -1}], Kind: "series", TMDBID: tmdbID}, true, nil
		}
	}
	return addonkit.Item{}, false, nil
}

// ItemIDsByTMDB is GET /library/tmdb/{kind}?ids=: the ids the library has.
func (c *Core) ItemIDsByTMDB(ctx context.Context, kind string, ids []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	for _, id := range ids {
		it, ok, err := c.ItemByTMDB(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = it.ID
		}
	}
	return out, nil
}

// EpisodeItems is GET /library/series/{tmdbId}/episode-items: the
// episodes not Hidden, by season and episode.
func (c *Core) EpisodeItems(_ context.Context, userID, tmdbID int64) ([]addonkit.EpisodeItem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return nil, c.Fail
	}
	if err := c.profileErr(userID); err != nil {
		return nil, err
	}
	out := []addonkit.EpisodeItem{}
	for k, id := range c.episodes[tmdbID] {
		if k[0] >= 0 && !c.Hidden[id] {
			out = append(out, addonkit.EpisodeItem{Season: k[0], Episode: k[1], ItemID: id})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Season < out[j].Season || out[i].Season == out[j].Season && out[i].Episode < out[j].Episode
	})
	return out, nil
}

// Progress is GET /progress: profile userID's entries of the items (a
// Hidden one left out).
func (c *Core) Progress(_ context.Context, userID int64, itemIDs []int64) (map[int64]addonkit.ProgressEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return nil, c.Fail
	}
	if err := c.profileErr(userID); err != nil {
		return nil, err
	}
	out := map[int64]addonkit.ProgressEntry{}
	for _, id := range itemIDs {
		if p, ok := c.progress[[2]int64{userID, id}]; ok && !c.Hidden[id] {
			out[id] = p
		}
	}
	return out, nil
}

// SaveProgress is PUT /progress: a position older than the stored one is
// ignored; watched from 92% of a known duration; 404 for a Hidden item.
func (c *Core) SaveProgress(_ context.Context, p addonkit.ProgressSave) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return c.Fail
	}
	if err := c.profileErr(p.UserID); err != nil {
		return err
	}
	if c.Hidden[p.ItemID] {
		return &addonkit.APIError{Status: http.StatusNotFound, Message: "no such item"}
	}
	if c.NotVideo[p.ItemID] {
		return &addonkit.APIError{Status: http.StatusBadRequest, Message: "not a movie or an episode"}
	}
	c.Saves = append(c.Saves, p)
	at := p.At
	if at.IsZero() {
		at = c.now()
	}
	c.set(p.UserID, p.ItemID, p.PositionMs, p.DurationMs, at)
	return nil
}

func (c *Core) set(userID, itemID, pos, dur int64, at time.Time) {
	if c.progress == nil {
		c.progress = map[[2]int64]addonkit.ProgressEntry{}
	}
	k := [2]int64{userID, itemID}
	if old, ok := c.progress[k]; ok && old.UpdatedAt.After(at) {
		return
	}
	c.progress[k] = addonkit.ProgressEntry{PositionMs: pos, DurationMs: dur, Watched: dur > 0 && pos*100 >= dur*92, UpdatedAt: at.UTC()}
}

// Play is local playback of item itemID by profile userID at at.
func (c *Core) Play(userID, itemID, pos, dur int64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(userID, itemID, pos, dur, at)
}

// Unwatch is «Не просмотрено»: the profile's progress of the item goes.
func (c *Core) Unwatch(userID, itemID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.progress, [2]int64{userID, itemID})
}

// Get is profile userID's progress of item itemID (ok false: none).
func (c *Core) Get(userID, itemID int64) (addonkit.ProgressEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.progress[[2]int64{userID, itemID}]
	return p, ok
}

// Users is GET /users.
func (c *Core) Users(context.Context) ([]addonkit.User, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.UsersFail != nil {
		return nil, c.UsersFail
	}
	return append([]addonkit.User(nil), c.Profiles...), nil
}

// SetSources sets the sources of tracked title id (any kinds).
func (c *Core) SetSources(id int64, srcs []tracking.Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sources == nil {
		c.sources = map[int64][]tracking.Source{}
	}
	c.sources[id] = srcs
}

// Sources is every source of tracked title id (coreadapt.Tracking's).
func (c *Core) Sources(_ context.Context, id int64) ([]tracking.Source, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tracking.Source(nil), c.sources[id]...), nil
}
