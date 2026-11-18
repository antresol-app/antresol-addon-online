package online

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/library"
	"github.com/antresol-app/antresol/addonkit"
)

// LibraryLookup finds the library item an online title mirrors its
// progress into (*addonkit.Client, permission library.read).
type LibraryLookup interface {
	// ItemByTMDB is the top-level movie or series with a TMDB id (ok
	// false when there is none).
	ItemByTMDB(ctx context.Context, kind string, tmdbID int64) (addonkit.Item, bool, error)
	// EpisodeItems are the library items of the series' episodes profile
	// userID may see (GET /library/series/{tmdbId}/episode-items).
	EpisodeItems(ctx context.Context, userID, tmdbID int64) ([]addonkit.EpisodeItem, error)
}

// LibraryProgress is the library's watch progress of the profiles
// (*addonkit.Client, permission progress).
type LibraryProgress interface {
	// Progress is profile userID's progress of the items (an item without
	// any is left out).
	Progress(ctx context.Context, userID int64, itemIDs []int64) (map[int64]addonkit.ProgressEntry, error)
	// SaveProgress records a position (Played: the time counts as
	// watched in «Статистика»); a position older than the stored one is
	// ignored.
	SaveProgress(ctx context.Context, p addonkit.ProgressSave) error
}

// UserLister lists the server's profiles (*addonkit.Client, GET /users,
// permission users.read).
type UserLister interface {
	Users(ctx context.Context) ([]User, error)
}

// ErrNoProfile: a profile without a uid (the header always carries one;
// the scanner's and the channels' identities have none and keep no
// progress).
var ErrNoProfile = errors.New("Профиль не найден")

// refused: the core refused a progress or library call for this row
// rather than failed: 404 for an item the profile may not see (over its
// rating limit, or gone), 400 for a kid or unknown profile or an item that
// is not a movie or an episode.
func refused(err error) bool {
	var ae *addonkit.APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusBadRequest)
}

// watched is the core's rule (its progress.Watched): finished from 92% of
// a known duration.
func watched(positionMs, durationMs int64) bool {
	return durationMs > 0 && positionMs*100 >= durationMs*92
}

// Entry is the online progress of one movie or one episode of a series.
// Source (a source id), Voice and Quality are the picker's last choices:
// plain hints a client matches against a fresh source list, never refs,
// so a stored entry can only lead to picking again, never to another item.
type Entry struct {
	Kind       string    `json:"kind"` // "movie" | "series"
	TMDBID     int       `json:"tmdbId"`
	Season     int       `json:"season"`  // 0 for a movie
	Episode    int       `json:"episode"` // 0 for a movie
	PositionMs int64     `json:"positionMs"`
	DurationMs int64     `json:"durationMs"`
	Finished   bool      `json:"finished"`
	Source     string    `json:"source"`
	Voice      string    `json:"voice"`
	Quality    string    `json:"quality"`
	UpdatedAt  time.Time `json:"updatedAt"`

	// The TMDB title as of the save (Title "": none was taken), so
	// «Продолжить» needs no TMDB call; Poster is its image cache file.
	Title  string `json:"-"`
	Rating string `json:"-"`
	Year   int    `json:"-"`
	Poster string `json:"-"`
	// NextSeason/NextEpisode: the episode the source listed after this
	// one (0: unknown); LastListed: it listed none after it.
	NextSeason  int  `json:"-"`
	NextEpisode int  `json:"-"`
	LastListed  bool `json:"-"`
	// Next (Continue only): the entry is the episode after a finished one.
	Next bool `json:"-"`
	// InLibrary: this position reached the library's progress (mirrored
	// or seeded); a scan never seeds it again.
	InLibrary bool `json:"-"`
	// At is when the client reached this position (zero: now). A save
	// older than the stored entry is ignored, so a queue replayed later
	// never overwrites a newer position; it is clamped to now.
	At time.Time `json:"-"`
}

