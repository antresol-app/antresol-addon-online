package online

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
)

// fakeOnline answers Sources with srcs and Browse from levels by ref.
type fakeOnline struct {
	srcs    []Source
	levels  map[string]Level
	browsed []string
	users   []User
}

func (f *fakeOnline) Sources(_ context.Context, u User, kind string, tmdbID int) ([]Source, error) {
	f.users = append(f.users, u)
	return f.srcs, nil
}

func (f *fakeOnline) Browse(_ context.Context, u User, ref string) (Level, error) {
	f.browsed = append(f.browsed, ref)
	lv, ok := f.levels[ref]
	if !ok {
		return Level{}, fmt.Errorf("no level %s", ref)
	}
	return lv, nil
}

// episodes is an episodes level of season 2 in voice v: episodes 1–2,
// and 3 unsupported.
func episodes(v string, voices []Choice) Level {
	return Level{Level: "episodes", Season: 2, Voices: voices, Episodes: []Episode{
		{Season: 2, Episode: 1, Ref: v + "/e1", MaxQuality: "1080p"},
		{Season: 2, Episode: 2, Ref: v + "/e2", MaxQuality: "720p"},
		{Season: 2, Episode: 3, Ref: v + "/e3", Unsupported: true},
	}}
}

func seriesEps() []tracking.Episode {
	return []tracking.Episode{{Season: 1, Number: 1}, {Season: 2, Number: 1}, {Season: 2, Number: 2}, {Season: 2, Number: 3}}
}

func newOnlineScanner(f *fakeOnline, maxVoices int) *onlineScanner {
	sc := NewTrackingScanner(nil, nil, User{ID: -7, Name: "tracking"}, maxVoices).(*onlineScanner)
	sc.api, sc.pause = f, 0
	return sc
}

// fakeStore holds a title's previous rows.
type fakeStore struct{ rows []tracking.Source }

func (f *fakeStore) Sources(context.Context, int64) ([]tracking.Source, error) { return f.rows, nil }

func srcKeys(srcs []tracking.Source) string {
	var out []string
	for _, s := range srcs {
		out = append(out, fmt.Sprintf("S%dE%d %s %s %s %v", s.Season, s.Episode, s.Provider, s.Ref, s.Quality, s.Labels))
	}
	return strings.Join(out, "; ")
}

func TestOnlineScannerOneSourcePerVoice(t *testing.T) {
	voices := []Choice{{Name: "Дубляж", Active: true, Ref: "bal/seasons/dub"}, {Name: "Многоголосый", Ref: "bal/seasons/mvo"}}
	f := &fakeOnline{
		srcs: []Source{{ID: "bal", Name: "Bal", Ref: "bal"}},
		levels: map[string]Level{
			// the voices are on the seasons level: each voice has its seasons
			"bal":             {Level: "seasons", Voices: voices, Seasons: []Season{{Number: 1, Ref: "dub/s1"}, {Number: 2, Ref: "dub/s2"}}},
			"bal/seasons/mvo": {Level: "seasons", Voices: voices, Seasons: []Season{{Number: 1, Ref: "mvo/s1"}, {Number: 2, Ref: "mvo/s2"}}},
			"dub/s2":          episodes("dub", nil),
			"mvo/s2":          episodes("mvo", nil),
		},
	}
	sc := newOnlineScanner(f, 0)
	if sc.Kind() != "online" {
		t.Fatal(sc.Kind())
	}
	got, err := sc.Scan(context.Background(), tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, seriesEps())
	if err != nil {
		t.Fatal(err)
	}
	want := "S2E1 Bal dub/e1 1080p [Дубляж]; S2E2 Bal dub/e2 720p [Дубляж]; " +
		"S2E1 Bal mvo/e1 1080p [Многоголосый]; S2E2 Bal mvo/e2 720p [Многоголосый]"
	if srcKeys(got) != want {
		t.Fatalf("sources\n got %s\nwant %s", srcKeys(got), want)
	}
	for _, s := range got {
		if s.Kind != "online" || s.TitleID != 1 {
			t.Fatalf("source %+v", s)
		}
	}
	// the latest season only; the active voice's level is not asked again
	if strings.Join(f.browsed, ",") != "bal,dub/s2,bal/seasons/mvo,mvo/s2" {
		t.Fatalf("browsed %v", f.browsed)
	}
	if len(f.users) != 1 || f.users[0].IsKid || f.users[0].ID != -7 {
		t.Fatalf("users %+v", f.users)
	}
}

