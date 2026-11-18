package online

import (
	"context"
	"database/sql"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
)

// WatchState is one episode's state for a profile (the core's
// tracking.WatchState; the tracking.watched hook's WatchedEpisode).
type WatchState struct {
	Watched     bool
	PositionSec int
}

// WatchedFn is the episodes (a film: [0, 0]) profile uid finished of a
// title.
type WatchedFn func(ctx context.Context, uid string, t tracking.Title) (map[[2]int]bool, error)

// WatchStateFn is profile uid's state per episode of a title.
type WatchStateFn func(ctx context.Context, uid string, kind string, tmdbID int64) (map[[2]int]WatchState, error)

// WatchedFunc is «Смотреть онлайн»'s watched episodes: the title's
// episodes (a film: [0, 0]) the profile finished there.
func WatchedFunc(db *sql.DB) WatchedFn {
	return func(ctx context.Context, uid string, t tracking.Title) (map[[2]int]bool, error) {
		rows, err := db.QueryContext(ctx, `SELECT season, episode FROM online_progress
			WHERE user_uid = ? AND kind = ? AND tmdb_id = ? AND finished = 1`, uid, t.Kind, t.TMDBID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[[2]int]bool{}
		for rows.Next() {
			var k [2]int
			if err := rows.Scan(&k[0], &k[1]); err != nil {
				return nil, err
			}
			out[k] = true
		}
		return out, rows.Err()
	}
}

// WatchStateFunc is «Смотреть онлайн»'s state per episode (a film: [0, 0]):
// the profile finished it there (watched), or else where it stopped
// (PositionSec).
func WatchStateFunc(db *sql.DB) WatchStateFn {
	return func(ctx context.Context, uid string, kind string, tmdbID int64) (map[[2]int]WatchState, error) {
		rows, err := db.QueryContext(ctx, `SELECT season, episode, finished, position_ms FROM online_progress
			WHERE user_uid = ? AND kind = ? AND tmdb_id = ? AND (finished = 1 OR position_ms > 0)`, uid, kind, tmdbID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[[2]int]WatchState{}
		for rows.Next() {
			var k [2]int
			var finished, ms int
			if err := rows.Scan(&k[0], &k[1], &finished, &ms); err != nil {
				return nil, err
			}
			st := WatchState{Watched: finished == 1}
			if !st.Watched {
				st.PositionSec = ms / 1000
			}
			out[k] = st
		}
		return out, rows.Err()
	}
}
