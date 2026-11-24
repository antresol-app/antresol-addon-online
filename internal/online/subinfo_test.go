package online

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSubs(t *testing.T) {
	want := []Sub{{Label: "English", URL: "https://cdn.example/en.vtt"}, {Label: "Русские", URL: "https://cdn.example/ru.srt"}}
	for _, b := range []string{
		`[{"label":"English","url":"https://cdn.example/en.vtt"},{"label":" Русские ","url":"https://cdn.example/ru.srt or https://cdn2.example/ru.srt"},{"label":"bad","url":"javascript:x"}]`,
		`{"subtitles":[{"label":"English","url":"https://cdn.example/en.vtt"},{"label":"Русские","url":"https://cdn.example/ru.srt"}]}`,
		`{"data":[{"label":"English","url":"https://cdn.example/en.vtt"},{"label":"Русские","url":"https://cdn.example/ru.srt"}]}`,
	} {
		if got := parseSubs([]byte(b)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v", b, got)
		}
	}
	for _, b := range []string{`{}`, `"x"`, `[1,2]`, `{"subtitles":"no"}`} {
		if got := parseSubs([]byte(b)); len(got) != 0 {
			t.Fatalf("%s: %+v", b, got)
		}
	}
}

func TestEpisodeSubtitleTracks(t *testing.T) {
	b := `{"type":"episode","data":[{"method":"play","url":"https://cdn.example/1.m3u8","s":1,"e":1,` +
		`"subtitles":[{"label":"Eng (Forced)","url":"https://cdn.example/1.en.vtt"},{"label":"Надписи","url":"https://cdn.example/1.x.vtt"}]},` +
		`{"method":"play","url":"https://cdn.example/2.m3u8","s":1,"e":2}]}`
	lv, err := parseLevel([]byte(b), RefContext{Kind: "series", TMDBID: 1, Source: "alpha", Season: 1})
	if err != nil || len(lv.Episodes) != 2 {
		t.Fatalf("%+v %v", lv, err)
	}
	if e := lv.Episodes[0]; e.Subtitles != 2 || !reflect.DeepEqual(e.SubtitleTracks, []SubInfo{{Label: "Eng (Forced)", Lang: "en"}, {Label: "Надписи"}}) {
		t.Fatalf("episode 1: %+v", e)
	}
	if e := lv.Episodes[1]; e.Subtitles != 0 || e.SubtitleTracks != nil {
		t.Fatalf("episode 2: %+v", e)
	}
}

// A label that names no language (Kinobase's player: «[1]…/rus.vtt») takes
// it from the file name; a name with no language tag still has none, and a
// label's language wins over the URL's.
func TestSubtitleTrackLangFromURL(t *testing.T) {
	got := subInfos([]Sub{
		{Label: "1", URL: "https://cdn.example/subs/rus.vtt"},
		{Label: "Субтитры", URL: "https://cdn.example/s/Film.2025.en.srt?t=1"},
		{Label: "", URL: "https://cdn.example/s/123_ukr.vtt"},
		{Label: "2", URL: "https://cdn.example/s/a1b2c3.vtt"},
		{Label: "English", URL: "https://cdn.example/s/ru.vtt"},
	})
	want := []SubInfo{{Label: "1", Lang: "ru"}, {Label: "Субтитры", Lang: "en"}, {Label: "", Lang: "uk"}, {Label: "2"}, {Label: "English", Lang: "en"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if langs := SubLangsOf(got); !reflect.DeepEqual(langs, []string{"RU", "EN", "UK"}) {
		t.Fatalf("langs %v", langs)
	}
}

// A file whose subtitles are behind a subtitles_call gets them at play:
// the call is asked beside the probes, through Lampac, once (cached); a
// failing or slow one plays without them.
func TestResolveAsksSubtitlesCall(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("\x00\x00\x00\x18ftypmp42" + strings.Repeat("m", 600)))
	}))
	defer cdn.Close()
	port := portOf(cdn)
	var calls atomic.Int32
	var mode atomic.Value
	mode.Store("ok")
	level := `{"type":"movie","data":[{"method":"play","translate":"Дубляж","url":"http://cdn.example:` + port + `/a/1080.mp4",` +
		`"subtitles_call":"http://lampac.test:9118/lite/alpha/subtitles?id=1&uid=secret"}]}`
	s := fakeService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lite/alpha/subtitles" {
			calls.Add(1)
			switch mode.Load() {
			case "fail":
				http.Error(w, "no", http.StatusBadGateway)
				return
			case "slow":
				time.Sleep(300 * time.Millisecond)
			}
			w.Write([]byte(`[{"label":"English","url":"https://cdn.example/en.vtt"},{"label":"Русские","url":"https://cdn.example/ru.srt"}]`))
			return
		}
		w.Write([]byte(level))
	}), dune)
	s.Upstream = testNet(t, "lampac.test:9118", []string{"cdn.example"}, cdn.Listener.Addr().String())
	ctx := context.Background()
	lv, err := s.Browse(ctx, adult(), s.Refs.Mint("/lite/alpha?id=1", RefContext{Kind: "movie", TMDBID: 1, Source: "alpha"}))
	if err != nil || len(lv.Files) != 1 || lv.Files[0].Subtitles != 0 {
		t.Fatalf("%+v %v", lv, err)
	}
	ref := lv.Files[0].Ref
	want := []Sub{{Label: "English", URL: "https://cdn.example/en.vtt"}, {Label: "Русские", URL: "https://cdn.example/ru.srt"}}
	for i := 0; i < 2; i++ {
		r, err := s.Resolve(ctx, adult(), ref, "", false)
		if err != nil || !reflect.DeepEqual(r.Subtitles, want) {
			t.Fatalf("play %d: %+v %v", i, r.Subtitles, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("subtitles calls: %d", n)
	}

	// past the caches: a failing call plays without subtitles
	mode.Store("fail")
	r, err := s.Resolve(ctx, adult(), ref, "", true)
	if err != nil || len(r.Subtitles) != 0 {
		t.Fatalf("failing call: %+v %v", r.Subtitles, err)
	}
	// a slow one is not waited for past subsCallWait
	old := subsCallWait
	subsCallWait = 20 * time.Millisecond
	t.Cleanup(func() { subsCallWait = old })
	mode.Store("slow")
	start := time.Now()
	r, err = s.Resolve(ctx, adult(), ref, "", true)
	if err != nil || len(r.Subtitles) != 0 || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("slow call: %+v %v after %v", r.Subtitles, err, time.Since(start))
	}
}
