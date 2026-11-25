package online

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/coretest"
	"github.com/antresol-app/antresol/addonkit"
)

type progressEnv struct {
	ps   *ProgressStore
	core *coretest.Core
	u    User
	now  time.Time
}

func newProgressEnv(t *testing.T) *progressEnv {
	t.Helper()
	u := User{ID: 1, UID: "u-mama", Name: "Мама", IsAdmin: true}
	e := &progressEnv{core: &coretest.Core{Profiles: []addonkit.User{u}}, u: u, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e.ps = &ProgressStore{DB: testDB(t), Lib: e.core, Progress: e.core, Users: e.core, Now: func() time.Time { return e.now }}
	// both clocks are the test's: nothing depends on the wall clock
	e.core.Now = func() time.Time { return e.now }
	return e
}

// local is the profile's library progress of item id (ok false: none).
func (e *progressEnv) local(id int64) (addonkit.ProgressEntry, bool) { return e.core.Get(e.u.ID, id) }

func (e *progressEnv) tick() { e.now = e.now.Add(time.Minute) }

func TestOnlineProgressSaveAndFinish(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 1, Episode: 2, PositionMs: 60_000, DurationMs: 100_000,
		Source: "alpha", Voice: "Дубляж", Quality: "1080p"}); err != nil {
		t.Fatal(err)
	}
	got, last, err := e.ps.Get(ctx, e.u, "series", 1396)
	if err != nil || len(got) != 1 || got[0].Finished || got[0].PositionMs != 60_000 {
		t.Fatalf("60%%: %+v %v", got, err)
	}
	if last != (Last{Source: "alpha", Voice: "Дубляж", Quality: "1080p", Season: 1, Episode: 2}) {
		t.Fatalf("last %+v", last)
	}
	e.tick()
	// the same s/e again: the row is updated, not added
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 1, Episode: 2, PositionMs: 93_000, DurationMs: 100_000,
		Source: "beta", Quality: "720p"}); err != nil {
		t.Fatal(err)
	}
	got, last, err = e.ps.Get(ctx, e.u, "series", 1396)
	if err != nil || len(got) != 1 || !got[0].Finished || got[0].PositionMs != 93_000 || got[0].Source != "beta" {
		t.Fatalf("93%%: %+v %v", got, err)
	}
	if last.Source != "beta" || last.Quality != "720p" || last.Season != 1 || last.Episode != 2 {
		t.Fatalf("last %+v", last)
	}
	// another episode, newer: it is Last; entries are newest first
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 1, Episode: 3, PositionMs: 1000, DurationMs: 100_000, Source: "alpha"}); err != nil {
		t.Fatal(err)
	}
	got, last, err = e.ps.Get(ctx, e.u, "series", 1396)
	if err != nil || len(got) != 2 || got[0].Episode != 3 || last.Episode != 3 || last.Source != "alpha" {
		t.Fatalf("%+v %+v %v", got, last, err)
	}
	// a movie ignores season and episode
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 7, Season: 4, Episode: 5, PositionMs: 1, DurationMs: 10}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := e.ps.Get(ctx, e.u, "movie", 7); len(got) != 1 || got[0].Season != 0 || got[0].Episode != 0 {
		t.Fatalf("movie %+v", got)
	}
	if got, last, err := e.ps.Get(ctx, e.u, "movie", 8); err != nil || len(got) != 0 || last != (Last{}) {
		t.Fatalf("none: %+v %+v %v", got, last, err)
	}
}

// Only plain hints are kept: a ref (or anything that is not a source id)
// is never stored, so nothing stored can be replayed as a stale ref.
func TestOnlineProgressKeepsOnlyHints(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 7, PositionMs: 1, DurationMs: 10,
		Source: "eyJwIjoiL2xpdGUvYSJ9.abc", Voice: "bad\nvoice", Quality: string(long)}); err != nil {
		t.Fatal(err)
	}
	_, last, err := e.ps.Get(ctx, e.u, "movie", 7)
	if err != nil || last.Source != "" || last.Voice != "" || last.Quality != "" {
		t.Fatalf("%+v %v", last, err)
	}
	for _, bad := range []Entry{{Kind: "game", TMDBID: 1}, {Kind: "movie"}, {Kind: "series", TMDBID: 1, Season: -1}} {
		if err := e.ps.Save(ctx, e.u, bad); err == nil {
			t.Fatalf("%+v saved", bad)
		}
	}
}

