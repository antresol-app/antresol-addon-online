package online

import (
	"context"
	"errors"
	"slices"
)

// SourcePrefs is the admin's choice of Lampac's sources («Настройки»):
// Order lists the ids shown first, in that order (the others follow in
// Lampac's own order); Off the ids never offered.
type SourcePrefs struct {
	Order []string
	Off   []string
}

func (s *Service) prefs() SourcePrefs {
	if s.Prefs == nil {
		return SourcePrefs{}
	}
	return s.Prefs()
}

// sourceOff: the admin turned source id off.
func (s *Service) sourceOff(id string) bool { return slices.Contains(s.prefs().Off, id) }

// arrange drops the sources the admin turned off and puts the ones of
// the admin's order first, in it; a new list (in is the cache's).
func (p SourcePrefs) arrange(in []Source) []Source {
	out := make([]Source, 0, len(in))
	for _, src := range in {
		if !slices.Contains(p.Off, src.ID) {
			out = append(out, src)
		}
	}
	rank := func(id string) int {
		if i := slices.Index(p.Order, id); i >= 0 {
			return i
		}
		return len(p.Order)
	}
	slices.SortStableFunc(out, func(a, b Source) int { return rank(a.ID) - rank(b.ID) })
	return out
}

// LampacSource is one source Lampac has, for «Настройки».
type LampacSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Found: it found the probe title (a working source, as Lampac checks).
	Found bool `json:"found"`
}

// LampacSources lists the sources Lampac has (its /lite/events of the
// status' probe title, the ones a Lampa client is not needed for), in
// Lampac's order, whatever the admin's choice.
func (s *Service) LampacSources(ctx context.Context) ([]LampacSource, error) {
	if s == nil || s.L == nil {
		return nil, ErrOff
	}
	s.init()
	ctx, cancel := context.WithTimeout(ctx, budget(s.SourcesBudget, defaultSourcesBudget))
	defer cancel()
	t, err := s.title(ctx, "movie", probeTMDB)
	if err != nil {
		t = TitleCtx{}
	}
	evs, err := s.L.Events(ctx, s.card(ctx, "movie", probeTMDB, t))
	if err != nil && !errors.Is(err, ErrNothing) {
		return nil, err
	}
	out := []LampacSource{}
	seen := map[string]bool{}
	for _, e := range evs {
		if e.Rch || !balanserRe.MatchString(e.Balanser) || seen[e.Balanser] {
			continue
		}
		seen[e.Balanser] = true
		name, _ := cutQuality(e.Name)
		if name == "" {
			name = e.Balanser
		}
		out = append(out, LampacSource{ID: e.Balanser, Name: name, Found: e.Show})
	}
	return out, nil
}
