// Package online is «Смотреть онлайн»: an adapter to a Lampac sidecar
// (its /lite/ JSON API), signed refs that hide Lampac from clients, and the
// kid and rating gate. Nothing here touches the HLS manager.
package online

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// UserAgent is the one User-Agent of every Lampac call and of upstream
// fetches that name none: links signed for a User-Agent then still play.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// Errors of the Lampac adapter; their text is what the person sees.
var (
	// ErrUnavailable: Lampac does not answer (refused, unreachable, timeout).
	ErrUnavailable = errors.New("Онлайн-источники сейчас недоступны")
	// ErrSourceFailed: a refusal ({"accsdb":…}, {"rch":true}), HTML, a
	// redirect, an oversized body or an HTTP error.
	ErrSourceFailed = errors.New("Источник не ответил")
	// ErrAuth: Lampac refused the account ({"accsdb": true}, 401, 403):
	// the address needs the account of «Настройки».
	ErrAuth = errors.New("Lampac не пускает: проверьте аккаунт в настройках «Онлайн»")
	// ErrNothing: {} or an empty list.
	ErrNothing = errors.New("Ничего не нашлось в этом источнике")
	// errTimeout marks an ErrUnavailable that was a timeout: for one
	// source's level it is that source failing, not Lampac.
	errTimeout = errors.New("timeout")
	// errBusy: all 4 Lampac slots stayed taken (Vault is saturated, not
	// the source failing).
	errBusy = errors.New("busy")
)

const (
	maxBody     = 4 << 20
	callTimeout = 20 * time.Second
	maxCalls    = 4
	// DefaultCallTimeout is callTimeout, for «Настройки».
	DefaultCallTimeout = callTimeout
)

// authKeys are the query keys that identify the Lampac user: never kept
// from an answer's link, never put in a ref.
var authKeys = map[string]bool{"uid": true, "token": true, "account_email": true, "box_mac": true}

// Lampac talks to the sidecar. Base is ANTRESOL_ONLINE_LAMPAC_URL: its scheme and
// host are the only origin called, and its query (the uid) is added to
// every call.
type Lampac struct {
	Base *url.URL
	HTTP *http.Client // nil: http.DefaultTransport
	UA   string       // "": UserAgent
	// Timeout bounds one call (0: 20 s).
	Timeout time.Duration
	// Health (nil: none) keeps how the last call went, for «Состояние»
	// (helpers.status): reached, refused the account, or not reached.
	Health *addonkit.HelperHealth

	once   sync.Once
	sem    chan struct{} // at most 4 calls at a time
	users  userSlots     // at most callsPerUser of them per profile (ForUser)
	server atomic.Value  // the last Server header (Status)
}

// NewLampac parses ANTRESOL_ONLINE_LAMPAC_URL ("http://lampac:9118/?uid=…").
func NewLampac(raw string) (*Lampac, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("ANTRESOL_ONLINE_LAMPAC_URL: неверный адрес")
	}
	return &Lampac{Base: u}, nil
}

// redactLampac is what may be logged of a Lampac URL: scheme://host:port.
func redactLampac(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "lampac"
	}
	return u.Scheme + "://" + u.Host
}

func (l *Lampac) init() {
	l.once.Do(func() { l.sem = make(chan struct{}, maxCalls) })
}

func (l *Lampac) ua() string {
	if l.UA != "" {
		return l.UA
	}
	return UserAgent
}

// sameOrigin says whether u (absolute) is on the Base origin.
func (l *Lampac) sameOrigin(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, l.Base.Scheme) && strings.EqualFold(u.Host, l.Base.Host)
}

// relative turns a Lampac path or a URL on the Base origin into
// "/path?query"; anything else (another origin, a scheme-relative URL,
// "..", control characters, a path outside allow) is an error.
func (l *Lampac) relative(raw string, allow func(p string) bool) (string, error) {
	if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '\\' }) >= 0 {
		return "", errors.New("online: bad Lampac path")
	}
	// an encoded slash or backslash in the path is a separator to some
	// servers and not to others: refused
	if rawPath, _, _ := strings.Cut(raw, "?"); strings.Contains(strings.ToLower(rawPath), "%5c") || strings.Contains(strings.ToLower(rawPath), "%2f") {
		return "", errors.New("online: bad Lampac path")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("online: bad Lampac path")
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil {
		if !u.IsAbs() || !l.sameOrigin(u) || u.User != nil {
			return "", errors.New("online: not the Lampac origin")
		}
	}
	p := u.EscapedPath()
	if !strings.HasPrefix(p, "/") || path.Clean(u.Path) != u.Path || strings.Contains(p, "//") || !allow(u.Path) {
		return "", errors.New("online: path not allowed")
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p, nil
}

func lampacPath(p string) bool { return strings.HasPrefix(p, "/lite/") || p == "/externalids" }

// stripQuery drops the pairs of raw whose key drop says, keeping the
// others exactly as they were encoded.
func stripQuery(raw string, drop func(key string) bool) string {
	if raw == "" {
		return ""
	}
	var keep []string
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" {
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if !drop(strings.ToLower(k)) {
			keep = append(keep, kv)
		}
	}
	return strings.Join(keep, "&")
}