func TestOnlineProgressMirrorsLibrary(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	movie := e.core.AddMovie(438631)
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 120_000, DurationMs: 9_000_000}); err != nil {
		t.Fatal(err)
	}
	p, ok := e.local(movie)
	if !ok || p.PositionMs != 120_000 || p.DurationMs != 9_000_000 {
		t.Fatalf("movie mirror: %+v %v", p, ok)
	}
	var eps [3]int64
	for i := 1; i <= 2; i++ {
		id := e.core.AddEpisode(1396, 1, i)
		eps[i] = id
	}
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 1, Episode: 2, PositionMs: 30_000, DurationMs: 60_000}); err != nil {
		t.Fatal(err)
	}
	if p, ok := e.local(eps[2]); !ok || p.PositionMs != 30_000 {
		t.Fatalf("episode mirror: %+v %v", p, ok)
	}
	if _, ok := e.local(eps[1]); ok {
		t.Fatal("S1E1 touched")
	}
	// an episode the library lacks: kept online only
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 2, Episode: 1, PositionMs: 1, DurationMs: 60_000}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := e.ps.Get(ctx, e.u, "series", 1396); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestOnlineProgressContinue(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	e.core.AddMovie(438631)
	save := func(en Entry) {
		t.Helper()
		e.tick()
		if err := e.ps.Save(ctx, e.u, en); err != nil {
			t.Fatal(err)
		}
	}
	save(Entry{Kind: "movie", TMDBID: 438631, PositionMs: 10, DurationMs: 100}) // in the library
	save(Entry{Kind: "movie", TMDBID: 7, PositionMs: 10, DurationMs: 100})
	save(Entry{Kind: "movie", TMDBID: 8, PositionMs: 95, DurationMs: 100}) // finished
	save(Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 1, PositionMs: 10, DurationMs: 100})
	save(Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 2, PositionMs: 20, DurationMs: 100})
	got, err := e.ps.Continue(ctx, e.u, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].Kind != "series" || got[0].TMDBID != 9 || got[0].Episode != 2 || got[1].TMDBID != 7 {
		t.Fatalf("%+v", got)
	}
	if got, _ := e.ps.Continue(ctx, e.u, 1); len(got) != 1 || got[0].TMDBID != 9 {
		t.Fatalf("limit: %+v", got)
	}
	// the newest episode finished, the list unknown: the next number is offered
	save(Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 2, PositionMs: 99, DurationMs: 100, Source: "alpha"})
	got, _ = e.ps.Continue(ctx, e.u, 10)
	if len(got) != 2 || got[0].TMDBID != 9 || got[0].Season != 1 || got[0].Episode != 3 || got[0].PositionMs != 0 || !got[0].Next ||
		got[0].Finished || got[0].Source != "alpha" {
		t.Fatalf("next by number: %+v", got)
	}
	// the source listed S2E1 after it
	save(Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 2, PositionMs: 99, DurationMs: 100, NextSeason: 2, NextEpisode: 1})
	if got, _ := e.ps.Continue(ctx, e.u, 10); got[0].Season != 2 || got[0].Episode != 1 {
		t.Fatalf("listed next: %+v", got)
	}
	// the next one was started before: resumed there
	save(Entry{Kind: "series", TMDBID: 9, Season: 2, Episode: 1, PositionMs: 40, DurationMs: 100})
	save(Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 2, PositionMs: 99, DurationMs: 100, NextSeason: 2, NextEpisode: 1})
	if got, _ := e.ps.Continue(ctx, e.u, 10); got[0].Season != 2 || got[0].Episode != 1 || got[0].PositionMs != 40 {
		t.Fatalf("started next: %+v", got)
	}
	// the last episode the source listed is finished: the series leaves the shelf
	save(Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 2, PositionMs: 99, DurationMs: 100, LastListed: true})
	if got, _ := e.ps.Continue(ctx, e.u, 10); len(got) != 1 || got[0].TMDBID != 7 {
		t.Fatalf("last listed: %+v", got)
	}
	// unfinished, «last» does not matter
	save(Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 2, PositionMs: 50, DurationMs: 100, LastListed: true})
	if got, _ := e.ps.Continue(ctx, e.u, 10); len(got) != 2 || got[0].Episode != 2 || got[0].Next {
		t.Fatalf("unfinished last: %+v", got)
	}
}

