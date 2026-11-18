package online

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"
)

func TestParseLevels(t *testing.T) {
	rc := RefContext{Kind: "movie", TMDBID: 438631, Source: "alpha"}

	t.Run("movie", func(t *testing.T) {
		lv, err := parseLevel(fixture(t, "movie-alpha.json"), rc)
		if err != nil {
			t.Fatal(err)
		}
		if lv.Level != "movie" || len(lv.Files) != 2 {
			t.Fatalf("%+v", lv)
		}
		f := lv.Files[0]
		if f.Name != "Дубляж" || f.Voice != "Дубляж" || !reflect.DeepEqual(f.Qualities, []string{"1080p", "720p"}) || f.Subtitles != 1 || f.Unsupported {
			t.Fatalf("file 0: %+v", f)
		}
		// the picker names the subtitles: label as given, language from it
		if !reflect.DeepEqual(f.SubtitleTracks, []SubInfo{{Label: "Русские", Lang: "ru"}}) || lv.Files[1].SubtitleTracks != nil {
			t.Fatalf("subtitle tracks: %+v / %+v", f.SubtitleTracks, lv.Files[1].SubtitleTracks)
		}
		// the « ~ 1080p» suffix is cut into the quality hint
		g := lv.Files[1]
		if g.Name != "Оригинал" || g.MaxQuality != "1080p" || !g.Unsupported {
			t.Fatalf("file 1: %+v", g)
		}
		p := lv.play[0]
		if p.Method != "play" || p.Headers["Referer"] != "https://player.example/" || len(p.Subtitles) != 1 {
			t.Fatalf("play 0: %+v", p)
		}
		// " or " fallbacks are kept in order
		if !reflect.DeepEqual(p.Qualities[0], QualityURLs{Name: "1080p", URLs: []string{"https://cdn.example/a/1080.m3u8", "https://cdn2.example/a/1080.m3u8"}}) {
			t.Fatalf("1080p: %+v", p.Qualities[0])
		}
	})

	t.Run("seasons", func(t *testing.T) {
		lv, err := parseLevel(fixture(t, "season-beta.json"), RefContext{Kind: "series", TMDBID: 1, Source: "beta"})
		if err != nil {
			t.Fatal(err)
		}
		if lv.Level != "seasons" || len(lv.Seasons) != 2 || lv.Seasons[0].Number != 1 || lv.Seasons[1].Number != 2 || lv.Seasons[1].Name != "2 сезон" {
			t.Fatalf("%+v", lv)
		}
		if lv.Seasons[0].path != "http://lampac.test:9118/lite/beta?rjson=False&kinopoisk_id=123&s=1" {
			t.Fatalf("path %q", lv.Seasons[0].path)
		}
	})

	t.Run("episodes", func(t *testing.T) {
		lv, err := parseLevel(fixture(t, "episode-beta-s1.json"), RefContext{Kind: "series", TMDBID: 1, Source: "beta", Season: 1})
		if err != nil {
			t.Fatal(err)
		}
		if lv.Level != "episodes" || lv.Season != 1 || len(lv.Episodes) != 2 || len(lv.Voices) != 2 {
			t.Fatalf("%+v", lv)
		}
		if e := lv.Episodes[1]; e.Season != 1 || e.Episode != 2 || e.Name != "Серия 2" {
			t.Fatalf("%+v", e)
		}
		if !lv.Voices[0].Active || lv.Voices[0].Name != "Дубляж" || lv.Voices[1].Active {
			t.Fatalf("voices %+v", lv.Voices)
		}
		// a call file keeps method=call
		if lv.play[0].Method != "call" || lv.play[0].URLs[0] != "http://lampac.test:9118/lite/beta/video?t=1&s=1&e=1&token=abc" {
			t.Fatalf("%+v", lv.play[0])
		}
	})

	t.Run("similar", func(t *testing.T) {
		lv, err := parseLevel(fixture(t, "similar-gamma.json"), rc)
		if err != nil {
			t.Fatal(err)
		}
		if lv.Level != "pick" || len(lv.Candidates) != 3 || lv.Candidates[1].Year != 1984 || lv.Candidates[2].Details != "сериал" {
			t.Fatalf("%+v", lv)
		}
	})

	t.Run("call", func(t *testing.T) {
		p, err := parseVideo(fixture(t, "call-alpha.json"))
		if err != nil {
			t.Fatal(err)
		}
		if p.Method != "play" || p.URLs[0] != "http://lampac.test:9118/proxy/tok1" || len(p.Subtitles) != 1 || p.Headers["Referer"] == "" {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("quality as voice", func(t *testing.T) {
		lv, err := parseLevel([]byte(`{"type":"movie","data":[{"method":"play","url":"https://cdn.example/x.m3u8","translate":"720p","title":"Альфа"}]}`), rc)
		if err != nil || lv.Files[0].Voice != "" || lv.Files[0].Name != "Альфа" {
			t.Fatalf("%+v %v", lv, err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, err := parseLevel([]byte(`{"type":"movie","data":[]}`), rc); !errors.Is(err, ErrNothing) {
			t.Fatalf("%v", err)
		}
		if _, err := parseLevel([]byte(`{"type":"weird","data":[{}]}`), rc); !errors.Is(err, ErrSourceFailed) {
			t.Fatalf("%v", err)
		}
	})
}

func TestSplitURLs(t *testing.T) {
	cases := map[string][]string{
		"https://cdn.example/a.m3u8":                                  {"https://cdn.example/a.m3u8"},
		"https://cdn.example/a.mp4 or https://cdn2.example/a.mp4":     {"https://cdn.example/a.mp4", "https://cdn2.example/a.mp4"},
		"https://cdn.example/a.mp4 https://cdn2.example/b.mp4":        {"https://cdn.example/a.mp4", "https://cdn2.example/b.mp4"},
		"https://cdn.example/a.mp4\x01https://cdn2.example/b.mp4":     {"https://cdn.example/a.mp4", "https://cdn2.example/b.mp4"},
		" https://cdn.example/a.mp4\r\n":                              {"https://cdn.example/a.mp4"},
		"javascript:alert(1) or ftp://cdn.example/x or //cdn.example": nil,
		"": nil,
	}
	for in, want := range cases {
		if got := splitURLs(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestQualityOrder(t *testing.T) {
	got := sortQualities([]string{"360p", "auto", "2160p", "720p", "1440p", "480p", "1080p", "240p"})
	want := []string{"2160p", "1440p", "1080p", "720p", "480p", "360p", "240p", "auto"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestCutQuality(t *testing.T) {
	for in, want := range map[string][2]string{
		"alpha - 1080p": {"alpha", "1080p"},
		"Beta ~ 720p":   {"Beta", "720p"},
		"gamma (ENG)":   {"gamma (ENG)", ""},
		"Альфа - фильм": {"Альфа - фильм", ""},
	} {
		n, q := cutQuality(in)
		if n != want[0] || q != want[1] {
			t.Errorf("%q: %q %q", in, n, q)
		}
	}
}

// A file ref finds its entry by identity in a re-fetched level: the
// position is only a hint.
func TestFileRefSurvivesReorderAndRefusesChange(t *testing.T) {
	rc := RefContext{Kind: "movie", TMDBID: 1, Source: "alpha"}
	mk := func(entries ...string) []byte {
		return []byte(`{"type":"movie","data":[` + strings.Join(entries, ",") + `]}`)
	}
	a := `{"method":"play","url":"https://cdn.example/a.m3u8","quality":{"1080p":"https://cdn.example/a.m3u8"},"translate":"Дубляж"}`
	b := `{"method":"play","url":"https://cdn.example/b.m3u8","quality":{"720p":"https://cdn.example/b.m3u8"},"translate":"Оригинал"}`
	b2 := `{"method":"play","url":"https://cdn.example/b2.m3u8","quality":{"720p":"https://cdn.example/b2.m3u8"},"translate":"Оригинал"}`
	c := `{"method":"play","url":"https://cdn.example/c.m3u8","translate":"Многоголосый"}`
	first, err := parseLevel(mk(a, b), rc)
	if err != nil {
		t.Fatal(err)
	}
	ref := rc
	ref.File, ref.ID, ref.L, ref.Voice = 2, first.ids[1], first.loose[1], "Оригинал"

	// fresh URLs, the same entry
	if p, n, err := first.PlayItemFor(ref); err != nil || n != 2 || p.URLs[0] != "https://cdn.example/b.m3u8" {
		t.Fatalf("%+v %d %v", p, n, err)
	}
	re, _ := parseLevel(mk(b2, c, a), ref) // reordered, one inserted, new signed URLs
	if p, n, err := re.PlayItemFor(ref); err != nil || n != 1 || p.URLs[0] != "https://cdn.example/b2.m3u8" {
		t.Fatalf("reordered: %+v %d %v", p, n, err)
	}
	if lv, err := only(re, ref); err != nil || len(lv.Files) != 1 || lv.Files[0].Name != "Оригинал" {
		t.Fatalf("only: %+v %v", lv, err)
	}
	// its qualities changed (a re-play after a stale link): still that entry
	requal, _ := parseLevel(mk(a, strings.Replace(b, `"720p"`, `"480p"`, 1)), ref)
	if p, n, err := requal.PlayItemFor(ref); err != nil || n != 2 || p.Qualities[0].Name != "480p" {
		t.Fatalf("other qualities: %+v %d %v", p, n, err)
	}
	// a ref minted before the loose identity: the exact one only
	old := ref
	old.L = ""
	if _, _, err := requal.PlayItemFor(old); !errors.Is(err, ErrNothing) {
		t.Fatalf("old ref: %v", err)
	}
	// the entry is gone (or another one): nothing, never the entry now at 2
	for _, changed := range [][]byte{mk(a, c), mk(a, strings.Replace(b, `"Оригинал"`, `"Субтитры"`, 1))} {
		lv, _ := parseLevel(changed, ref)
		if p, _, err := lv.PlayItemFor(ref); !errors.Is(err, ErrNothing) {
			t.Fatalf("changed list: %+v %v", p, err)
		}
		if _, err := only(lv, ref); !errors.Is(err, ErrNothing) {
			t.Fatalf("only on a changed list: %v", err)
		}
	}
	// episodes: the same s/e in another place is found; another episode is not
	ep := func(e int, name string) string {
		return `{"method":"call","url":"http://lampac.test:9118/lite/beta/video?e=` + strconv.Itoa(e) + `","s":1,"e":` + strconv.Itoa(e) + `,"name":"` + name + `"}`
	}
	erc := RefContext{Kind: "series", TMDBID: 1, Season: 1, Voice: "Дубляж"}
	eps, _ := parseLevel([]byte(`{"type":"episode","data":[`+ep(1, "Серия 1")+`,`+ep(2, "Серия 2")+`]}`), erc)
	erc.File, erc.Episode, erc.ID, erc.L = 2, 2, eps.ids[1], eps.loose[1]
	shifted, _ := parseLevel([]byte(`{"type":"episode","data":[`+ep(0, "0 серия")+`,`+ep(1, "Серия 1")+`,`+ep(2, "Серия 2")+`]}`), erc)
	if p, n, err := shifted.PlayItemFor(erc); err != nil || n != 3 || !strings.HasSuffix(p.URLs[0], "e=2") {
		t.Fatalf("shifted episodes: %+v %d %v", p, n, err)
	}
}

func TestCarriedHeaders(t *testing.T) {
	lv, err := parseLevel([]byte(`{"type":"movie","data":[{"method":"play","url":"https://cdn.example/a.m3u8","translate":"x","headers":{
		"referer":"https://player.example/","Origin":"https://player.example","Host":"192.168.1.1","Authorization":"Bearer t",
		"X-Forwarded-For":"10.0.0.1","Connection":"close","User-Agent":"a\r\nX: y","Cookie":"a=b","Accept":7}}]}`), RefContext{Kind: "movie", TMDBID: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Referer": "https://player.example/", "Origin": "https://player.example", "Cookie": "a=b"}
	if got := lv.play[0].Headers; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

// TestLocalizeEpisodes: a made-up episode name follows the request's
// language; a name the source gave stays.
func TestLocalizeEpisodes(t *testing.T) {
	eps := []Episode{{Episode: 3, Name: "Серия 3"}, {Episode: 4, Name: "Пилот"}}
	localizeEpisodes(i18n.WithLanguage(context.Background(), i18n.EN), eps)
	if eps[0].Name != "Episode 3" || eps[1].Name != "Пилот" {
		t.Fatalf("%+v", eps)
	}
	ru := []Episode{{Episode: 3, Name: "Серия 3"}}
	localizeEpisodes(context.Background(), ru)
	if ru[0].Name != "Серия 3" {
		t.Fatalf("%+v", ru)
	}
}
