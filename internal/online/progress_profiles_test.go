package online

import (
	"context"
	"errors"
	"testing"
)

// Online positions, their mirror into the library and the seeding of a
// title added later are all one profile's: another profile sees none.
func TestOnlineProgressIsPerProfile(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	other := User{ID: 2, UID: "u-papa", Name: "Папа"}
	e.core.Profiles = append(e.core.Profiles, other)
	movie := e.core.AddMovie(438631)
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 438631, PositionMs: 120_000, DurationMs: 9_000_000}); err != nil {
		t.Fatal(err)
	}
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "series", TMDBID: 1396, Season: 1, Episode: 2, PositionMs: 1000, DurationMs: 60_000}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := e.ps.Get(ctx, other, "movie", 438631); err != nil || len(got) != 0 {
		t.Fatalf("other profile's online entry: %+v %v", got, err)
	}
	if got, err := e.ps.Continue(ctx, other, 20); err != nil || len(got) != 0 {
		t.Fatalf("other profile's «Продолжить»: %+v %v", got, err)
	}
	if _, ok := e.core.Get(other.ID, movie); ok {
		t.Fatal("mirrored into the other profile")
	}
	// a series added later is seeded for its watcher only
	ep := e.core.AddEpisode(1396, 1, 2)
	if err := e.ps.SeedLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	if p, ok := e.local(ep); !ok || p.PositionMs != 1000 {
		t.Fatalf("seed: %+v %v", p, ok)
	}
	if _, ok := e.core.Get(other.ID, ep); ok {
		t.Fatal("seeded into the other profile")
	}
}

// Rows are the uid's, not the id's: a new profile given a deleted one's id
// sees none of its rows, and a uid the profiles no longer list is not
// seeded into anyone's library.
func TestOnlineProgressKeyedByUID(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 7, PositionMs: 10, DurationMs: 100}); err != nil {
		t.Fatal(err)
	}
	heir := User{ID: e.u.ID, UID: "u-new", Name: "Новый"}
	if got, err := e.ps.Continue(ctx, heir, 10); err != nil || len(got) != 0 {
		t.Fatalf("the id's new profile sees the old rows: %+v %v", got, err)
	}
	e.core.Profiles = []User{heir}
	movie := e.core.AddMovie(7)
	if err := e.ps.SeedLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	if p, ok := e.core.Get(heir.ID, movie); ok {
		t.Fatalf("a deleted profile's row seeded into its id's heir: %+v", p)
	}
}

// A row the core refuses never blocks the seeding of the others: an item
// over the profile's limit (404) is skipped and marked (not sent again),
// a kid's rows are not looked at (its calls are 400), and every other
// profile's titles, before and after them, are seeded.
func TestOnlineProgressSeedSkipsRefusedRows(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	kid := User{ID: 3, UID: "u-kid", Name: "Маша", IsKid: true}
	e.core.Profiles = append(e.core.Profiles, kid)
	// in the store's order (kind, tmdb_id) the refused rows come first
	for _, en := range []Entry{
		{Kind: "movie", TMDBID: 1, PositionMs: 10, DurationMs: 100},
		{Kind: "movie", TMDBID: 2, PositionMs: 20, DurationMs: 100},
		{Kind: "series", TMDBID: 9, Season: 1, Episode: 1, PositionMs: 30, DurationMs: 100},
	} {
		e.tick()
		if err := e.ps.Save(ctx, e.u, en); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.ps.DB.Exec(`INSERT INTO online_progress (user_uid, kind, tmdb_id, season, episode, position_ms, duration_ms, updated_at)
		VALUES (?, 'movie', 1, 0, 0, 5, 100, ?)`, kid.UID, e.now.Format(onlineTS)); err != nil {
		t.Fatal(err)
	}
	hidden := e.core.AddMovie(1)
	e.core.Hidden = map[int64]bool{hidden: true}
	movie := e.core.AddMovie(2)
	ep := e.core.AddEpisode(9, 1, 1)
	if err := e.ps.SeedLibrary(ctx); err != nil {
		t.Fatalf("a refused row failed the pass: %v", err)
	}
	if p, ok := e.local(movie); !ok || p.PositionMs != 20 {
		t.Fatalf("movie after the refused one: %+v %v", p, ok)
	}
	if p, ok := e.local(ep); !ok || p.PositionMs != 30 {
		t.Fatalf("episode after the refused one: %+v %v", p, ok)
	}
	for _, s := range e.core.Saves {
		if s.UserID == kid.ID || s.ItemID == hidden {
			t.Fatalf("a refused save was taken: %+v", s)
		}
	}
	var marked int
	if err := e.ps.DB.QueryRow(`SELECT in_library FROM online_progress WHERE user_uid=? AND tmdb_id=1`, e.u.UID).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("the refused row is not marked: %d %v", marked, err)
	}
	// the next pass sends nothing again
	n := len(e.core.Saves)
	if err := e.ps.SeedLibrary(ctx); err != nil || len(e.core.Saves) != n {
		t.Fatalf("second pass: %v, %d saves (was %d)", err, len(e.core.Saves), n)
	}
}

// The mirror of a save into an item the profile may not see is no error
// and no write: the online row is stored as if the library had none.
func TestOnlineProgressMirrorRefusedItem(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	e.core.Hidden = map[int64]bool{e.core.AddMovie(7): true}
	if err := e.ps.Save(ctx, e.u, Entry{Kind: "movie", TMDBID: 7, PositionMs: 1, DurationMs: 10}); err != nil {
		t.Fatal(err)
	}
	if len(e.core.Saves) != 0 {
		t.Fatalf("mirrored: %+v", e.core.Saves)
	}
	if got, _, err := e.ps.Get(ctx, e.u, "movie", 7); err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

// A profile without a uid keeps no progress.
func TestOnlineProgressNeedsUID(t *testing.T) {
	e := newProgressEnv(t)
	ctx := context.Background()
	anon := User{ID: 1, Name: "без uid"}
	if err := e.ps.Save(ctx, anon, Entry{Kind: "movie", TMDBID: 7, PositionMs: 1, DurationMs: 10}); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("save: %v", err)
	}
	if _, _, err := e.ps.Get(ctx, anon, "movie", 7); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("get: %v", err)
	}
	if _, err := e.ps.Continue(ctx, anon, 10); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("continue: %v", err)
	}
	var n int
	if err := e.ps.DB.QueryRow(`SELECT COUNT(*) FROM online_progress`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows: %d %v", n, err)
	}
}