func TestOnlineScannerVoicesOnEpisodesLevel(t *testing.T) {
	voices := []Choice{{Name: "A", Ref: "a/s2"}, {Name: "B", Active: true, Ref: "b/s2"}}
	f := &fakeOnline{
		srcs: []Source{{ID: "bal", Name: "Bal", Ref: "bal"}},
		levels: map[string]Level{
			"bal":  {Level: "seasons", Seasons: []Season{{Number: 2, Ref: "b/s2"}, {Number: 1, Ref: "b/s1"}}},
			"b/s2": episodes("b", voices),
			"a/s2": episodes("a", voices),
		},
	}
	got, err := newOnlineScanner(f, 0).Scan(context.Background(), tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, seriesEps())
	if err != nil {
		t.Fatal(err)
	}
	want := "S2E1 Bal b/e1 1080p [B]; S2E2 Bal b/e2 720p [B]; S2E1 Bal a/e1 1080p [A]; S2E2 Bal a/e2 720p [A]"
	if srcKeys(got) != want {
		t.Fatalf("sources\n got %s\nwant %s", srcKeys(got), want)
	}
}

func TestOnlineScannerCapsVoices(t *testing.T) {
	var voices []Choice
	levels := map[string]Level{}
	for i := range 9 {
		v := fmt.Sprintf("v%d", i)
		voices = append(voices, Choice{Name: v, Active: i == 0, Ref: v})
	}
	for _, c := range voices {
		levels[c.Ref] = episodes(c.Ref, voices)
	}
	levels["bal"] = levels["v0"]
	f := &fakeOnline{srcs: []Source{{ID: "bal", Name: "Bal", Ref: "bal"}}, levels: levels}
	got, err := newOnlineScanner(f, 0).Scan(context.Background(), tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, seriesEps())
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, s := range got {
		labels[s.Labels[0]] = true
	}
	if len(labels) != 6 || len(got) != 12 || len(f.browsed) != 6 {
		t.Fatalf("voices %v, %d sources, browsed %v", labels, len(got), f.browsed)
	}
	f.browsed = nil
	if got, _ := newOnlineScanner(f, 2).Scan(context.Background(), tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, seriesEps()); len(got) != 4 {
		t.Fatalf("maxVoices 2: %s", srcKeys(got))
	}
}

