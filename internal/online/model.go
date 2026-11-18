package online

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// The Lampac answers (?rjson=true), as far as Vault reads them.

type levelResp struct {
	Type  string            `json:"type"`
	Voice []voiceDTO        `json:"voice"`
	Data  []json.RawMessage `json:"data"`
}

// movieResp, seasonResp, episodeResp and similarResp are the data of
// levelResp by its type.
type (
	movieResp struct {
		Data []itemDTO `json:"data"`
	}
	seasonResp struct {
		Data []seasonDTO `json:"data"`
	}
	episodeResp struct {
		Data []itemDTO `json:"data"`
	}
	similarResp struct {
		Data []similarDTO `json:"data"`
	}
)

type voiceDTO struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Active bool   `json:"active"`
	Name   string `json:"name"`
}

type seasonDTO struct {
	Method string  `json:"method"`
	ID     flexInt `json:"id"`
	URL    string  `json:"url"`
	Name   string  `json:"name"`
}

type similarDTO struct {
	Method  string     `json:"method"`
	URL     string     `json:"url"`
	Year    flexInt    `json:"year"`
	Details flexString `json:"details"`
	Title   string     `json:"title"`
}

type subDTO struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// itemDTO is a MovieDto / EpisodeDto (and a call's VideoDto).
type itemDTO struct {
	Method        string     `json:"method"`
	URL           string     `json:"url"`
	Stream        string     `json:"stream"`
	Headers       strMap     `json:"headers"`
	Quality       orderedMap `json:"quality"`
	Subtitles     []subDTO   `json:"subtitles"`
	SubtitlesCall string     `json:"subtitles_call"`
	Translate     string     `json:"translate"`
	Name          string     `json:"name"`
	VoiceName     string     `json:"voice_name"`
	MaxQuality    string     `json:"maxquality"`
	Details       string     `json:"details"`
	Title         string     `json:"title"`
	S             flexInt    `json:"s"`
	E             flexInt    `json:"e"`
}

// orderedMap is a JSON object of strings, in its order.
type orderedMap []struct{ K, V string }

func (m *orderedMap) UnmarshalJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	if delim, ok := t.(json.Delim); !ok || delim != '{' {
		return nil // not an object: no qualities
	}
	for d.More() {
		kt, err := d.Token()
		if err != nil {
			return err
		}
		var v any
		if err := d.Decode(&v); err != nil {
			return err
		}
		if s, ok := v.(string); ok {
			*m = append(*m, struct{ K, V string }{kt.(string), s})
		}
	}
	return nil
}

// strMap is a JSON object of strings; other values and shapes are dropped.
type strMap map[string]string

func (m *strMap) UnmarshalJSON(b []byte) error {
	var o orderedMap
	if err := o.UnmarshalJSON(b); err != nil {
		return nil
	}
	if len(o) > 0 {
		*m = strMap{}
		for _, kv := range o {
			(*m)[kv.K] = kv.V
		}
	}
	return nil
}

// Level is one step of a source, as clients see it (no Lampac or CDN URL:
// every ref is signed).
type Level struct {
	Level      string      `json:"level"` // movie | seasons | episodes | pick
	Voices     []Choice    `json:"voices,omitempty"`
	Seasons    []Season    `json:"seasons,omitempty"`
	Season     int         `json:"season,omitempty"`
	Episodes   []Episode   `json:"episodes,omitempty"`
	Files      []File      `json:"files,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`

	// play is the play data of each file (movie) or episode (episodes),
	// in their order: what Resolve plays; ids are their identities
	// (itemID). Never sent to clients.
	play  []PlayItem
	ids   []string
	loose []string // ids without the quality names
}

// Choice is a voice-over: a link to the same level in that voice.
type Choice struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Ref    string `json:"ref"`
	path   string
}

type Season struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
	Ref    string `json:"ref"`
	path   string
}

// Episode is a playable episode; its ref is a file ref.
// episodeName is the name of an episode the source gives none (a catalog
// key: Browse shows it in the request's language; the item id uses it as is).
const episodeName = "Серия %d"

