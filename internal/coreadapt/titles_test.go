package coreadapt_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/antresol-app/antresol-addon-online/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-online/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// lib is a library of top-level items by kind and TMDB id.
type lib struct {
	items map[string]addonkit.Item
	err   error
}

func (l lib) ItemByTMDB(_ context.Context, kind string, id int64) (addonkit.Item, bool, error) {
	if l.err != nil {
		return addonkit.Item{}, false, l.err
	}
	it, ok := l.items[kind+":"+jsonInt(id)]
	return it, ok, nil
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

// fakeTMDB is the core's metadata pass-through for TMDB: movie 10 (RU 16+,
// with an IMDb id and a poster), tv 20 (no IMDb id); anything else 404;
// down makes every call 502.
func fakeTMDB(t *testing.T, down *bool) *tmdb.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/movie/10":
			w.Write([]byte(`{"id":10,"title":"Десять","original_title":"Ten","original_language":"en","release_date":"2019-05-01",
				"imdb_id":"tt10","poster_path":"/ten.jpg",
				"release_dates":{"results":[{"iso_3166_1":"RU","release_dates":[{"certification":"16+"}]}]}}`))
		case "/tv/20":
			w.Write([]byte(`{"id":20,"name":"","original_name":"Twenty","first_air_date":"2020-01-01",
				"content_ratings":{"results":[{"iso_3166_1":"RU","rating":"12+"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status_code":34}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &tmdb.Client{BaseURL: srv.URL, HTTP: srv.Client(), Backoff: 1}
}

// The title card and rating of «Смотреть онлайн» come from TMDB through
// the core, the library's IMDb id filling a gap; TMDB's 404 is an unknown
// title; a TMDB that fails falls back to the library's card (Fallback),
// and with TMDB off on the server the library's card is the answer.
func TestTitles_TMDBThenLibrary(t *testing.T) {
	down := false
	l := lib{items: map[string]addonkit.Item{
		"series:20": {Title: "Двадцать", Rating: "12+", Year: 2020, Meta: json.RawMessage(`{"imdb_id":"tt20"}`)},
		"movie:30":  {Title: "Тридцать", OriginalTitle: "Thirty", Rating: "18+", Year: 2001},
	}}
	ti := coreadapt.Titles{TMDB: fakeTMDB(t, &down), Lib: l}
	ctx := context.Background()
	m, err := ti.Title(ctx, "movie", 10)
	if err != nil || m.Title != "Десять" || m.OriginalTitle != "Ten" || m.OriginalLanguage != "en" || m.Rating != "16+" ||
		m.IMDb != "tt10" || m.Year != 2019 || m.Serial || m.Poster != tmdb.DefaultImageBase+"/w342/ten.jpg" || m.Fallback {
		t.Fatalf("movie %+v %v", m, err)
	}
	s, err := ti.Title(ctx, "series", 20)
	if err != nil || s.Title != "Twenty" || s.IMDb != "tt20" || s.Rating != "12+" || !s.Serial {
		t.Fatalf("series %+v %v", s, err)
	}
	if _, err := ti.Title(ctx, "movie", 99); !errors.Is(err, online.ErrUnknownTitle) {
		t.Fatalf("unknown: %v", err)
	}
	down = true
	f, err := ti.Title(ctx, "movie", 30)
	if err != nil || f.Title != "Тридцать" || f.Rating != "18+" || !f.Fallback {
		t.Fatalf("fallback %+v %v", f, err)
	}
	if _, err := ti.Title(ctx, "movie", 10); err == nil || errors.Is(err, online.ErrUnknownTitle) {
		t.Fatalf("TMDB down, not in the library: %v", err)
	}
	off := coreadapt.Titles{Lib: l}
	if f, err := off.Title(ctx, "movie", 30); err != nil || f.Title != "Тридцать" || f.Fallback {
		t.Fatalf("TMDB off %+v %v", f, err)
	}
	if _, err := off.Title(ctx, "movie", 10); !errors.Is(err, online.ErrUnknownTitle) {
		t.Fatalf("TMDB off, not in the library: %v", err)
	}
	broken := coreadapt.Titles{Lib: lib{err: errors.New("core down")}}
	if _, err := broken.Title(ctx, "movie", 30); err == nil || errors.Is(err, online.ErrUnknownTitle) {
		t.Fatalf("library down: %v", err)
	}
}

// TMDB is nil when the server has the source off (HEAD of its root 404),
// a client on the pass-through when on (400), and kept when the probe
// cannot tell (the core down).
func TestTMDB_ProbesTheSource(t *testing.T) {
	for code, want := range map[int]bool{http.StatusNotFound: false, http.StatusBadRequest: true, http.StatusInternalServerError: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodHead || r.URL.Path != "/api/addon-core/v1/metadata/tmdb" || r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("probe %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			w.WriteHeader(code)
		}))
		c := addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
		tm := coreadapt.TMDB(context.Background(), c)
		if (tm != nil) != want || (tm != nil && tm.BaseURL != c.MetadataBase("tmdb")) {
			t.Fatalf("%d: %+v", code, tm)
		}
		srv.Close()
	}
	c := addonkit.NewClient(addonkit.Env{CoreURL: "http://127.0.0.1:1", Token: "tok"})
	if coreadapt.TMDB(context.Background(), c) == nil {
		t.Fatal("core down: no client")
	}
}
