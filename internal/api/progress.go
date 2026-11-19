package api

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// progressRequest is the body of PUT /api/progress.
type progressRequest struct {
	Kind       string `json:"kind"`
	TMDBID     int    `json:"tmdbId"`
	Season     int    `json:"season"`
	Episode    int    `json:"episode"`
	PositionMs int64  `json:"positionMs"`
	DurationMs int64  `json:"durationMs"`
	Source     string `json:"source"`
	Voice      string `json:"voice"`
	Quality    string `json:"quality"`
	// NextSeason/NextEpisode (series): the episode the source lists after
	// this one (0: unknown); LastListed: it lists none after it.
	NextSeason  int  `json:"nextSeason"`
	NextEpisode int  `json:"nextEpisode"`
	LastListed  bool `json:"lastListed"`
	// UpdatedAt (RFC 3339): when the client reached the position (a save
	// queued while the server could not take it); older than the stored
	// entry → ignored.
	UpdatedAt *time.Time `json:"updatedAt"`
}

// saveProgress: PUT /api/progress — the online player's position (204),
// behind the kid and rating gate of the title. The store mirrors it into
// the library's progress when the library has that movie or episode
// (SaveProgress of the core).
func (a *API) saveProgress(w http.ResponseWriter, r *http.Request) {
	if a.d.Progress == nil {
		http.NotFound(w, r)
		return
	}
	var in progressRequest
	if !decode(w, r, &in) {
		return
	}
	// the request's history: a separate one (a shared TV) keeps its own
	// positions under its uid; the limits stay the profile's
	u := userOf(r).HistoryUser()
	t, err := a.d.Online.Gate(r.Context(), u, in.Kind, in.TMDBID)
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	var at time.Time
	if in.UpdatedAt != nil {
		at = *in.UpdatedAt
	}
	// the title as of now goes with the entry: «Продолжить» reads no TMDB
	err = a.d.Progress.Save(r.Context(), u, online.Entry{Kind: in.Kind, TMDBID: in.TMDBID, Season: in.Season, Episode: in.Episode,
		PositionMs: in.PositionMs, DurationMs: in.DurationMs, Source: in.Source, Voice: in.Voice, Quality: in.Quality,
		Title: t.Title, Rating: t.Rating, Year: t.Year, Poster: a.posters.name(t.Poster, in.Kind, in.TMDBID),
		NextSeason: in.NextSeason, NextEpisode: in.NextEpisode, LastListed: in.LastListed, At: at})
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getProgress: GET /api/progress?kind=movie|series&tmdbId=N — a title's
// entries of the profile, newest first, and the last choices (null: none).
func (a *API) getProgress(w http.ResponseWriter, r *http.Request) {
	if a.d.Progress == nil {
		http.NotFound(w, r)
		return
	}
	kind, id, ok := titleOf(w, r)
	if !ok {
		return
	}
	u := userOf(r).HistoryUser()
	if _, err := a.d.Online.Gate(r.Context(), u, kind, id); err != nil {
		onlineErr(w, r, err)
		return
	}
	entries, last, err := a.d.Progress.Get(r.Context(), u, kind, id)
	if err != nil {
		onlineErr(w, r, err)
		return
	}
	out := map[string]any{"entries": entries, "last": nil}
	if len(entries) == 0 {
		out["entries"] = []online.Entry{}
	} else {
		out["last"] = last
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}

// posters puts the TMDB posters of titles watched online into the core's
// image cache (POST /images), in the background of a progress save, so
// «Продолжить» shows them without a download.
type posters struct {
	cache func(ctx context.Context, url string) (string, error)
	// done records the cache file of a title's poster on its entries.
	done func(ctx context.Context, kind string, id int, name string) error
	now  func() time.Time

	mu    sync.Mutex
	known map[string]string    // poster URL → cache file
	busy  map[string]bool      // fetching
	miss  map[string]time.Time // failed at
	wg    sync.WaitGroup
}

const (
	posterRetry   = 10 * time.Minute
	posterTimeout = 30 * time.Second
	maxPosters    = 5000
)

func newPosters(cache func(ctx context.Context, url string) (string, error),
	done func(ctx context.Context, kind string, id int, name string) error) *posters {
	return &posters{cache: cache, done: done, now: time.Now,
		known: map[string]string{}, busy: map[string]bool{}, miss: map[string]time.Time{}}
}

// name is the cache file of a poster already fetched (else ""); it starts
// one fetch in the background, unless one runs or failed lately.
func (p *posters) name(url, kind string, id int) string {
	if p == nil || url == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n, ok := p.known[url]; ok {
		return n
	}
	if p.busy[url] || (!p.miss[url].IsZero() && p.now().Sub(p.miss[url]) < posterRetry) {
		return ""
	}
	if len(p.miss) > maxPosters {
		p.miss = map[string]time.Time{}
	}
	p.busy[url] = true
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), posterTimeout)
		defer cancel()
		n, err := p.cache(ctx, url)
		if err == nil {
			err = p.done(ctx, kind, id, n)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.busy, url)
		if err != nil {
			log.Printf("online: poster %s %d: %v", kind, id, err)
			p.miss[url] = p.now()
			return
		}
		if len(p.known) > maxPosters {
			p.known = map[string]string{}
		}
		p.known[url] = n
		delete(p.miss, url)
	}()
	return ""
}