type Episode struct {
	Season     int      `json:"season"`
	Episode    int      `json:"episode"`
	Name       string   `json:"name"`
	Ref        string   `json:"ref"`
	Qualities  []string `json:"qualities"`
	MaxQuality string   `json:"maxQuality,omitempty"`
	Subtitles  int      `json:"subtitles"`
	// SubtitleTracks are the labels and languages of its subtitles (as
	// many as Subtitles), for the picker to say which there are. A «call»
	// entry learns its own only when played.
	SubtitleTracks []SubInfo `json:"subtitleTracks,omitempty"`
	Unsupported    bool      `json:"unsupported,omitempty"`
}

// SubInfo is what the picker shows of a subtitle track: its label as the
// source gives it and its language ("": the label names none).
type SubInfo struct {
	Label string `json:"label"`
	Lang  string `json:"lang"`
}

// subInfos are the labels and languages of subs (nil: none).
func subInfos(subs []Sub) []SubInfo {
	var out []SubInfo
	for _, s := range subs {
		out = append(out, SubInfo{Label: s.Label, Lang: labelLang(s.Label)})
	}
	return out
}

// File is a playable film entry (usually one per voice-over).
type File struct {
	Name       string   `json:"name"`
	Voice      string   `json:"voice"`
	Qualities  []string `json:"qualities"`
	MaxQuality string   `json:"maxQuality,omitempty"`
	Ref        string   `json:"ref"`
	Subtitles  int      `json:"subtitles"`
	// SubtitleTracks: as Episode's.
	SubtitleTracks []SubInfo `json:"subtitleTracks,omitempty"`
	Unsupported    bool      `json:"unsupported,omitempty"`
}

// Candidate is a title to pick when the source found several.
type Candidate struct {
	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	Details string `json:"details,omitempty"`
	Ref     string `json:"ref"`
	path    string
}

// PlayItem is what a file entry plays, with every URL split at " or "
// and checked. Headers are the answer's, as given (Resolve sanitizes).
type PlayItem struct {
	Method        string // play | call
	URLs          []string
	Stream        string
	Headers       map[string]string
	Qualities     []QualityURLs // best first
	Subtitles     []Sub
	SubtitlesCall string
}

// QualityURLs is one quality and its fallbacks, in order.
type QualityURLs struct {
	Name string
	URLs []string
}

// Sub is a sidecar subtitle of a file. File and Lang are set once it is
// converted to WebVTT (Subs.VTT) for a play answer.
type Sub struct {
	Label, URL string
	File, Lang string
}

// PlayItemFor finds the file a file ref points to in a (re-fetched) level:
// the entry whose identity is c.ID, at c.File (1-based) when it is still
// there, else the first one elsewhere. Lampac's lists are not stable
// (voices added, entries reordered or dropped), so the position is only a
// hint; no match is ErrNothing, never another video. It returns the
// entry's 1-based position too.
func (lv Level) PlayItemFor(c RefContext) (PlayItem, int, error) {
	n := lv.match(c)
	if n == 0 {
		return PlayItem{}, 0, ErrNothing
	}
	return lv.play[n-1], n, nil
}

// match is the entry with the ref's identity, else (a source that added
// or dropped a quality) the one with its identity without the quality
// names; the position breaks ties.
func (lv Level) match(c RefContext) int {
	if c.ID == "" || c.File < 1 {
		return 0
	}
	if n := find(lv.ids, c.ID, c.File); n > 0 {
		return n
	}
	if c.L == "" {
		return 0
	}
	return find(lv.loose, c.L, c.File)
}

// find is the 1-based position of id in ids: at hint when it is there,
// else the first one (0: none).
func find(ids []string, id string, hint int) int {
	if hint <= len(ids) && ids[hint-1] == id {
		return hint
	}
	for i, x := range ids {
		if x == id {
			return i + 1
		}
	}
	return 0
}

// itemID is the identity of a file entry: a short hash of what tells it
// from its neighbours (voice, season, episode, names, quality names).
func itemID(voice string, season, episode int, name, title string, qualities []string) string {
	h := sha256.New()
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00%s", norm(voice), season, episode, norm(name), norm(title))
	for _, q := range sortQualities(qualities) {
		fmt.Fprintf(h, "\x00%s", norm(q))
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:9])
}