func TestOnlineProgressSnapshot(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	poster := "0123456789abcdef0123456789abcdef01234567.jpg"
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 7, PositionMs: 1, DurationMs: 10,
		Title: "Альфа", Rating: "16+", Year: 2021, Poster: poster}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	// a save without a snapshot keeps the stored one; a bad poster name is dropped
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 7, PositionMs: 2, DurationMs: 10, Poster: "../x.jpg"}); err != nil {
		t.Fatal(err)
	}
	got, err := e.ps.Continue(ctx, e.u, 0)
	if err != nil || len(got) != 1 || got[0].Title != "Альфа" || got[0].Rating != "16+" || got[0].Year != 2021 || got[0].Poster != poster || got[0].PositionMs != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	other := "fedcba9876543210fedcba9876543210fedcba98.jpg"
	if err := e.ps.SetPoster(ctx, "movie", 7, other); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.ps.Continue(ctx, e.u, 0); got[0].Poster != other {
		t.Fatalf("%+v", got)
	}
}

type failingLib struct{}

func (failingLib) ItemByTMDB(context.Context, string, int64) (addonkit.Item, bool, error) {
	return addonkit.Item{}, false, errors.New("disk")
}
func (failingLib) EpisodeItems(context.Context, int64, int64) ([]addonkit.EpisodeItem, error) {
	return nil, errors.New("disk")
}

// A mirror that fails does not fail the save: the online row is stored.
func TestOnlineProgressMirrorFailureKeepsRow(t *testing.T) {
	e := newProgressEnv(t)
	e.ps.Lib = failingLib{}
	if err := e.ps.Save(context.Background(), e.u, Entry{Kind: "movie", TMDBID: 7, PositionMs: 1, DurationMs: 10}); err != nil {
		t.Fatal(err)
	}
	e.ps.Lib = e.core
	if got, _, _ := e.ps.Get(context.Background(), e.u, "movie", 7); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

// Get carries newer local playback of the mirrored item.
func TestOnlineProgressGetSeesLocalPlayback(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	movie := e.core.AddMovie(438631)
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 1000, DurationMs: 10_000}); err != nil {
		t.Fatal(err)
	}
	e.core.Play(e.u.ID, movie, 5000, 10_000, e.now.Add(time.Hour))
	if got, _, _ := e.ps.Get(ctx, e.u, "movie", 438631); len(got) != 1 || got[0].PositionMs != 5000 {
		t.Fatalf("%+v", got)
	}
	ep := e.core.AddEpisode(9, 1, 1)
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 1, PositionMs: 1000, DurationMs: 10_000}); err != nil {
		t.Fatal(err)
	}
	e.core.Play(e.u.ID, ep, 9500, 10_000, e.now.Add(2*time.Hour))
	if got, _, _ := e.ps.Get(ctx, e.u, "series", 9); len(got) != 1 || got[0].PositionMs != 9500 || !got[0].Finished {
		t.Fatalf("%+v", got)
	}
}

// A save queued by a client and sent late never overwrites a newer position.
func TestOnlineProgressStaleSaveIgnored(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	old := e.now
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 90_000, DurationMs: 1_000_000, Source: "alpha"}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	// reached before the stored one: ignored
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 30_000, DurationMs: 1_000_000, Source: "beta", At: old}); err != nil {
		t.Fatal(err)
	}
	got, _, err := e.ps.Get(ctx, e.u, "movie", 438631)
	if err != nil || len(got) != 1 || got[0].PositionMs != 90_000 || got[0].Source != "alpha" {
		t.Fatalf("stale overwrote: %+v %v", got, err)
	}
	// reached later (and a clock ahead is clamped to now): kept
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 120_000, DurationMs: 1_000_000, Source: "beta", At: e.now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = e.ps.Get(ctx, e.u, "movie", 438631)
	if got[0].PositionMs != 120_000 || !got[0].UpdatedAt.Equal(e.now) {
		t.Fatalf("newer: %+v", got[0])
	}
}

