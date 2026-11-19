package api

import (
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/coretest"
	"github.com/antresol-app/antresol/addonkit"
)

// A request that writes a separate history (the header's history: a
// shared TV) keeps its online positions under the history's uid and
// mirrors them into the history's library progress; the profile's own
// requests do not see them. The gate is still the profile's.
func TestProgress_SeparateHistory(t *testing.T) {
	tvHist := addonkit.History{ID: 30, UID: "uid-tv", Name: "Гостиная"}
	m := *mama
	m.Histories = []addonkit.History{tvHist}
	core := &coretest.Core{Profiles: []addonkit.User{*admin, *teen, m}}
	movie := core.AddMovie(3)
	e := newEnv(t, fakeOnline(t, false), withCore(core))
	tv := *mama
	tv.History = &tvHist
	body := `{"kind":"movie","tmdbId":3,"positionMs":60000,"durationMs":100000,"source":"alpha"}`
	if w := e.do("PUT", "/api/addons/online/x/progress", body, &tv); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if len(core.Saves) != 1 || core.Saves[0].UserID != tvHist.ID || core.Saves[0].ItemID != movie {
		t.Fatalf("mirror %+v", core.Saves)
	}
	var uid string
	e.db.QueryRow(`SELECT user_uid FROM online_progress`).Scan(&uid)
	if uid != "uid-tv" {
		t.Fatalf("keyed by %q", uid)
	}
	if w := e.do("GET", "/api/addons/online/x/progress?kind=movie&tmdbId=3", "", &tv); !strings.Contains(w.Body.String(), `"positionMs":60000`) {
		t.Fatalf("tv reads: %s", w.Body)
	}
	if w := e.do("GET", "/api/addons/online/x/progress?kind=movie&tmdbId=3", "", mama); !strings.Contains(w.Body.String(), `"entries":[]`) {
		t.Fatalf("the person sees the TV's: %s", w.Body)
	}
	// the profile's limit applies to its history: an 18+ title is refused
	lim := *teen
	lim.History = &addonkit.History{ID: 31, UID: "uid-teen-tv"}
	if w := e.do("PUT", "/api/addons/online/x/progress", `{"kind":"movie","tmdbId":1,"positionMs":1,"durationMs":10}`, &lim); w.Code == 204 {
		t.Fatalf("over the limit: %d", w.Code)
	}
}
