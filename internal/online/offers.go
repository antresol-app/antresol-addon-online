package online

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"time"
)

// Offer is what a source offers at a glance, for the badges of its row in a
// picker: the voice-overs it lists, whether its files have subtitles and in
// which languages («RU», in order), and the best quality its files name. It
// is read from the source's first level (a series': its first season too),
// as the web sheet does (OnlineSheet's offerOf).
type Offer struct {
	Voices   []string
	Subs     bool
	SubLangs []string
	Quality  string
}

// FileVoice is the voice-over a film file plays in.
func FileVoice(f File) string {
	if f.Voice != "" {
		return f.Voice
	}
	return f.Name
}

// SubLangsOf are the languages of tracks, uppercased, each once, in order (a
// track whose label names none adds nothing).
func SubLangsOf(tracks []SubInfo) []string {
	var langs []string
	seen := map[string]bool{}
	for _, t := range tracks {
		if l := strings.ToUpper(strings.TrimSpace(t.Lang)); l != "" && !seen[l] {
			seen[l] = true
			langs = append(langs, l)
		}
	}
	return langs
}

// OfferOf reads the offer of lv, a source's first level, with the playable
// level under it (inner: a series' first season) when that is known. false:
// a «pick» level (titles, not files) has none.
func OfferOf(lv Level, inner *Level) (Offer, bool) {
	if lv.Level == "pick" {
		return Offer{}, false
	}
	play := lv
	if inner != nil && inner.Level != "pick" {
		play = *inner
	}
	voices := lv.Voices
	if len(voices) == 0 {
		voices = play.Voices
	}
	var o Offer
	for _, v := range voices {
		o.Voices = append(o.Voices, v.Name)
	}
	if len(o.Voices) == 0 { // no list of voices: those the files play in
		seen := map[string]bool{}
		for _, f := range play.Files {
			if v := FileVoice(f); !f.Unsupported && v != "" && !seen[v] {
				seen[v] = true
				o.Voices = append(o.Voices, v)
			}
		}
	}
	var tracks []SubInfo
	add := func(unsupported bool, subs int, t []SubInfo, qualities []string, maxQuality string) {
		if subs > 0 {
			o.Subs = true
			tracks = append(tracks, t...)
		}
		if unsupported {
			return
		}
		q := maxQuality
		if len(qualities) > 0 {
			q = sortQualities(qualities)[0]
		}
		if q != "" && (o.Quality == "" || qualityLess(q, o.Quality)) {
			o.Quality = q
		}
	}
	switch play.Level {
	case "movie":
		for _, f := range play.Files {
			add(f.Unsupported, f.Subtitles, f.SubtitleTracks, f.Qualities, f.MaxQuality)
		}
	case "episodes":
		for _, e := range play.Episodes {
			add(e.Unsupported, e.Subtitles, e.SubtitleTracks, e.Qualities, e.MaxQuality)
		}
	}
	o.SubLangs = SubLangsOf(tracks)
	return o, true
}

const (
	// offerWorkers is how many sources one call looks up at once: a
	// profile's Lampac slots (callsPerUser), which what the person waits for
	// shares.
	offerWorkers = callsPerUser
	// offerMissTTL is how long a source that failed is not asked again for
	// its offer.
	offerMissTTL = 2 * time.Minute
)

// offerKey is one source of one title.
type offerKey struct {
	kind   string
	tmdbID int
	source string
}

// drainKey is one profile's look at one title.
type drainKey struct {
	user   int64
	kind   string
	tmdbID int
}

// Offers looks up the offer of each of srcs (a title's sources) for the
// badges of a picker's rows: the sources' first levels, which Lampac's
// answers cached for a few minutes make cheap the next time. It waits at most
// `wait` and returns what is ready by then; a source that is not has no entry
// (a «pick» level, a failing source or a slow one). The lookups do not stop
// with the request: those in flight end within a level's budget and fill the
// caches, and one lookup at a time goes on through the sources not yet asked,
// so the next call finds them (the web sheet asks one at a time: the profile's
// slots are what the person's own choice needs).
func (s *Service) Offers(ctx context.Context, u User, kind string, tmdbID int, srcs []Source, wait time.Duration) map[string]Offer {
	out := map[string]Offer{}
	if len(srcs) == 0 || wait <= 0 || !s.on() || u.IsKid {
		return out
	}
	s.init()
	lctx := context.WithoutCancel(ctx)
	dk := drainKey{u.ID, kind, tmdbID}
	drain := s.startDrain(dk)

	var (
		mu    sync.Mutex
		next  int
		over  bool // the caller stopped waiting
		wg    sync.WaitGroup
		ended = make(chan struct{})
	)
	worker := func(first bool) {
		defer wg.Done()
		for {
			mu.Lock()
			if next >= len(srcs) || (over && !(first && drain)) {
				mu.Unlock()
				return
			}
			src := srcs[next]
			next++
			mu.Unlock()
			if o, ok := s.offer(lctx, u, offerKey{kind, tmdbID, src.ID}, src); ok {
				mu.Lock()
				out[src.ID] = o
				mu.Unlock()
			}
		}
	}
	wg.Add(offerWorkers)
	s.offerWG.Add(1)
	for i := range offerWorkers {
		go worker(i == 0)
	}
	go func() {
		defer s.offerWG.Done()
		wg.Wait()
		close(ended)
		if drain {
			s.endDrain(dk)
		}
	}()

	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ended:
	case <-t.C:
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	over = true
	return maps.Clone(out)
}

// WaitOffers waits for the lookups of Offers still going on in the
// background (tests, shutdown).
func (s *Service) WaitOffers() { s.offerWG.Wait() }

// startDrain: false when this profile's lookups for the title already go on
// in the background (the new call's own workers then stop with its wait).
func (s *Service) startDrain(k drainKey) bool {
	s.omu.Lock()
	defer s.omu.Unlock()
	if s.draining == nil {
		s.draining = map[drainKey]bool{}
	}
	if s.draining[k] {
		return false
	}
	s.draining[k] = true
	return true
}

func (s *Service) endDrain(k drainKey) {
	s.omu.Lock()
	defer s.omu.Unlock()
	delete(s.draining, k)
}

// offer reads src's first level, and a series' first season under it.
func (s *Service) offer(ctx context.Context, u User, k offerKey, src Source) (Offer, bool) {
	if _, miss := s.offerMiss.get(ctx, k); miss {
		return Offer{}, false
	}
	lv, err := s.Browse(ctx, u, src.Ref)
	if err != nil {
		// a source that failed is not asked at once again; one that was not
		// asked for want of a slot (the profile's own work) is
		if !errors.Is(err, errBusy) && !errors.Is(err, context.Canceled) {
			s.offerMiss.putFor(k, struct{}{}, offerMissTTL)
		}
		return Offer{}, false
	}
	var inner *Level
	if lv.Level == "seasons" && len(lv.Seasons) > 0 {
		if in, err := s.Browse(ctx, u, lv.Seasons[0].Ref); err == nil {
			inner = &in
		}
	}
	return OfferOf(lv, inner)
}
