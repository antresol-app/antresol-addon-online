package hooks

import (
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// «Продолжить» of a device with a separate history is that history's;
// a history GET /users lists under its profile keeps its rows after a
// scan, one no longer listed (deleted) loses them.
func TestSeparateHistoryContinueAndCleanUp(t *testing.T) {
	e := newHookEnv(t, Deps{})
	tvHist := addonkit.History{ID: 30, UID: "uid-tv", Name: "Гостиная"}
	tv := mama
	tv.History = &tvHist
	e.save(tv.HistoryUser(), movie(438631, "12+", 1000))
	e.save(mama, movie(7, "", 2000))
	cards := e.cards(&tv)
	if len(cards) != 1 || cards[0].TMDBID != 438631 {
		t.Fatalf("tv cards %+v", cards)
	}
	if own := e.cards(&mama); len(own) != 1 || own[0].TMDBID != 7 {
		t.Fatalf("person's cards %+v", own)
	}

	e.core.Profiles[1].Histories = []addonkit.History{tvHist}
	if w := e.call("library.afterScan", nil, `{}`); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if e.rows("uid-tv") != 1 {
		t.Fatal("a listed history lost its rows")
	}
	e.core.Profiles[1].Histories = nil
	e.call("library.afterScan", nil, `{}`)
	if e.rows("uid-tv") != 0 || e.rows(mama.UID) != 1 {
		t.Fatalf("rows: tv %d mama %d", e.rows("uid-tv"), e.rows(mama.UID))
	}
}