// looseID is itemID without the quality names.
func looseID(voice string, season, episode int, name, title string) string {
	return itemID(voice, season, episode, name, title, nil)
}

// parseLevel normalizes one Lampac level. Children carry the raw link in
// path; the service signs them.
func parseLevel(b []byte, c RefContext) (Level, error) {
	var r levelResp
	if err := json.Unmarshal(b, &r); err != nil {
		return Level{}, fmt.Errorf("%w: level", ErrSourceFailed)
	}
	if len(r.Data) == 0 {
		return Level{}, ErrNothing
	}
	var lv Level
	activeVoice := c.Voice
	for _, v := range r.Voice {
		if n := strings.TrimSpace(v.Name); n != "" && v.URL != "" {
			lv.Voices = append(lv.Voices, Choice{Name: n, Active: v.Active, path: v.URL})
			if v.Active && c.Voice == "" {
				activeVoice = n
			}
		}
	}
	switch r.Type {
	case "movie":
		var m movieResp
		if err := json.Unmarshal(b, &m); err != nil {
			return Level{}, fmt.Errorf("%w: movie", ErrSourceFailed)
		}
		lv.Level = "movie"
		for _, it := range m.Data {
			p, ok := playItem(it)
			if !ok {
				continue
			}
			// some sources put a quality where the voice-over goes
			translate := it.Translate
			if qualityRe.MatchString(strings.TrimSpace(translate)) {
				translate = ""
			}
			name, hint := cutQuality(first(translate, it.Name, it.Title))
			voice, _ := cutQuality(first(translate, c.Voice))
			f := File{Name: name, Voice: voice, Qualities: names(p.Qualities),
				MaxQuality: first(hint, maxQuality(it)), Subtitles: len(p.Subtitles), SubtitleTracks: subInfos(p.Subtitles), Unsupported: unsupported(p)}
			lv.Files = append(lv.Files, f)
			lv.play = append(lv.play, p)
			lv.ids = append(lv.ids, itemID(f.Voice, 0, 0, f.Name, it.Title, f.Qualities))
			lv.loose = append(lv.loose, looseID(f.Voice, 0, 0, f.Name, it.Title))
		}
		if len(lv.Files) == 0 {
			return Level{}, ErrNothing
		}
	case "season":
		var m seasonResp
		if err := json.Unmarshal(b, &m); err != nil {
			return Level{}, fmt.Errorf("%w: season", ErrSourceFailed)
		}
		lv.Level = "seasons"
		for _, it := range m.Data {
			n := seasonNumber(it)
			if n <= 0 || it.URL == "" { // specials (season 0) are not offered
				continue
			}
			lv.Seasons = append(lv.Seasons, Season{Number: n, Name: strings.TrimSpace(it.Name), path: it.URL})
		}
		if len(lv.Seasons) == 0 {
			return Level{}, ErrNothing
		}
	case "episode":
		var m episodeResp
		if err := json.Unmarshal(b, &m); err != nil {
			return Level{}, fmt.Errorf("%w: episode", ErrSourceFailed)
		}
		lv.Level = "episodes"
		for _, it := range m.Data {
			p, ok := playItem(it)
			if !ok {
				continue
			}
			s := int(it.S)
			if s == 0 {
				s = c.Season
			}
			name, hint := cutQuality(first(it.Name, it.Title, fmt.Sprintf(episodeName, int(it.E))))
			e := Episode{Season: s, Episode: int(it.E), Name: name, Qualities: names(p.Qualities),
				MaxQuality: first(hint, maxQuality(it)), Subtitles: len(p.Subtitles), SubtitleTracks: subInfos(p.Subtitles), Unsupported: unsupported(p)}
			lv.Episodes = append(lv.Episodes, e)
			lv.play = append(lv.play, p)
			lv.ids = append(lv.ids, itemID(activeVoice, e.Season, e.Episode, e.Name, it.Title, e.Qualities))
			lv.loose = append(lv.loose, looseID(activeVoice, e.Season, e.Episode, e.Name, it.Title))
		}
		if len(lv.Episodes) == 0 {
			return Level{}, ErrNothing
		}
		lv.Season = c.Season
		if lv.Season == 0 {
			lv.Season = lv.Episodes[0].Season
		}
	case "similar":
		var m similarResp
		if err := json.Unmarshal(b, &m); err != nil {
			return Level{}, fmt.Errorf("%w: similar", ErrSourceFailed)
		}
		lv.Level = "pick"
		for _, it := range m.Data {
			if it.URL == "" {
				continue
			}
			lv.Candidates = append(lv.Candidates, Candidate{Title: strings.TrimSpace(it.Title), Year: int(it.Year),
				Details: strings.TrimSpace(string(it.Details)), path: it.URL})
		}
		if len(lv.Candidates) == 0 {
			return Level{}, ErrNothing
		}
	default:
		return Level{}, fmt.Errorf("%w: unknown level type", ErrSourceFailed)
	}
	return lv, nil
}