// Last is what the picker preselects: the newest entry's choices.
type Last struct {
	Source  string `json:"source"`
	Voice   string `json:"voice"`
	Quality string `json:"quality"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

// ProgressStore keeps online watch progress (online_progress, the addon's
// own database, by profile uid) and mirrors it into the library's
// progress (by profile id) when the library has the item.
type ProgressStore struct {
	DB       *sql.DB
	Lib      LibraryLookup   // nil: no mirror, nothing counts as in the library
	Progress LibraryProgress // nil: no mirror
	// Users gives a stored uid its profile id for SeedLibrary (nil: no
	// seeding); a uid it does not list is skipped.
	Users UserLister
	Now   func() time.Time
}

// Fixed-width so lexicographic order equals chronological order.
const onlineTS = "2006-01-02T15:04:05.000000000Z"

const (
	// maxHint is the longest voice or quality name kept (runes).
	maxHint = 100
	// maxContinueRows bounds the titles Continue looks at.
	maxContinueRows = 200
)

func (p *ProgressStore) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// hint keeps a voice or quality name only when it is a short printable
// name ("": anything else).
func hint(s string) string {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxHint {
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return s
}

// normalize checks an entry's title and clamps the rest; a source that is
// not a source id, and an odd voice or quality, are dropped.
func normalize(e Entry) (Entry, error) {
	if !validKind(e.Kind) || e.TMDBID <= 0 || e.Season < 0 || e.Episode < 0 || e.Season > 10_000 || e.Episode > 100_000 {
		return e, ErrUnknownTitle
	}
	if e.Kind == "movie" {
		e.Season, e.Episode, e.NextSeason, e.NextEpisode, e.LastListed = 0, 0, 0, 0, false
	}
	if e.NextSeason < 0 || e.NextEpisode <= 0 || e.NextSeason > 10_000 || e.NextEpisode > 100_000 || e.LastListed {
		e.NextSeason, e.NextEpisode = 0, 0
	}
	e.PositionMs, e.DurationMs = max(e.PositionMs, 0), max(e.DurationMs, 0)
	e.Finished = watched(e.PositionMs, e.DurationMs)
	if !balanserRe.MatchString(e.Source) {
		e.Source = ""
	}
	e.Voice, e.Quality = hint(e.Voice), hint(e.Quality)
	if !library.ImageCacheName.MatchString(e.Poster) {
		e.Poster = ""
	}
	return e, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Save records the position of a movie or an episode (finished from 92%,
// as the core's progress), with the title snapshot of e (an empty one
// keeps the stored snapshot), for profile u (by its uid). When the
// library has that movie or episode, its library progress gets the same
// position and the online duration (a different cut makes the local
// resume point approximate), and the time played counts in «Статистика»
// (a played save). A failing mirror is logged, not returned: the online
// position is stored and the next save mirrors again.
func (p *ProgressStore) Save(ctx context.Context, u User, e Entry) error {
	if u.UID == "" {
		return ErrNoProfile
	}
	e, err := normalize(e)
	if err != nil {
		return err
	}
	at := p.now()
	if !e.At.IsZero() && e.At.Before(at) {
		at = e.At
	}
	res, err := p.DB.ExecContext(ctx, `INSERT INTO online_progress(user_uid,kind,tmdb_id,season,episode,position_ms,duration_ms,finished,source,voice,quality,updated_at,
	  title,rating,year,poster,next_season,next_episode,last_listed,in_library)
	  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)
	  ON CONFLICT(user_uid,kind,tmdb_id,season,episode) DO UPDATE SET position_ms=excluded.position_ms, duration_ms=excluded.duration_ms,
	  finished=excluded.finished, source=excluded.source, voice=excluded.voice, quality=excluded.quality, updated_at=excluded.updated_at,
	  title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE online_progress.title END,
	  rating=CASE WHEN excluded.title<>'' THEN excluded.rating ELSE online_progress.rating END,
	  year=CASE WHEN excluded.title<>'' THEN excluded.year ELSE online_progress.year END,
	  poster=CASE WHEN excluded.poster<>'' THEN excluded.poster ELSE online_progress.poster END,
	  next_season=excluded.next_season, next_episode=excluded.next_episode, last_listed=excluded.last_listed, in_library=0
	  WHERE excluded.updated_at >= online_progress.updated_at`,
		u.UID, e.Kind, e.TMDBID, e.Season, e.Episode, e.PositionMs, e.DurationMs, b2i(e.Finished), e.Source, e.Voice, e.Quality,
		at.UTC().Format(onlineTS), e.Title, e.Rating, e.Year, e.Poster, e.NextSeason, e.NextEpisode, b2i(e.LastListed))
	if err != nil {
		return fmt.Errorf("online progress: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil // a stale save: the stored entry is newer
	}
	if p.Lib == nil || p.Progress == nil {
		return nil
	}
	id, ok, err := p.libraryItem(ctx, u.ID, e)
	if err == nil && ok {
		err = p.Progress.SaveProgress(ctx, addonkit.ProgressSave{UserID: u.ID, ItemID: id, PositionMs: e.PositionMs,
			DurationMs: e.DurationMs, At: at, Played: true})
		if err == nil {
			err = p.markInLibrary(ctx, u.UID, e, at)
		}
	}
	if refused(err) {
		err = nil // the profile may not see that item: as if the library had none
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("online progress: mirror %s %d: %v", e.Kind, e.TMDBID, err)
	}
	return nil
}

// markInLibrary records that the entry's position as of at reached the
// library (a newer save since keeps its own flag).
func (p *ProgressStore) markInLibrary(ctx context.Context, uid string, e Entry, at time.Time) error {
	_, err := p.DB.ExecContext(ctx, `UPDATE online_progress SET in_library=1
	  WHERE user_uid=? AND kind=? AND tmdb_id=? AND season=? AND episode=? AND updated_at=?`,
		uid, e.Kind, e.TMDBID, e.Season, e.Episode, at.UTC().Format(onlineTS))
	return err
}

// SetPoster records the poster's image cache file on every entry of a
// title (fetched after the save that named it).
func (p *ProgressStore) SetPoster(ctx context.Context, kind string, tmdbID int, name string) error {
	if !library.ImageCacheName.MatchString(name) {
		return nil
	}
	_, err := p.DB.ExecContext(ctx, `UPDATE online_progress SET poster=? WHERE kind=? AND tmdb_id=?`, name, kind, tmdbID)
	return err
}

// libraryItem is the library movie, or the episode (by season and
// number) of the library series profile userID may see, with the entry's
// TMDB id.
func (p *ProgressStore) libraryItem(ctx context.Context, userID int64, e Entry) (int64, bool, error) {
	if e.Kind == "movie" {
		it, ok, err := p.Lib.ItemByTMDB(ctx, e.Kind, int64(e.TMDBID))
		if err != nil {
			return 0, false, fmt.Errorf("library: %w", err)
		}
		return it.ID, ok, nil
	}
	eps, err := p.episodes(ctx, userID, e.TMDBID, e.Season)
	if err != nil {
		return 0, false, err
	}
	id, ok := eps[[2]int{e.Season, e.Episode}]
	return id, ok, nil
}

// episodes maps {season, episode} to the library episodes profile userID
// may see of the series with a TMDB id (season >= 0: that season only;
// -1: all).
func (p *ProgressStore) episodes(ctx context.Context, userID int64, tmdbID, season int) (map[[2]int]int64, error) {
	out := map[[2]int]int64{}
	eps, err := p.Lib.EpisodeItems(ctx, userID, int64(tmdbID))
	if addonkit.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("library episodes: %w", err)
	}
	for _, ep := range eps {
		if season >= 0 && ep.Season != season {
			continue
		}
		if k := [2]int{ep.Season, ep.Episode}; ep.ItemID > 0 {
			if _, dup := out[k]; !dup {
				out[k] = ep.ItemID
			}
		}
	}
	return out, nil
}

// local is profile userID's library progress of item id (ok false: none).
func (p *ProgressStore) local(ctx context.Context, userID, id int64) (addonkit.ProgressEntry, bool, error) {
	m, err := p.Progress.Progress(ctx, userID, []int64{id})
	if err != nil {
		return addonkit.ProgressEntry{}, false, err
	}
	l, ok := m[id]
	return l, ok, nil
}

const selectOnline = `SELECT kind, tmdb_id, season, episode, position_ms, duration_ms, finished, source, voice, quality, updated_at,
  title, rating, year, poster, next_season, next_episode, last_listed, in_library FROM online_progress o `

func scanOnline(rows *sql.Rows) (Entry, error) {
	var e Entry
	var fin, last, inLib int
	var ts string
	if err := rows.Scan(&e.Kind, &e.TMDBID, &e.Season, &e.Episode, &e.PositionMs, &e.DurationMs, &fin, &e.Source, &e.Voice, &e.Quality, &ts,
		&e.Title, &e.Rating, &e.Year, &e.Poster, &e.NextSeason, &e.NextEpisode, &last, &inLib); err != nil {
		return e, err
	}
	e.Finished, e.LastListed, e.InLibrary = fin != 0, last != 0, inLib != 0
	t, err := time.Parse(onlineTS, ts)
	if err != nil {
		return e, fmt.Errorf("online progress: bad timestamp %q: %w", ts, err)
	}
	e.UpdatedAt = t
	return e, nil
}

func (p *ProgressStore) query(ctx context.Context, q string, args ...any) ([]Entry, error) {
	rows, err := p.DB.QueryContext(ctx, selectOnline+q, args...)
	if err != nil {
		return nil, fmt.Errorf("online progress: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanOnline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get lists a title's entries (every episode of a series), newest first,
// and the newest one's choices (zero when there is none). An entry whose
// library item was played locally after it carries the local position.
func (p *ProgressStore) Get(ctx context.Context, u User, kind string, tmdbID int) ([]Entry, Last, error) {
	if u.UID == "" {
		return nil, Last{}, ErrNoProfile
	}
	if !validKind(kind) || tmdbID <= 0 {
		return nil, Last{}, ErrUnknownTitle
	}
	out, err := p.query(ctx, `WHERE user_uid=? AND kind=? AND tmdb_id=? ORDER BY updated_at DESC, rowid DESC`, u.UID, kind, tmdbID)
	if err != nil || len(out) == 0 {
		return out, Last{}, err
	}
	if err := p.withLocal(ctx, u.ID, out); err != nil {
		return nil, Last{}, err
	}
	n := out[0]
	return out, Last{Source: n.Source, Voice: n.Voice, Quality: n.Quality, Season: n.Season, Episode: n.Episode}, nil
}

// withLocal puts newer local playback of the mirrored library items into
// the entries (all of one title).
func (p *ProgressStore) withLocal(ctx context.Context, userID int64, es []Entry) error {
	if p.Lib == nil || p.Progress == nil {
		return nil
	}
	items, err := p.titleItems(ctx, userID, es[0].Kind, es[0].TMDBID)
	if refused(err) {
		return nil // the core refuses this profile's library: no local playback
	}
	if err != nil {
		return err
	}
	for i, e := range es {
		id, ok := items[[2]int{e.Season, e.Episode}]
		if !ok {
			continue
		}
		l, ok, err := p.local(ctx, userID, id)
		if refused(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("online progress: local: %w", err)
		}
		if ok && l.UpdatedAt.After(e.UpdatedAt) {
			es[i].PositionMs, es[i].DurationMs, es[i].Finished = l.PositionMs, l.DurationMs, l.Watched
		}
	}
	return nil
}

// Continue is «Продолжить просмотр» of online titles, newest first, at
// most limit (<= 0: all). A title's newest entry is left out only when
// that exact movie or episode is in the library and has its own progress
// there, or had it («Не просмотрено» since): the library's «Продолжить»
// owns it. An episode a partly downloaded series lacks stays here. Per
// title its newest entry:
// unfinished, it is offered as is; a finished movie is not offered; after
// a finished episode the next one is (Next: the one the source listed
// after it, else the next number, resumed where it was left if it was
// started), unless the source listed none after it.
func (p *ProgressStore) Continue(ctx context.Context, u User, limit int) ([]Entry, error) {
	if u.UID == "" {
		return nil, ErrNoProfile
	}
	// the newest entry of each title
	all, err := p.query(ctx, `WHERE user_uid=? AND updated_at=(SELECT MAX(updated_at) FROM online_progress n
	  WHERE n.user_uid=o.user_uid AND n.kind=o.kind AND n.tmdb_id=o.tmdb_id) ORDER BY updated_at DESC, rowid DESC LIMIT ?`, u.UID, maxContinueRows)
	if err != nil {
		return nil, err
	}
	type title struct {
		kind string
		id   int
	}
	seen := map[title]bool{}
	var out []Entry
	for _, e := range all {
		k := title{e.Kind, e.TMDBID}
		if seen[k] {
			continue
		}
		seen[k] = true
		if e.Finished && (e.Kind == "movie" || e.LastListed) {
			continue
		}
		local, err := p.hasLocal(ctx, u.ID, e)
		if err != nil {
			return nil, err
		}
		if local {
			continue // the library's «Продолжить» shows it
		}
		if e.Finished {
			if e, err = p.next(ctx, u.UID, e); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// next is the episode after the finished entry e, with its own progress
// when it was started (from the beginning when it was finished too).
func (p *ProgressStore) next(ctx context.Context, uid string, e Entry) (Entry, error) {
	s, n := e.Season, e.Episode+1
	if e.NextEpisode > 0 {
		s, n = e.NextSeason, e.NextEpisode
	}
	out := e
	out.Season, out.Episode, out.PositionMs, out.DurationMs, out.Finished, out.Next = s, n, 0, 0, false, true
	out.NextSeason, out.NextEpisode, out.LastListed = 0, 0, false
	had, err := p.query(ctx, `WHERE user_uid=? AND kind=? AND tmdb_id=? AND season=? AND episode=?`, uid, e.Kind, e.TMDBID, s, n)
	if err != nil {
		return e, err
	}
	if len(had) == 1 && !had[0].Finished {
		out.PositionMs, out.DurationMs = had[0].PositionMs, had[0].DurationMs
	}
	return out, nil
}

// hasLocal is whether the entry's own movie or episode is in the library
// with library progress of the user, or its position reached that
// progress before (cleared since by «Не просмотрено»).
func (p *ProgressStore) hasLocal(ctx context.Context, userID int64, e Entry) (bool, error) {
	if p.Lib == nil || p.Progress == nil {
		return false, nil
	}
	id, ok, err := p.libraryItem(ctx, userID, e)
	if refused(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("online progress: %w", err)
	}
	if !ok {
		return false, nil
	}
	if e.InLibrary {
		return true, nil
	}
	_, ok, err = p.local(ctx, userID, id)
	if refused(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("online progress: local: %w", err)
	}
	return ok, nil
}

// maxSeedRows bounds the entries one SeedLibrary pass looks at.
const maxSeedRows = 5000

// SeedLibrary gives a movie or episode that reached the library after it
// was watched online the online position, as of when it was reached (run
// after each library scan). Library progress that is as new or newer is
// kept: the core ignores an older position. Each online position is
// seeded at most once, and never one the mirror already wrote
// (in_library): a library row deleted by «Не просмотрено» stays deleted.
// A uid the profiles (Users) do not list, or a kid's, is skipped. A row the
// core refuses (its item over the profile's rating limit, or gone) is
// skipped and marked, so it is not sent again; one log line counts them.
func (p *ProgressStore) SeedLibrary(ctx context.Context) error {
	if p.Lib == nil || p.Progress == nil || p.Users == nil {
		return nil
	}
	users, err := p.Users.Users(ctx)
	if err != nil {
		return fmt.Errorf("online progress: profiles: %w", err)
	}
	ids := map[string]int64{}
	for _, u := range users {
		if u.UID != "" && !u.IsKid {
			ids[u.UID] = u.ID
		}
	}
	type row struct {
		uid string
		e   Entry
	}
	rows, err := p.DB.QueryContext(ctx, `SELECT user_uid, kind, tmdb_id, season, episode, position_ms, duration_ms, updated_at
	  FROM online_progress WHERE in_library=0 ORDER BY kind, tmdb_id LIMIT ?`, maxSeedRows)
	if err != nil {
		return fmt.Errorf("online progress: %w", err)
	}
	var all []row
	for rows.Next() {
		var r row
		var ts string
		if err := rows.Scan(&r.uid, &r.e.Kind, &r.e.TMDBID, &r.e.Season, &r.e.Episode, &r.e.PositionMs, &r.e.DurationMs, &ts); err != nil {
			rows.Close()
			return fmt.Errorf("online progress: %w", err)
		}
		if r.e.UpdatedAt, err = time.Parse(onlineTS, ts); err != nil {
			continue
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("online progress: %w", err)
	}
	// the library items of each title, looked up once per profile (a
	// profile sees the episodes its limits allow)
	type title struct {
		user int64
		kind string
		id   int
	}
	items := map[title]map[[2]int]int64{}
	skipped := 0
	defer func() {
		if skipped > 0 {
			log.Printf("online progress: seed: %d rows the library refused, skipped", skipped)
		}
	}()
	// skip marks a row the core refused: it is not sent again
	skip := func(r row) error {
		skipped++
		if err := p.markInLibrary(ctx, r.uid, r.e, r.e.UpdatedAt); err != nil {
			return fmt.Errorf("online progress: seed: %w", err)
		}
		return nil
	}
	for _, r := range all {
		user, ok := ids[r.uid]
		if !ok {
			continue
		}
		k := title{user, r.e.Kind, r.e.TMDBID}
		m, ok := items[k]
		if !ok {
			if m, err = p.titleItems(ctx, user, r.e.Kind, r.e.TMDBID); refused(err) {
				m, err = map[[2]int]int64{}, nil // the series as the profile may see it: none
			}
			if err != nil {
				return err
			}
			items[k] = m
		}
		id, ok := m[[2]int{r.e.Season, r.e.Episode}]
		if !ok {
			continue
		}
		l, had, err := p.local(ctx, user, id)
		switch {
		case refused(err):
			if err := skip(r); err != nil {
				return err
			}
			continue
		case err != nil:
			return fmt.Errorf("online progress: local: %w", err)
		case had && !l.UpdatedAt.Before(r.e.UpdatedAt):
			// the library has a position as new: it owns this one now
			if err := p.markInLibrary(ctx, r.uid, r.e, r.e.UpdatedAt); err != nil {
				return fmt.Errorf("online progress: seed: %w", err)
			}
			continue
		}
		err = p.Progress.SaveProgress(ctx, addonkit.ProgressSave{UserID: user, ItemID: id, PositionMs: r.e.PositionMs,
			DurationMs: r.e.DurationMs, At: r.e.UpdatedAt})
		if refused(err) {
			if err := skip(r); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("online progress: seed: %w", err)
		}
		if err := p.markInLibrary(ctx, r.uid, r.e, r.e.UpdatedAt); err != nil {
			return fmt.Errorf("online progress: seed: %w", err)
		}
	}
	return nil
}

// titleItems maps {season, episode} ({0, 0} for a movie) to the library
// items of a title (a series' episodes as profile userID sees them).
func (p *ProgressStore) titleItems(ctx context.Context, userID int64, kind string, tmdbID int) (map[[2]int]int64, error) {
	if kind != "movie" {
		m, err := p.episodes(ctx, userID, tmdbID, -1)
		if err != nil {
			return nil, fmt.Errorf("online progress: %w", err)
		}
		return m, nil
	}
	it, ok, err := p.Lib.ItemByTMDB(ctx, "movie", int64(tmdbID))
	if err != nil {
		return nil, fmt.Errorf("online progress: library: %w", err)
	}
	if !ok {
		return map[[2]int]int64{}, nil
	}
	return map[[2]int]int64{{0, 0}: it.ID}, nil
}
