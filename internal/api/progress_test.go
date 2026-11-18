package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/coretest"
	"github.com/antresol-app/antresol/addonkit"
)

// withCore mirrors the progress into core (the library and its progress,
// as *addonkit.Client would).
func withCore(core *coretest.Core) envOpt {
	return func(d *Deps) {
		d.Progress.Lib, d.Progress.Progress, d.Progress.Users = core, core, core
	}
}

// A save of a title the library has goes to the library's progress too
// (PUT /progress of the core, a played save), as the profile's own;
// an item the core refuses the profile (over its limit) is saved online
// only, and the answer is still 204.
func TestProgress_MirrorsLibraryItem(t *testing.T) {
	core := &coretest.Core{Profiles: []addonkit.User{*admin, *teen, *mama}}
	movie := core.AddMovie(3)
	ep := core.AddEpisode(438631, 1, 2)
	e := newEnv(t, fakeOnline(t, false), withCore(core))
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	body := `{"kind":"movie","tmdbId":3,"positionMs":60000,"durationMs":100000,"source":"alpha","updatedAt":"` + at.Format(time.RFC3339) + `"}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, mama); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if len(core.Saves) != 1 {
		t.Fatalf("saves: %+v", core.Saves)
	}
	s := core.Saves[0]
	if s.UserID != mama.ID || s.ItemID != movie || s.PositionMs != 60000 || s.DurationMs != 100000 || !s.Played || !s.At.Equal(at) {
		t.Fatalf("mirrored %+v", s)
	}
	if p, ok := core.Get(mama.ID, movie); !ok || p.PositionMs != 60000 {
		t.Fatalf("library progress %+v %v", p, ok)
	}
	// an episode: by season and number
	body = `{"kind":"series","tmdbId":438631,"season":1,"episode":2,"positionMs":5000,"durationMs":100000}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, mama); w.Code != 204 {
		t.Fatalf("episode put: %d %s", w.Code, w.Body)
	}
	if len(core.Saves) != 2 || core.Saves[1].ItemID != ep || core.Saves[1].UserID != mama.ID {
		t.Fatalf("episode mirror: %+v", core.Saves)
	}
	// the core refuses the item to the profile: saved online only
	core.Hidden = map[int64]bool{movie: true}
	body = `{"kind":"movie","tmdbId":3,"positionMs":70000,"durationMs":100000}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, teen); w.Code != 204 {
		t.Fatalf("refused mirror: %d %s", w.Code, w.Body)
	}
	if len(core.Saves) != 2 {
		t.Fatalf("a refused item was mirrored: %+v", core.Saves)
	}
	if w := e.do("GET", "/api/addons/online/x/progress?kind=movie&tmdbId=3", "", teen); w.Code != 200 || !strings.Contains(w.Body.String(), `"positionMs":70000`) {
		t.Fatalf("online entry: %d %s", w.Code, w.Body)
	}
	// each profile's own entries: the keying is the profile's uid
	if w := e.do("GET", "/api/addons/online/x/progress?kind=movie&tmdbId=3", "", mama); !strings.Contains(w.Body.String(), `"positionMs":60000`) {
		t.Fatalf("mama's entry: %s", w.Body)
	}
	var uids []string
	rows, err := e.db.Query(`SELECT DISTINCT user_uid FROM online_progress ORDER BY user_uid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var u string
		rows.Scan(&u)
		uids = append(uids, u)
	}
	rows.Close()
	if strings.Join(uids, ",") != "uid-mama,uid-teen" {
		t.Fatalf("uids %v", uids)
	}
}