// parseSubs reads the answer of a subtitles_call: a list of {label, url},
// or an object with one under "subtitles" (or "data"). Anything else is
// no subtitles.
func parseSubs(b []byte) []Sub {
	var list []subDTO
	if err := json.Unmarshal(b, &list); err != nil {
		var o struct {
			Subtitles []subDTO `json:"subtitles"`
			Data      []subDTO `json:"data"`
		}
		if json.Unmarshal(b, &o) != nil {
			return nil
		}
		list = append(o.Subtitles, o.Data...)
	}
	return subsOf(list)
}

// subsOf are a source's subtitles with a usable URL (its first one).
func subsOf(list []subDTO) []Sub {
	var out []Sub
	for _, s := range list {
		if us := splitURLs(s.URL); len(us) > 0 {
			out = append(out, Sub{Label: strings.TrimSpace(s.Label), URL: us[0]})
		}
	}
	return out
}

// parseVideo reads the answer of a call (a VideoDto).
func parseVideo(b []byte) (PlayItem, error) {
	var it itemDTO
	if err := json.Unmarshal(b, &it); err != nil {
		return PlayItem{}, fmt.Errorf("%w: call", ErrSourceFailed)
	}
	if it.Method == "" {
		it.Method = "play"
	}
	p, ok := playItem(it)
	if !ok || p.Method != "play" || len(p.URLs) == 0 {
		return PlayItem{}, ErrNothing
	}
	return p, nil
}

// playItem normalizes a file entry; false: nothing playable in it.
func playItem(it itemDTO) (PlayItem, bool) {
	p := PlayItem{Method: it.Method, Headers: carriedHeaders(it.Headers)}
	switch it.Method {
	case "call":
		// the call URL is a Lampac link: one URL, not split
		if u := strings.TrimSpace(it.URL); u != "" && !hasCtl(u) {
			p.URLs = []string{u}
		}
		if s := strings.TrimSpace(it.Stream); s != "" && !hasCtl(s) {
			p.Stream = s
		}
		if len(p.URLs) == 0 {
			return PlayItem{}, false
		}
	case "play":
		p.URLs = splitURLs(it.URL)
		for _, q := range it.Quality {
			if us := splitURLs(q.V); len(us) > 0 {
				p.Qualities = append(p.Qualities, QualityURLs{Name: strings.TrimSpace(q.K), URLs: us})
			}
		}
		sort.SliceStable(p.Qualities, func(i, j int) bool { return qualityLess(p.Qualities[i].Name, p.Qualities[j].Name) })
		if len(p.URLs) == 0 && len(p.Qualities) > 0 {
			p.URLs = p.Qualities[0].URLs
		}
		if len(p.URLs) == 0 {
			return PlayItem{}, false
		}
	default:
		return PlayItem{}, false
	}
	p.Subtitles = subsOf(it.Subtitles)
	if s := strings.TrimSpace(it.SubtitlesCall); s != "" && !hasCtl(s) {
		p.SubtitlesCall = s
	}
	return p, true
}

// carried are the request headers a source may ask for (canonical
// names); anything else (Host, Authorization, X-Forwarded-For, hop-by-hop)
// is dropped.
var carried = map[string]bool{"Referer": true, "Origin": true, "User-Agent": true, "Cookie": true, "Accept": true, "Accept-Language": true}

const (
	maxCarried    = 8
	maxCarriedLen = 4 << 10
)

