package online

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
)

// Online scanner defaults.
const (
	defaultMaxVoices = 6
	browsePause      = 2 * time.Second
	onlineCadence    = 4 * time.Hour
)

// onlineAPI is what the online scanner asks of Lampac (*Service).
type onlineAPI interface {
	Sources(ctx context.Context, u User, kind string, tmdbID int) ([]Source, error)
	Browse(ctx context.Context, u User, ref string) (Level, error)
}

// SourceStore is where a scanner reads its kind's previous rows (the
// core's tracked titles: coreadapt.Tracking).
type SourceStore interface {
	Sources(ctx context.Context, titleID int64) ([]tracking.Source, error)
}

// ErrUnchanged: the scanner did not look this time; the stored rows stay
// (the tracking.scan hook answers {unchanged: true}).
var ErrUnchanged = errors.New("tracking: unchanged")

// Scanner is a tracking scanner of one kind (the core's
// tracking.Scanner, answered through the tracking.scan:<kind> hook).
type Scanner interface {
	Kind() string
	Scan(ctx context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error)
}

// onlineScanner is the tracking scanner of kind "online".
type onlineScanner struct {
	api       onlineAPI
	store     SourceStore // nil: no cadence, nothing kept
	as        User
	maxVoices int
	pause     time.Duration // between Browse calls
	now       func() time.Time
	log       *slog.Logger

	mu   sync.Mutex
	last map[int64]time.Time // when each title was scanned (this process)
}

// onlineWalk is one scan's walk (scans may run at once).
type onlineWalk struct {
	*onlineScanner
	browsed bool         // a Browse call was made (the next one waits)
	seasons map[int]bool // the seasons the balancer at hand gave (a film: 0)
}

// NewTrackingScanner is the tracking scanner of Lampac's balancers: for each
// source of the title it browses the latest season's episodes (a film: its
// files) in up to maxVoices voice-overs (0 = 6), 2 s apart, as the user as
// (an adult non-kid identity); one Source per (episode, balancer, voice):
// Ref the episode's (file's) ref, Provider the balancer's name, Labels the
// voice, Quality its best quality. Unsupported entries are left out. A
// balancer's previous rows (st) of the seasons it did not give this time,
// or all of them when it failed, are kept. A title is scanned again after
// 4 h, or on every refresh while an episode aired in the last 2 days.
func NewTrackingScanner(o *Service, st SourceStore, as User, maxVoices int) Scanner {
	if maxVoices <= 0 {
		maxVoices = defaultMaxVoices
	}
	sc := &onlineScanner{as: as, maxVoices: maxVoices, pause: browsePause, now: time.Now,
		log: slog.Default().With("component", "tracking-online"), last: map[int64]time.Time{}}
	if o != nil {
		sc.api = o
	}
	if st != nil {
		sc.store = st
	}
	return sc
}

func (*onlineScanner) Kind() string { return "online" }

// Scan: online off gives no sources; a source that fails keeps its
// previous rows, and when every source fails the scan fails (Refresh
// keeps the old rows). ErrUnchanged: scanned under 4 h ago and no
// episode aired in the last 2 days.
func (c *onlineScanner) Scan(ctx context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error) {
	if t.TMDBID <= 0 || c.api == nil {
		return nil, nil
	}
	now := c.now()
	var prev []tracking.Source
	if c.store != nil {
		all, err := c.store.Sources(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		prev = ofKind(all, "online")
	}
	if !airedLately(eps, now) && c.scannedSince(t.ID, prev, now.Add(-onlineCadence)) {
		return nil, ErrUnchanged
	}
	srcs, err := c.api.Sources(ctx, c.as, t.Kind, int(t.TMDBID))
	if errors.Is(err, ErrOff) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w := &onlineWalk{onlineScanner: c}
	var out []tracking.Source
	var errs []error
	for _, src := range srcs {
		w.seasons = map[int]bool{}
		got, err := w.source(ctx, t, src)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			c.log.Debug("tracking: online source", "title", t.ID, "source", src.ID, "err", err)
			errs = append(errs, err)
		}
		out = append(out, got...)
		failed, given := err != nil, w.seasons
		out = keepPrev(out, prev, func(x tracking.Source) bool {
			return x.Provider == src.Name && (failed || !given[x.Season])
		})
	}
	if len(errs) > 0 && len(errs) == len(srcs) {
		return nil, errors.Join(errs...)
	}
	c.mu.Lock()
	c.last[t.ID] = now
	c.mu.Unlock()
	return out, nil
}

// scannedSince: the title was scanned after since (by this process, or by
// the last_seen of its rows).
func (c *onlineScanner) scannedSince(titleID int64, prev []tracking.Source, since time.Time) bool {
	c.mu.Lock()
	at, ok := c.last[titleID]
	c.mu.Unlock()
	if ok && at.After(since) {
		return true
	}
	for _, x := range prev {
		if x.LastSeen.After(since) {
			return true
		}
	}
	return false
}

// airedLately: an episode aired today or in the 2 days before.
func airedLately(eps []tracking.Episode, now time.Time) bool {
	today, from := now.Format(time.DateOnly), now.AddDate(0, 0, -2).Format(time.DateOnly)
	for _, e := range eps {
		if e.AirDate != "" && e.AirDate >= from && e.AirDate <= today {
			return true
		}
	}
	return false
}