// The poster of a saved title goes into the core's image cache (POST
// /images) in the background, and its cache name onto the title's
// entries; a failed fetch is not asked again at once.
func TestProgress_PosterThroughCoreImages(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	fail := true
	e := newEnv(t, fakeOnline(t, false), func(d *Deps) {
		d.CacheImage = func(_ context.Context, url string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, url)
			if fail {
				return "", errors.New("down")
			}
			return "0123456789abcdef0123456789abcdef01234567.jpg", nil
		}
	})
	body := `{"kind":"movie","tmdbId":3,"positionMs":1000,"durationMs":100000}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, mama); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	e.api.Wait()
	if w := e.do("PUT", "/api/addons/online/x/progress", body, mama); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	e.api.Wait()
	mu.Lock()
	if len(asked) != 1 || asked[0] != "https://image.example/t/p/w342/movie.jpg" {
		t.Fatalf("asked %v", asked)
	}
	mu.Unlock()
	// past the retry pause: fetched, recorded on the entry
	mu.Lock()
	fail = false
	mu.Unlock()
	e.api.posters.now = func() time.Time { return time.Now().Add(posterRetry + time.Minute) }
	if w := e.do("PUT", "/api/addons/online/x/progress", body, mama); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	e.api.Wait()
	var poster string
	if err := e.db.QueryRow(`SELECT poster FROM online_progress WHERE kind='movie' AND tmdb_id=3`).Scan(&poster); err != nil ||
		poster != "0123456789abcdef0123456789abcdef01234567.jpg" {
		t.Fatalf("poster %q %v", poster, err)
	}
}

func TestOnlineProgressRoutes(t *testing.T) {
	e := newEnv(t, fakeOnline(t, false))
	body := `{"kind":"series","tmdbId":3,"season":1,"episode":2,"positionMs":60000,"durationMs":100000,"source":"alpha","voice":"Дубляж","quality":"1080p"}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, admin); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	w := e.do("GET", "/api/addons/online/x/progress?kind=series&tmdbId=3", "", admin)
	var out struct {
		Entries []struct {
			Kind                   string
			TmdbID                 int `json:"tmdbId"`
			Season, Episode        int
			PositionMs             int64 `json:"positionMs"`
			Finished               bool
			Source, Voice, Quality string
		}
		Last *struct {
			Source, Voice, Quality string
			Season, Episode        int
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != 200 || err != nil || len(out.Entries) != 1 || out.Last == nil {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if en := out.Entries[0]; en.TmdbID != 3 || en.Season != 1 || en.Episode != 2 || en.PositionMs != 60000 || en.Finished || en.Voice != "Дубляж" {
		t.Fatalf("%+v", en)
	}
	if l := out.Last; l.Source != "alpha" || l.Quality != "1080p" || l.Season != 1 || l.Episode != 2 {
		t.Fatalf("last %+v", l)
	}
	// a queued save from before the stored one (updatedAt) is ignored
	stale := `{"kind":"series","tmdbId":3,"season":1,"episode":2,"positionMs":5000,"durationMs":100000,"source":"beta","updatedAt":"2020-01-01T00:00:00Z"}`
	if w := e.do("PUT", "/api/addons/online/x/progress", stale, admin); w.Code != 204 {
		t.Fatalf("stale put: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/progress?kind=series&tmdbId=3", "", admin); !strings.Contains(w.Body.String(), `"positionMs":60000`) {
		t.Fatalf("stale save overwrote: %s", w.Body)
	}
	w = e.do("GET", "/api/addons/online/x/progress?kind=movie&tmdbId=3", "", admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"entries":[]`) || !strings.Contains(w.Body.String(), `"last":null`) {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{"kind=game&tmdbId=3", "kind=movie&tmdbId=0", "kind=movie&tmdbId=999"} {
		if w := e.do("GET", "/api/addons/online/x/progress?"+bad, "", admin); w.Code != 400 {
			t.Fatalf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	for _, bad := range []string{`{"kind":"game","tmdbId":3}`, `{"kind":`} {
		if w := e.do("PUT", "/api/addons/online/x/progress", bad, admin); w.Code != 400 {
			t.Fatalf("bad put %s: %d %s", bad, w.Code, w.Body)
		}
	}
	if w := e.do("GET", "/api/addons/online/x/progress?kind=series&tmdbId=3", "", nil); w.Code != 403 {
		t.Fatalf("no profile: %d", w.Code)
	}
	// a header without a uid names no profile: refused at the gate, before
	// the store (whose ErrNoProfile stays for the scanner's identities)
	if w := e.do("PUT", "/api/addons/online/x/progress", body, &addonkit.User{ID: 9}); w.Code != 403 {
		t.Fatalf("no uid: %d %s", w.Code, w.Body)
	}
}

func TestOnlineProgressGate(t *testing.T) {
	e := newEnv(t, fakeOnline(t, false))
	body := `{"kind":"movie","tmdbId":3,"positionMs":1,"durationMs":10}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, kid); w.Code != 403 || errOf(w) != "Недоступно в детском профиле" {
		t.Fatalf("kid put: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/progress?kind=movie&tmdbId=3", "", kid); w.Code != 403 || errOf(w) != "Недоступно в детском профиле" {
		t.Fatalf("kid get: %d %s", w.Code, w.Body)
	}
	for _, id := range []string{"1", "2"} { // 18+, unrated
		if w := e.do("PUT", "/api/addons/online/x/progress", `{"kind":"movie","tmdbId":`+id+`,"positionMs":1,"durationMs":10}`, teen); w.Code != 403 ||
			errOf(w) != "Недоступно для этого профиля" {
			t.Fatalf("teen put %s: %d %s", id, w.Code, w.Body)
		}
		if w := e.do("GET", "/api/addons/online/x/progress?kind=movie&tmdbId="+id, "", teen); w.Code != 403 {
			t.Fatalf("teen get %s: %d %s", id, w.Code, w.Body)
		}
	}
	if w := e.do("PUT", "/api/addons/online/x/progress", body, teen); w.Code != 204 {
		t.Fatalf("teen 12+: %d %s", w.Code, w.Body)
	}
}