// target is the URL Get calls: the Base origin, the path, the query
// without auth keys and rjson, then rjson=true and the Base query.
func (l *Lampac) target(pathAndQuery string) (string, error) {
	rel, err := l.relative(pathAndQuery, lampacPath)
	if err != nil {
		return "", err
	}
	p, q, _ := strings.Cut(rel, "?")
	q = stripQuery(q, func(k string) bool { return authKeys[k] || k == "rjson" })
	parts := []string{}
	if q != "" {
		parts = append(parts, q)
	}
	parts = append(parts, "rjson=true")
	if bq := stripQuery(l.Base.RawQuery, func(k string) bool { return k == "rjson" }); bq != "" {
		parts = append(parts, bq)
	}
	return l.Base.Scheme + "://" + l.Base.Host + p + "?" + strings.Join(parts, "&"), nil
}

// Get calls Lampac (a path, or a URL on the Base origin, under /lite/ or
// /externalids) and returns its JSON answer. Refusals, HTML and HTTP
// errors are ErrSourceFailed, {} is ErrNothing, a dead Lampac is
// ErrUnavailable. Errors never carry the URL (it holds the uid).
func (l *Lampac) Get(ctx context.Context, pathAndQuery string) ([]byte, error) {
	l.init()
	target, err := l.target(pathAndQuery)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceFailed, err)
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = callTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if id, ok := userOf(ctx); ok {
		release, err := l.users.acquire(ctx, id)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, fmt.Errorf("online: %w", context.Canceled)
			}
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, errBusy)
		}
		defer release()
	}
	select {
	case l.sem <- struct{}{}:
		defer func() { <-l.sem }()
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("online: %w", context.Canceled)
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, errBusy)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: bad request", ErrSourceFailed)
	}
	req.Header.Set("User-Agent", l.ua())
	req.Header.Set("Accept", "application/json")
	c := http.Client{CheckRedirect: l.checkRedirect}
	if l.HTTP != nil {
		c.Transport, c.Jar = l.HTTP.Transport, l.HTTP.Jar
	}
	resp, err := c.Do(req)
	if errors.Is(err, errRedirect) {
		return nil, fmt.Errorf("%w: bad-redirect", ErrSourceFailed)
	}
	if err != nil {
		if !errors.Is(ctx.Err(), context.Canceled) { // not a request the caller gave up
			l.Health.Record(transportProblem(err))
		}
		return nil, transportErr(err)
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Server"); s != "" {
		l.server.Store(s)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		l.Health.Record(&addonkit.HelperStatusError{Code: resp.StatusCode})
		return nil, fmt.Errorf("%w: %w: HTTP %d", ErrSourceFailed, ErrAuth, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		// Lampac answered: a source behind it failed
		l.Health.Record(nil)
		return nil, fmt.Errorf("%w: HTTP %d", ErrSourceFailed, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, transportErr(err)
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("%w: answer over %d bytes", ErrSourceFailed, maxBody)
	}
	out, err := classify(b)
	if errors.Is(err, ErrAuth) {
		l.Health.Record(&addonkit.HelperStatusError{Code: http.StatusUnauthorized})
	} else {
		l.Health.Record(nil)
	}
	return out, err
}

// transportProblem is a client error for HelperHealth, without its text
// (it may carry the uid): what HelperProblem sorts by.
func transportProblem(err error) error {
	var ne net.Error
	var op *net.OpError
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns):
		return &net.DNSError{Err: "lookup failed", IsTimeout: dns.IsTimeout, IsNotFound: dns.IsNotFound}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return context.DeadlineExceeded
	case errors.As(err, &op) && op.Op == "dial" && strings.Contains(op.Err.Error(), "refused"):
		return errors.New("connection refused")
	}
	return errors.New("connection failed")
}

// errRedirect: a redirect off the Lampac origin or its allowed paths.
var errRedirect = errors.New("redirect refused")

// checkRedirect follows Lampac's own redirects (a source may answer 302
// to its own path with another query), at most 3, with the auth query
// put back; any other redirect is refused.
func (l *Lampac) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 3 || !l.sameOrigin(req.URL) {
		return errRedirect
	}
	t, err := l.target(req.URL.RequestURI())
	if err != nil {
		return errRedirect
	}
	u, err := url.Parse(t)
	if err != nil {
		return errRedirect
	}
	req.URL, req.Host = u, u.Host
	req.Header.Set("User-Agent", l.ua())
	return nil
}

