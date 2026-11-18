// Package coreadapt maps the core API (addonkit) onto the copied core
// models the online code works in: the tracked titles' sources and
// episodes. The library, the progress and the profiles the online code
// reads from *addonkit.Client directly (its interfaces take the SDK's
// types).
package coreadapt

import (
	"context"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/tracking"
	"github.com/antresol-app/antresol/addonkit"
)

// Tracking is the core's tracked titles through addonkit (permission
// tracking): the online scanner's previous rows (online.SourceStore).
type Tracking struct{ C *addonkit.Client }

// ToTitle is the copied model of an SDK tracked title (a tracking.scan
// hook's title).
func ToTitle(t addonkit.TrackingTitle) tracking.Title {
	return tracking.Title{ID: t.ID, Kind: t.Kind, TMDBID: t.TMDBID, IMDbID: t.IMDbID, Name: t.Name,
		Poster: t.Poster, Status: t.Status, Cert: t.Cert}
}

// ToEpisodes is the copied model of an SDK episode list.
func ToEpisodes(eps []addonkit.TrackingEpisode) []tracking.Episode {
	out := make([]tracking.Episode, len(eps))
	for i, e := range eps {
		out[i] = tracking.Episode{TitleID: e.TitleID, Season: e.Season, Number: e.Number, Name: e.Name, AirDate: e.AirDate}
	}
	return out
}

// ToSource is the copied model of an SDK source.
func ToSource(s addonkit.TrackingSource) tracking.Source {
	m := s.Meta
	return tracking.Source{TitleID: s.TitleID, Season: s.Season, Episode: s.Episode, Kind: s.Kind,
		Ref: s.Ref, Provider: s.Provider, Quality: s.Quality, Labels: s.Labels,
		Meta: tracking.SourceMeta{ItemID: m.ItemID, FileID: m.FileID, SourceID: m.SourceID, Provider: m.Provider,
			Voice: m.Voice, Lang: m.Lang, Audio: m.Audio, Studios: m.Studios, Subs: m.Subs, FileIndex: m.FileIndex},
		FirstSeen: s.FirstSeen, LastSeen: s.LastSeen}
}

// FromSource is the SDK form of a copied source (a scanner's answer).
func FromSource(s tracking.Source) addonkit.TrackingSource {
	m := s.Meta
	return addonkit.TrackingSource{TitleID: s.TitleID, Season: s.Season, Episode: s.Episode, Kind: s.Kind,
		Ref: s.Ref, Provider: s.Provider, Quality: s.Quality, Labels: s.Labels,
		Meta: addonkit.TrackingSourceMeta{ItemID: m.ItemID, FileID: m.FileID, SourceID: m.SourceID, Provider: m.Provider,
			Voice: m.Voice, Lang: m.Lang, Audio: m.Audio, Studios: m.Studios, Subs: m.Subs, FileIndex: m.FileIndex},
		FirstSeen: s.FirstSeen, LastSeen: s.LastSeen}
}

// Sources is every source of the title, of every kind; none for a title
// the core does not track.
func (t Tracking) Sources(ctx context.Context, id int64) ([]tracking.Source, error) {
	srcs, err := t.C.TitleSources(ctx, id)
	if addonkit.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]tracking.Source, len(srcs))
	for i, s := range srcs {
		out[i] = ToSource(s)
	}
	return out, nil
}