// «Продолжить» drops an online entry only when that exact movie or
// episode is in the library with its own progress: an episode of a partly
// downloaded series stays an online card.
func TestOnlineProgressContinueKeepsEpisodeMissingFromLibrary(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		e.core.AddEpisode(1396, 1, i)
	}
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 2, Episode: 1, PositionMs: 40, DurationMs: 100}); err != nil {
		t.Fatal(err)
	}
	got, err := e.ps.Continue(ctx, e.u, 10)
	if err != nil || len(got) != 1 || got[0].TMDBID != 1396 || got[0].Season != 2 || got[0].Episode != 1 || got[0].PositionMs != 40 {
		t.Fatalf("online-only episode: %+v %v", got, err)
	}
	// a library episode watched online is mirrored: the library shelf shows it
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 1, Episode: 2, PositionMs: 30, DurationMs: 100}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.ps.Continue(ctx, e.u, 10); err != nil || len(got) != 0 {
		t.Fatalf("library episode: %+v %v", got, err)
	}
}

// A title watched online and added to the library later keeps its card
// until the library has its own progress; SeedLibrary (after a scan) gives
// it the online position, as of when it was reached.
func TestOnlineProgressSeedsLibraryAddedLater(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 50_000, DurationMs: 100_000}); err != nil {
		t.Fatal(err)
	}
	reached := e.now
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 3, PositionMs: 20_000, DurationMs: 100_000}); err != nil {
		t.Fatal(err)
	}
	movie := e.core.AddMovie(438631)
	ep := e.core.AddEpisode(9, 1, 3)
	// in the library, without its own progress: still on the shelf
	if got, err := e.ps.Continue(ctx, e.u, 10); err != nil || len(got) != 2 {
		t.Fatalf("before seeding: %+v %v", got, err)
	}
	e.tick()
	if err := e.ps.SeedLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	p, ok := e.local(movie)
	if !ok || p.PositionMs != 50_000 || p.DurationMs != 100_000 || !p.UpdatedAt.Equal(reached) {
		t.Fatalf("movie seed: %+v %v", p, ok)
	}
	if p, ok := e.local(ep); !ok || p.PositionMs != 20_000 {
		t.Fatalf("episode seed: %+v %v", p, ok)
	}
	// now the library shelf has them
	if got, err := e.ps.Continue(ctx, e.u, 10); err != nil || len(got) != 0 {
		t.Fatalf("after seeding: %+v %v", got, err)
	}
	// newer local playback is never overwritten by a later seeding
	e.tick()
	e.core.Play(e.u.ID, movie, 70_000, 100_000, e.now)
	if err := e.ps.SeedLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.local(movie); p.PositionMs != 70_000 {
		t.Fatalf("local overwritten: %+v", p)
	}
}

// The library mirror carries the online row's own timestamp.
func TestOnlineProgressMirrorUsesSaveTime(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	movie := e.core.AddMovie(438631)
	e.core.Now = func() time.Time { return e.now.Add(time.Hour) } // a different clock must not leak in
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 1000, DurationMs: 10_000}); err != nil {
		t.Fatal(err)
	}
	if p, ok := e.local(movie); !ok || !p.UpdatedAt.Equal(e.now) {
		t.Fatalf("%+v %v (want %v)", p, ok, e.now)
	}
}