// transportErr maps a client error to a category. Its text is never kept:
// net/http puts upstream text in some errors (a bad Location header with
// the uid Lampac echoes into its links), and logs must never see the uid.
func transportErr(err error) error {
	var ne net.Error
	var op *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("online: %w", context.Canceled) // the client went away
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return fmt.Errorf("%w: %w", ErrUnavailable, errTimeout)
	case errors.As(err, &op) && op.Op == "dial":
		return fmt.Errorf("%w: connection", ErrUnavailable)
	case errors.As(err, &op):
		return fmt.Errorf("%w: connection lost", ErrUnavailable)
	}
	return fmt.Errorf("%w: protocol", ErrUnavailable)
}

// classify checks a Lampac answer: it answers 200 with a refusal body.
func classify(b []byte) ([]byte, error) {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') || !json.Valid(t) {
		return nil, fmt.Errorf("%w: not JSON", ErrSourceFailed)
	}
	if t[0] == '[' {
		return t, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t, &m); err != nil {
		return nil, fmt.Errorf("%w: not an object", ErrSourceFailed)
	}
	if len(m) == 0 {
		return nil, ErrNothing
	}
	if truthy(m["accsdb"]) {
		return nil, fmt.Errorf("%w: %w (accsdb)", ErrSourceFailed, ErrAuth)
	}
	if truthy(m["rch"]) {
		return nil, fmt.Errorf("%w: the source needs a Lampa client (rch)", ErrSourceFailed)
	}
	return t, nil
}

func truthy(r json.RawMessage) bool {
	s := strings.TrimSpace(string(r))
	return s != "" && s != "false" && s != "null" && s != "0" && s != `""`
}

// ExternalIDs asks Lampac for the IMDb and Kinopoisk ids of a TMDB title
// (imdb, when known, helps it find the KP id).
func (l *Lampac) ExternalIDs(ctx context.Context, tmdbID int, serial bool, imdb string) (string, string, error) {
	v := url.Values{"id": {strconv.Itoa(tmdbID)}, "serial": {boolNum(serial)}, "imdb_id": {imdb}}
	b, err := l.Get(ctx, "/externalids?"+v.Encode())
	if errors.Is(err, ErrNothing) {
		return imdb, "", nil
	}
	if err != nil {
		return imdb, "", err
	}
	var r struct {
		IMDb flexString `json:"imdb_id"`
		KP   flexString `json:"kinopoisk_id"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return imdb, "", fmt.Errorf("%w: externalids", ErrSourceFailed)
	}
	if r.IMDb != "" {
		imdb = string(r.IMDb)
	}
	kp := string(r.KP)
	if kp == "0" {
		kp = ""
	}
	return imdb, kp, nil
}

// Q is the card of a title, as Lampac's own client sends it.
type Q struct {
	TMDBID                                           int
	IMDb, KP, Title, OriginalTitle, OriginalLanguage string
	Year                                             int
	Serial                                           bool
}

// values is the card query of q (the keys Lampac's client adds to every
// link it follows).
func (q Q) values() url.Values {
	v := url.Values{"id": {strconv.Itoa(q.TMDBID)}, "source": {"tmdb"}, "serial": {boolNum(q.Serial)}}
	add := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	add("imdb_id", q.IMDb)
	add("kinopoisk_id", q.KP)
	add("title", q.Title)
	add("original_title", q.OriginalTitle)
	add("original_language", q.OriginalLanguage)
	if q.Year > 0 {
		v.Set("year", strconv.Itoa(q.Year))
	}
	return v
}

// Event is one source of /lite/events.
type Event struct {
	Name, URL, Balanser string
	Show, Rch           bool
	Index               int
}

// Events lists the sources Lampac has for a title (with checkOnlineSearch,
// the ones that found it are show:true, working first).
func (l *Lampac) Events(ctx context.Context, q Q) ([]Event, error) {
	b, err := l.Get(ctx, "/lite/events?"+q.values().Encode())
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name     string  `json:"name"`
		URL      string  `json:"url"`
		Balanser string  `json:"balanser"`
		Show     *bool   `json:"show"`
		Rch      bool    `json:"rch"`
		Index    flexInt `json:"index"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%w: events", ErrSourceFailed)
	}
	out := make([]Event, 0, len(raw))
	for _, e := range raw {
		// without checkOnlineSearch there is no "show": every source is listed
		show := e.Show == nil || *e.Show
		out = append(out, Event{Name: e.Name, URL: e.URL, Balanser: e.Balanser, Show: show, Rch: e.Rch, Index: int(e.Index)})
	}
	return out, nil
}

// Server is the last Server header Lampac sent ("" = none).
func (l *Lampac) Server() string {
	s, _ := l.server.Load().(string)
	return s
}

func boolNum(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// flexInt reads a JSON number or a numeric string (Lampac sends both).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if n, err := strconv.Atoi(s); err == nil {
		*f = flexInt(n)
	} else if x, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(int(x))
	}
	return nil
}

// flexString reads a JSON string or a number as text.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(strings.TrimSpace(s))
		return nil
	}
	t := strings.TrimSpace(string(b))
	if t != "null" {
		*f = flexString(t)
	}
	return nil
}