// carriedHeaders keeps the allowed headers with valid values (no CR, LF
// or other control characters), at most 8, each at most 4 KiB.
func carriedHeaders(h strMap) map[string]string {
	var out map[string]string
	for k, v := range h {
		name := http.CanonicalHeaderKey(strings.TrimSpace(k))
		if !carried[name] || len(v) > maxCarriedLen || !httpguts.ValidHeaderFieldValue(v) ||
			strings.ContainsAny(v, "\r\n\x00") || len(out) >= maxCarried {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[name] = strings.TrimSpace(v)
	}
	return out
}

func hasCtl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0
}

// splitURLs splits a Lampac URL field into its fallbacks (" or "), and
// splits again at whitespace and control characters (some sources join
// several URLs in one string). Only absolute http(s) URLs with a host are
// kept, in order.
func splitURLs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, " or ") {
		for _, f := range strings.FieldsFunc(part, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			u, err := url.Parse(f)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
				continue
			}
			out = append(out, f) // as given: a CDN signature may depend on its exact encoding
		}
	}
	return out
}

func names(q []QualityURLs) []string {
	out := make([]string, 0, len(q))
	for _, x := range q {
		out = append(out, x.Name)
	}
	return out
}

// unsupported: every URL of the entry is DASH (.mpd).
func unsupported(p PlayItem) bool {
	if p.Method != "play" {
		return false
	}
	all := p.URLs
	for _, q := range p.Qualities {
		all = append(all, q.URLs...)
	}
	for _, u := range all {
		if !isMPD(u) {
			return false
		}
	}
	return len(all) > 0
}

func isMPD(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".mpd")
}

func maxQuality(it itemDTO) string {
	if q := strings.TrimSpace(it.MaxQuality); qualityRe.MatchString(q) {
		return q
	}
	return ""
}

// quality order: 2160 > 1440 > 1080 > 720 > 480 > 360 > the rest (by
// height, then name).
var qualityRank = map[int]int{2160: 0, 1440: 1, 1080: 2, 720: 3, 480: 4, 360: 5}

func qualityHeight(q string) int {
	q = strings.ToLower(strings.TrimSpace(q))
	switch q {
	case "4k", "uhd":
		return 2160
	case "2k":
		return 1440
	case "fhd":
		return 1080
	case "hd":
		return 720
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(q, "p"))
	return n
}

func qualityLess(a, b string) bool {
	ha, hb := qualityHeight(a), qualityHeight(b)
	ra, oka := qualityRank[ha]
	rb, okb := qualityRank[hb]
	switch {
	case oka && okb:
		return ra < rb
	case oka != okb:
		return oka
	case ha != hb:
		return ha > hb
	}
	return a < b
}

// sortQualities sorts quality names best first.
func sortQualities(q []string) []string {
	out := append([]string(nil), q...)
	sort.SliceStable(out, func(i, j int) bool { return qualityLess(out[i], out[j]) })
	return out
}

var (
	qualityRe     = regexp.MustCompile(`(?i)^(\d{3,4}p|[248]k|uhd|fhd|hd|sd)$`)
	qualitySuffix = regexp.MustCompile(`(?i)^(.*?)\s+[~-]\s+(\d{3,4}p|[248]k|uhd|fhd|hd|sd)\s*$`)
	firstNumber   = regexp.MustCompile(`\d+`)
)

// cutQuality cuts Lampac's « - 1080p» / « ~ 1080p» hint off a name.
func cutQuality(s string) (name, quality string) {
	s = strings.TrimSpace(s)
	if m := qualitySuffix.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		return strings.TrimSpace(m[1]), m[2]
	}
	return s, ""
}

// seasonNumber is the season of a season link: its id, else its s=, else
// the first number in its name.
func seasonNumber(it seasonDTO) int {
	if it.ID > 0 {
		return int(it.ID)
	}
	if u, err := url.Parse(it.URL); err == nil {
		if n, err := strconv.Atoi(u.Query().Get("s")); err == nil {
			return n
		}
	}
	if m := firstNumber.FindString(it.Name); m != "" {
		n, _ := strconv.Atoi(m)
		return n
	}
	return 0
}

func first(s ...string) string {
	for _, x := range s {
		if x = strings.TrimSpace(x); x != "" {
			return x
		}
	}
	return ""
}