// «Не просмотрено» on a library item is the newest word: a scan never
// brings the online position back, and the online card does not return.
func TestOnlineProgressSeedRespectsUnwatched(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	// mirrored at save time (the library had it), then unmarked
	movie := e.core.AddMovie(438631)
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 40_000, DurationMs: 100_000}); err != nil {
		t.Fatal(err)
	}
	// seeded at a scan (added after it was watched online), then unmarked
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 9, Season: 1, Episode: 1, PositionMs: 95_000, DurationMs: 100_000}); err != nil {
		t.Fatal(err)
	}
	ep := e.core.AddEpisode(9, 1, 1)
	if err := e.ps.SeedLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	if p, ok := e.local(ep); !ok || !p.Watched {
		t.Fatalf("seed: %+v %v", p, ok)
	}
	e.tick()
	for _, id := range []int64{movie, ep} {
		e.core.Unwatch(e.u.ID, id)
	}
	if err := e.ps.SeedLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{movie, ep} {
		if p, ok := e.local(id); ok {
			t.Fatalf("item %d resurrected: %+v", id, p)
		}
	}
	if got, err := e.ps.Continue(ctx, e.u, 10); err != nil || len(got) != 0 {
		t.Fatalf("online card back: %+v %v", got, err)
	}
	// a new online position after the unmark reaches the library again
	e.tick()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 10_000, DurationMs: 100_000}); err != nil {
		t.Fatal(err)
	}
	if p, ok := e.local(movie); !ok || p.PositionMs != 10_000 {
		t.Fatalf("new save: %+v %v", p, ok)
	}
}

type countLib struct {
	LibraryLookup
	episodes atomic.Int32
}

func (c *countLib) EpisodeItems(ctx context.Context, userID, tmdbID int64) ([]addonkit.EpisodeItem, error) {
	c.episodes.Add(1)
	return c.LibraryLookup.EpisodeItems(ctx, userID, tmdbID)
}

type countProgress struct {
	LibraryProgress
	saves atomic.Int32
}

func (c *countProgress) SaveProgress(ctx context.Context, p addonkit.ProgressSave) error {
	c.saves.Add(1)
	return c.LibraryProgress.SaveProgress(ctx, p)
}

// The player saves every 10 s: the library's episodes of the series are
// listed once for a while, and a save close to the last mirrored position
// is not mirrored again (the online position is stored all the same); a
// move of mirrorEvery or more, a seek back and a finish are.
func TestOnlineProgressMirrorIsThrottled(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	lib, prog := &countLib{LibraryLookup: e.core}, &countProgress{LibraryProgress: e.core}
	e.ps.Lib, e.ps.Progress = lib, prog
	ep := e.core.AddEpisode(1396, 1, 1)
	save := func(pos int64, finished bool) {
		t.Helper()
		e.tick()
		if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 1, Episode: 1, PositionMs: pos, DurationMs: 3_000_000, Finished: finished}); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct {
		pos      int64
		finished bool
		mirrored bool
	}{
		{100_000, false, true},  // the first
		{110_000, false, false}, // 10 s on
		{125_000, false, false}, // 25 s on
		{131_000, false, true},  // 31 s from the mirrored one
		{20_000, false, true},   // a seek back
		{2_900_000, true, true}, // the end
	} {
		before := prog.saves.Load()
		save(c.pos, c.finished)
		if got := prog.saves.Load() > before; got != c.mirrored {
			t.Fatalf("save %d at %d ms: mirrored %v, want %v", i, c.pos, got, c.mirrored)
		}
		var stored, inLib int64 // the online row follows every save, mirrored or not, and stays the library's
		e.ps.DB.QueryRow(`SELECT position_ms, in_library FROM online_progress WHERE user_uid=? AND kind='series' AND tmdb_id=1396`, e.u.UID).Scan(&stored, &inLib)
		if stored != c.pos || inLib != 1 {
			t.Fatalf("save %d: stored position %d (want %d), in_library %d (want 1)", i, stored, c.pos, inLib)
		}
	}
	if n := lib.episodes.Load(); n != 1 {
		t.Fatalf("the library's episodes listed %d times by 6 saves, want 1", n)
	}
	if got, _, _ := e.ps.Get(ctx, e.u, "series", 1396); len(got) != 1 || got[0].PositionMs != 2_900_000 {
		t.Fatalf("the online position is not stored: %+v", got)
	}
	if p, ok := e.local(ep); !ok || p.PositionMs != 2_900_000 {
		t.Fatalf("library progress %+v %v", p, ok)
	}
}
