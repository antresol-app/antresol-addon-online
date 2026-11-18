package coreadapt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-online/internal/online"
	"github.com/antresol-app/antresol/addonkit"
)

// TitleLibrary finds a top-level library item by TMDB id
// (*addonkit.Client, permission library.read).
type TitleLibrary interface {
	ItemByTMDB(ctx context.Context, kind string, tmdbID int64) (addonkit.Item, bool, error)
}

// Titles is online.Service.Titles over the core: the TMDB title through
// the core's metadata pass-through (TMDB nil: the server has it off), the
// library's card where TMDB fails or is off, and the library's IMDb id
// where TMDB has none. It is the in-core onlineTitle, unchanged in its
// rules.
type Titles struct {
	TMDB *tmdb.Client
	Lib  TitleLibrary
}

// Title is the card and rating of a TMDB title (online.ErrUnknownTitle
// when neither TMDB nor the library has it).
func (t Titles) Title(ctx context.Context, kind string, id int) (online.TitleCtx, error) {
	it, inLib, libErr := t.Lib.ItemByTMDB(ctx, kind, int64(id))
	var libIMDb string
	if libErr == nil && inLib && len(it.Meta) > 0 {
		var m struct {
			IMDb string `json:"imdb_id"`
		}
		if json.Unmarshal(it.Meta, &m) == nil {
			libIMDb = m.IMDb
		}
	}
	if t.TMDB != nil {
		var d tmdb.Details
		var err error
		if kind == "movie" {
			d, err = t.TMDB.Movie(ctx, id)
		} else {
			d, err = t.TMDB.TV(ctx, id)
		}
		switch {
		case errors.Is(err, tmdb.ErrNotFound):
			return online.TitleCtx{}, online.ErrUnknownTitle
		case err == nil:
			tc := online.TitleCtx{Title: d.Title, OriginalTitle: d.OriginalTitle, OriginalLanguage: d.OriginalLanguage,
				Rating: d.Rating, IMDb: d.IMDbID, Year: d.Year, Serial: kind == "series", Poster: t.TMDB.ImageURL("w342", d.Poster)}
			if tc.Title == "" {
				tc.Title = tc.OriginalTitle
			}
			if tc.IMDb == "" {
				tc.IMDb = libIMDb
			}
			return tc, nil
		case libErr != nil || !inLib:
			return online.TitleCtx{}, err
		}
	}
	if libErr != nil {
		return online.TitleCtx{}, libErr
	}
	if !inLib {
		return online.TitleCtx{}, online.ErrUnknownTitle
	}
	// with TMDB on this is its failure: the card is kept briefly
	return online.TitleCtx{Title: it.Title, OriginalTitle: it.OriginalTitle, Rating: it.Rating, IMDb: libIMDb, Year: it.Year,
		Serial: kind == "series", Fallback: t.TMDB != nil}, nil
}

// probeTimeout bounds the probe of the metadata source.
const probeTimeout = 10 * time.Second

// TMDB is the copied TMDB client on the core's metadata pass-through
// (BaseURL = MetadataBase("tmdb"), HTTP = MetadataHTTP(), no key: the core
// adds its own), or nil when the server has TMDB off: HEAD of the source
// root is 404 then and 400 when on, without asking TMDB (contract,
// «Metadata pass-through»). Any other outcome keeps the client, so a core
// that is down shows on the first real call.
func TMDB(ctx context.Context, c *addonkit.Client) *tmdb.Client {
	hc := c.MetadataHTTP()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	tm := &tmdb.Client{BaseURL: c.MetadataBase("tmdb"), HTTP: hc}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.MetadataBase("tmdb"), nil)
	if err != nil {
		return tm
	}
	resp, err := hc.Do(req)
	if err != nil {
		slog.Warn("online: metadata probe", "source", "tmdb", "err", err)
		return tm
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return tm
}
