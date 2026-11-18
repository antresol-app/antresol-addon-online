package online

import (
	"context"
	"fmt"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
)

func seedWatched(t *testing.T) *progressEnv {
	t.Helper()
	e := newProgressEnv(t)
	// another profile's rows (a finished episode and a position) must not show
	if _, err := e.ps.DB.Exec(`INSERT INTO online_progress (user_uid, kind, tmdb_id, season, episode, position_ms, finished, updated_at) VALUES
		(?, 'series', 100, 1, 5, 0, 1, 'x'), (?, 'series', 100, 1, 6, 8000, 0, 'x')`, "u-papa", "u-papa"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ps.DB.Exec(`INSERT INTO online_progress (user_uid, kind, tmdb_id, season, episode, position_ms, finished, updated_at) VALUES
		(?, 'series', 100, 1, 2, 0, 1, 'x'), (?, 'series', 100, 1, 3, 90000, 0, 'x'), (?, 'movie', 100, 0, 0, 7000, 0, 'x')`,
		e.u.UID, e.u.UID, e.u.UID); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestWatchedFunc(t *testing.T) {
	e := seedWatched(t)
	got, err := WatchedFunc(e.ps.DB)(context.Background(), e.u.UID, tracking.Title{Kind: "series", TMDBID: 100})
	if err != nil || fmt.Sprint(got) != "map[[1 2]:true]" {
		t.Fatalf("watched %v %v", got, err)
	}
}

func TestWatchStateFunc(t *testing.T) {
	e := seedWatched(t)
	got, err := WatchStateFunc(e.ps.DB)(context.Background(), e.u.UID, "series", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[[2]int]WatchState{{1, 2}: {Watched: true}, {1, 3}: {PositionSec: 90}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("states %v, want %v", got, want)
	}
}