func ofKind(all []tracking.Source, kind string) []tracking.Source {
	var out []tracking.Source
	for _, x := range all {
		if x.Kind == kind {
			out = append(out, x)
		}
	}
	return out
}

// keepPrev adds to out the previous rows keep chooses that out lacks.
func keepPrev(out, prev []tracking.Source, keep func(tracking.Source) bool) []tracking.Source {
	type key struct {
		s, e int
		ref  string
	}
	have := map[key]bool{}
	for _, x := range out {
		have[key{x.Season, x.Episode, x.Ref}] = true
	}
	for _, x := range prev {
		k := key{x.Season, x.Episode, x.Ref}
		if keep(x) && !have[k] {
			have[k] = true
			out = append(out, x)
		}
	}
	return out
}

// browse is Browse after the pause (none before a scan's first call).
func (c *onlineWalk) browse(ctx context.Context, ref string) (Level, error) {
	if c.browsed && c.pause > 0 {
		tm := time.NewTimer(c.pause)
		select {
		case <-ctx.Done():
			tm.Stop()
			return Level{}, ctx.Err()
		case <-tm.C:
		}
	}
	c.browsed = true
	return c.api.Browse(ctx, c.as, ref)
}

// source walks one balancer; err: its first level failed (a later
// voice's failure only leaves that voice out).
func (c *onlineWalk) source(ctx context.Context, t tracking.Title, src Source) ([]tracking.Source, error) {
	lv, err := c.browse(ctx, src.Ref)
	if err != nil {
		return nil, err
	}
	var out []tracking.Source
	c.descend(ctx, t, src, lv, "", false, &out)
	return out, nil
}

// descend follows a level down to its episodes (the latest season) or
// files. The first level listing voices is opened in each voice (the
// active one first, up to maxVoices); below it the voice is kept.
func (c *onlineWalk) descend(ctx context.Context, t tracking.Title, src Source, lv Level, voice string, voiced bool, out *[]tracking.Source) {
	if ctx.Err() != nil {
		return
	}
	if !voiced && len(lv.Voices) > 0 {
		for i, v := range activeFirst(lv.Voices) {
			if i >= c.maxVoices {
				break
			}
			next := lv
			if !v.Active {
				var err error
				if next, err = c.browse(ctx, v.Ref); err != nil {
					continue
				}
			}
			c.descend(ctx, t, src, next, v.Name, true, out)
		}
		return
	}
	switch lv.Level {
	case "seasons":
		var latest *Season
		for i, s := range lv.Seasons {
			if latest == nil || s.Number > latest.Number {
				latest = &lv.Seasons[i]
			}
		}
		if latest == nil {
			return
		}
		next, err := c.browse(ctx, latest.Ref)
		if err != nil {
			return
		}
		c.descend(ctx, t, src, next, voice, voiced, out)
	case "episodes":
		c.seasons[lv.Season] = true
		for _, e := range lv.Episodes {
			c.seasons[e.Season] = true
			if e.Unsupported || e.Episode <= 0 {
				continue
			}
			*out = append(*out, onlineSource(t, src, e.Season, e.Episode, e.Ref, e.MaxQuality, voice))
		}
	case "movie":
		c.seasons[0] = true
		// one file per voice: a balancer may list each voice-over as a file
		seen := map[string]bool{}
		n := 0
		for _, f := range lv.Files {
			v := voice
			if v == "" {
				v = f.Voice
			}
			if f.Unsupported || seen[v] {
				continue
			}
			if n >= c.maxVoices {
				break
			}
			seen[v] = true
			n++
			*out = append(*out, onlineSource(t, src, 0, 0, f.Ref, f.MaxQuality, v))
		}
	}
	// «pick» (several titles of that name): not followed, no year to tell
}

func onlineSource(t tracking.Title, src Source, season, ep int, ref, quality, voice string) tracking.Source {
	var labels []string
	if voice != "" {
		labels = []string{voice}
	}
	lang, subs := voiceLang(voice)
	return tracking.Source{TitleID: t.ID, Season: season, Episode: ep, Kind: "online", Ref: ref,
		Provider: src.Name, Quality: quality, Labels: labels,
		Meta: tracking.SourceMeta{Provider: src.ID, Voice: voice, Lang: lang, Subs: subs}}
}

// voiceLang is what a Lampac voice name says of its audio: Russian unless it
// names the original track ("" = unknown), English («ENG», «English») or
// subtitles over the original (no audio language, subtitles "ru").
func voiceLang(voice string) (lang string, subs []string) {
	v := strings.ToLower(voice)
	switch {
	case strings.Contains(v, "субтитры") || slices.Contains(tokens(v), "sub"):
		return "", []string{"ru"}
	case strings.Contains(v, "оригинал") || strings.Contains(v, "original"):
		return "", nil
	case strings.Contains(v, "english") || slices.Contains(tokens(v), "eng"):
		return "en", nil
	}
	return "ru", nil
}

// tokens splits s into its letter and digit runs.
func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// activeFirst: the active voice (its level is the one at hand), then the
// others in the source's order.
func activeFirst(vs []Choice) []Choice {
	out := make([]Choice, 0, len(vs))
	for _, v := range vs {
		if v.Active {
			out = append(out, v)
		}
	}
	for _, v := range vs {
		if !v.Active {
			out = append(out, v)
		}
	}
	return out
}
