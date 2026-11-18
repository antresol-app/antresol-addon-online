package coreadapt_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
	"github.com/antresol-app/antresol/addonkit"
	"github.com/antresol-app/antresol/addonkit/addonkittest"
)

// goldenSrc is the core's recorded answers: the core checkout's
// (ANTRESOL_CORE, as make sets it, relative to the module root), else
// addonkit's own.
func goldenSrc() string {
	if core := os.Getenv("ANTRESOL_CORE"); core != "" {
		if !filepath.IsAbs(core) {
			core = filepath.Join("..", "..", core)
		}
		return filepath.Join(core, "addonkit", "testdata", "golden")
	}
	return addonkittest.GoldenDir()
}

// The tracking adapter maps the core's recorded sources onto the copied
// model and back; a title the core does not track has none.
func TestTrackingSources(t *testing.T) {
	srv := addonkittest.ReplayGolden(t, goldenSrc())
	tr := coreadapt.Tracking{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})}
	ctx := context.Background()
	srcs, err := tr.Sources(ctx, 1)
	if err != nil || len(srcs) != 3 {
		t.Fatalf("sources %+v %v", srcs, err)
	}
	seen := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	three := 3
	want := tracking.Source{TitleID: 1, Season: 1, Episode: 2, Kind: "alpha", Ref: "alpha:1", Provider: "a1", Quality: "1080p",
		Labels: []string{"HDR"}, Meta: tracking.SourceMeta{SourceID: "s-1", Voice: "Студия", Lang: "ru", Audio: "MVO",
			Studios: []string{"Студия"}, Subs: []string{"en"}, FileIndex: &three}, FirstSeen: seen, LastSeen: seen}
	if !reflect.DeepEqual(srcs[1], want) {
		t.Fatalf("source %+v, want %+v", srcs[1], want)
	}
	if back := coreadapt.ToSource(coreadapt.FromSource(srcs[1])); !reflect.DeepEqual(back, want) {
		t.Fatalf("round trip %+v", back)
	}
	if srcs, err := tr.Sources(ctx, 9); err != nil || len(srcs) != 0 {
		t.Fatalf("unknown title: %+v %v", srcs, err)
	}
}

func TestTrackingModels(t *testing.T) {
	ti := coreadapt.ToTitle(addonkit.TrackingTitle{ID: 1, Kind: "series", TMDBID: 9, IMDbID: "tt1", Name: "Сериал", Poster: "/p.jpg",
		Status: "airing", Cert: "16+"})
	if ti != (tracking.Title{ID: 1, Kind: "series", TMDBID: 9, IMDbID: "tt1", Name: "Сериал", Poster: "/p.jpg", Status: "airing", Cert: "16+"}) {
		t.Fatalf("title %+v", ti)
	}
	eps := coreadapt.ToEpisodes([]addonkit.TrackingEpisode{{TitleID: 1, Season: 1, Number: 2, Name: "Вторая", AirDate: "2026-01-02"}})
	if len(eps) != 1 || eps[0] != (tracking.Episode{TitleID: 1, Season: 1, Number: 2, Name: "Вторая", AirDate: "2026-01-02"}) {
		t.Fatalf("episodes %+v", eps)
	}
}