func TestOnlineScannerMoviePerVoice(t *testing.T) {
	f := &fakeOnline{
		srcs: []Source{{ID: "a", Name: "A", Ref: "a"}, {ID: "b", Name: "B", Ref: "b"}, {ID: "c", Name: "C", Ref: "c"}},
		levels: map[string]Level{
			"a": {Level: "movie", Files: []File{
				{Voice: "Дубляж", Ref: "a/1", MaxQuality: "1080p"}, {Voice: "Дубляж", Ref: "a/2", MaxQuality: "720p"},
				{Voice: "Оригинал", Ref: "a/3", Unsupported: true}, {Voice: "Оригинал", Ref: "a/4", MaxQuality: "480p"}}},
			"b": {Level: "movie", Files: []File{{Ref: "b/1", MaxQuality: "2160p"}}},
			// c fails: the others still count
		},
	}
	got, err := newOnlineScanner(f, 0).Scan(context.Background(), tracking.Title{ID: 1, Kind: "movie", TMDBID: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "S0E0 A a/1 1080p [Дубляж]; S0E0 A a/4 480p [Оригинал]; S0E0 B b/1 2160p []"
	if srcKeys(got) != want {
		t.Fatalf("sources\n got %s\nwant %s", srcKeys(got), want)
	}
}

func TestOnlineScannerAllFailIsError(t *testing.T) {
	f := &fakeOnline{srcs: []Source{{ID: "a", Name: "A", Ref: "a"}}, levels: map[string]Level{}}
	if _, err := newOnlineScanner(f, 0).Scan(context.Background(), tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, seriesEps()); err == nil {
		t.Fatal("no error when every source failed")
	}
	// no TMDB id: nothing to ask
	if got, err := newOnlineScanner(f, 0).Scan(context.Background(), tracking.Title{ID: 1, Kind: "series"}, nil); err != nil || got != nil {
		t.Fatal(got, err)
	}
}

var onlineNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// twoBalancers: A gives season 2 (episodes 1–2), B fails.
func twoBalancers() *fakeOnline {
	return &fakeOnline{
		srcs: []Source{{ID: "a", Name: "A", Ref: "a"}, {ID: "b", Name: "B", Ref: "b"}},
		levels: map[string]Level{
			"a":    {Level: "seasons", Seasons: []Season{{Number: 1, Ref: "a/s1"}, {Number: 2, Ref: "a/s2"}}},
			"a/s2": episodes("a", nil),
		},
	}
}

func row(provider string, season, ep int, ref string, seen time.Time) tracking.Source {
	return tracking.Source{TitleID: 1, Season: season, Episode: ep, Kind: "online", Ref: ref, Provider: provider, LastSeen: seen}
}

func TestOnlineScannerKeepsFailedBalancerRows(t *testing.T) {
	f := twoBalancers()
	sc := newOnlineScanner(f, 0)
	old := onlineNow.Add(-5 * time.Hour)
	sc.store = &fakeStore{rows: []tracking.Source{
		row("A", 2, 1, "a/old", old),     // A gave season 2 again: dropped
		row("B", 2, 1, "b/e1", old),      // B failed: kept
		row("Gone", 2, 1, "gone/1", old), // no such balancer any more: dropped
		{TitleID: 1, Season: 2, Episode: 1, Kind: "torrent", Ref: "t", LastSeen: old},
	}}
	sc.now = func() time.Time { return onlineNow }
	got, err := sc.Scan(context.Background(), tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, seriesEps())
	if err != nil {
		t.Fatal(err)
	}
	want := "S2E1 A a/e1 1080p []; S2E2 A a/e2 720p []; S2E1 B b/e1  []"
	if srcKeys(got) != want {
		t.Fatalf("sources\n got %s\nwant %s", srcKeys(got), want)
	}
	// every balancer failing fails the scan (Refresh keeps the rows)
	f.levels = map[string]Level{}
	sc.last = map[int64]time.Time{}
	if _, err := sc.Scan(context.Background(), tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, seriesEps()); err == nil {
		t.Fatal("no error when every balancer failed")
	}
}

func TestOnlineScannerCadence(t *testing.T) {
	ti := tracking.Title{ID: 1, Kind: "series", TMDBID: 5}
	for name, c := range map[string]struct {
		seen    time.Duration // the rows' last_seen before now
		aired   string        // the last episode's air date
		scanned bool
	}{
		"recent":               {seen: time.Hour, aired: "2026-10-01"},
		"after 4h":             {seen: 5 * time.Hour, aired: "2026-10-01", scanned: true},
		"aired yesterday":      {seen: time.Hour, aired: "2026-11-12", scanned: true},
		"aired 2 days ago":     {seen: time.Hour, aired: "2026-11-11", scanned: true},
		"aired 3 days ago":     {seen: time.Hour, aired: "2026-11-10"},
		"airs later":           {seen: time.Hour, aired: "2026-11-20"},
		"never scanned, ended": {aired: "2026-10-01", scanned: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := twoBalancers()
			sc := newOnlineScanner(f, 0)
			sc.now = func() time.Time { return onlineNow }
			st := &fakeStore{}
			if c.seen > 0 {
				st.rows = []tracking.Source{row("A", 2, 1, "a/e1", onlineNow.Add(-c.seen))}
			}
			sc.store = st
			eps := append(seriesEps(), tracking.Episode{Season: 2, Number: 4, AirDate: c.aired})
			_, err := sc.Scan(context.Background(), ti, eps)
			if c.scanned != (err == nil) || (!c.scanned && !errors.Is(err, ErrUnchanged)) || c.scanned != (len(f.users) == 1) {
				t.Fatalf("err %v, sources asked %d", err, len(f.users))
			}
		})
	}
	// scanned by this process: not again within 4 h, though it found nothing
	f := &fakeOnline{levels: map[string]Level{}}
	sc := newOnlineScanner(f, 0)
	sc.store, sc.now = &fakeStore{}, func() time.Time { return onlineNow }
	if _, err := sc.Scan(context.Background(), ti, seriesEps()); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Scan(context.Background(), ti, seriesEps()); !errors.Is(err, ErrUnchanged) || len(f.users) != 1 {
		t.Fatalf("err %v, sources asked %d", err, len(f.users))
	}
}

func TestVoiceLang(t *testing.T) {
	for _, c := range []struct {
		voice, lang string
		subs        []string
	}{
		{"AniLibria", "ru", nil}, {"Оригинал", "", nil}, {"Original", "", nil},
		{"ENG", "en", nil}, {"Eng Dub", "en", nil}, {"English", "en", nil},
		{"Субтитры", "", []string{"ru"}}, {"SUB Ru", "", []string{"ru"}},
	} {
		if l, s := voiceLang(c.voice); l != c.lang || fmt.Sprint(s) != fmt.Sprint(c.subs) {
			t.Errorf("%q: %q %v", c.voice, l, s)
		}
	}
}

func TestScannerKeepsOlderSeasons(t *testing.T) {
	ctx := context.Background()
	eps := []tracking.Episode{{Season: 1, Number: 1, AirDate: "2025-01-01"}, {Season: 2, Number: 1, AirDate: "2026-10-01"},
		{Season: 2, Number: 2, AirDate: "2026-10-08"}}

	// A browses season 2 only; its season-1 row stays
	f := twoBalancers()
	f.srcs = f.srcs[:1]
	sc := newOnlineScanner(f, 0)
	old := onlineNow.Add(-5 * time.Hour)
	sc.store = &fakeStore{rows: []tracking.Source{row("A", 1, 1, "a/s1e1", old), row("A", 2, 2, "a/gone", old)}}
	sc.now = func() time.Time { return onlineNow }
	got, err := sc.Scan(ctx, tracking.Title{ID: 1, Kind: "series", TMDBID: 5}, eps)
	if err != nil {
		t.Fatal(err)
	}
	if want := "S2E1 A a/e1 1080p []; S2E2 A a/e2 720p []; S1E1 A a/s1e1  []"; srcKeys(got) != want {
		t.Fatalf("keeps\n got %s\nwant %s", srcKeys(got), want)
	}
}
func TestOnlineRowsCarryProviderIDAndLang(t *testing.T) {
	f := &fakeOnline{
		srcs: []Source{{ID: "bal", Name: "Bal", Ref: "bal"}},
		levels: map[string]Level{
			"bal": {Level: "movie", Files: []File{
				{Voice: "AniLibria", Ref: "a", MaxQuality: "1080p"}, {Voice: "Оригинал", Ref: "b", MaxQuality: "1080p"},
				{Voice: "SUB Ru", Ref: "c", MaxQuality: "1080p"}, {Voice: "English", Ref: "d", MaxQuality: "1080p"}}},
		},
	}
	got, err := newOnlineScanner(f, 0).Scan(context.Background(), tracking.Title{ID: 1, Kind: "movie", TMDBID: 5}, nil)
	if err != nil || len(got) != 4 {
		t.Fatalf("%+v %v", got, err)
	}
	for i, c := range []struct {
		voice, lang string
		subs        []string
	}{{"AniLibria", "ru", nil}, {"Оригинал", "", nil}, {"SUB Ru", "", []string{"ru"}}, {"English", "en", nil}} {
		s := got[i]
		if s.Provider != "Bal" || s.Meta.Provider != "bal" || s.Meta.Voice != c.voice || s.Meta.Lang != c.lang ||
			len(s.Meta.Subs) != len(c.subs) || (len(c.subs) == 1 && s.Meta.Subs[0] != c.subs[0]) {
			t.Fatalf("row %d: %+v", i, s)
		}
	}
}
