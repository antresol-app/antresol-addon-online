package online

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// The upstream guard: stream URLs come from sources and their playlists,
// so the client that fetches them never reaches the home network (private,
// loopback, link-local, metadata addresses), except Lampac itself, and
// there only its stream paths.

const (
	upstreamHeaderTimeout = 20 * time.Second
	upstreamBodyIdle      = 30 * time.Second
	maxUpstreamRedirects  = 5
)

// errBlocked: an address of the home network (or a scheme, a userinfo or a
// Lampac path) the upstream client does not go to.
var errBlocked = errors.New("online: upstream address not allowed")

// guardConfig is the upstream guard. lampac is the Lampac host:port as
// configured; resolve and allow are for tests (nil: the system resolver,
// nothing else allowed).
type guardConfig struct {
	lampac  string
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	allow   func(netip.AddrPort) bool
}

// newUpstreamClient is the client of every stream, playlist, subtitle and
// probe fetch. Its dialer refuses addonkit.BlockedAddr addresses, except the
// Lampac address when the URL names Lampac's own host:port (resolved on
// each dial); only http and https, at most 5 redirects (each re-checked);
// response headers within 20 s, and a body read that stalls for 30 s ends
// the request.
func newUpstreamClient(lampacHostPort string) *http.Client {
	return newGuarded(guardConfig{lampac: lampacHostPort})
}

func newGuarded(cfg guardConfig) *http.Client {
	if cfg.resolve == nil {
		cfg.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	cfg.lampac = strings.ToLower(cfg.lampac)
	tr := &http.Transport{
		Proxy:                 nil, // a proxy would connect on our behalf and dodge the check
		DialContext:           cfg.dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: upstreamHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: &idleTransport{base: tr, idle: upstreamBodyIdle},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxUpstreamRedirects {
				return errBlocked
			}
			// Go sends the previous URL as the Referer of a hop: on the
			// Lampac origin it holds the uid. Only the source's own
			// Referer (if any) goes on.
			if ref := via[0].Header.Get("Referer"); ref != "" {
				req.Header.Set("Referer", ref)
			} else {
				req.Header.Del("Referer")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" || req.URL.User != nil {
				return errBlocked
			}
			// Lampac only from Lampac, and only its stream paths
			if hostPort(req.URL) == cfg.lampac && (hostPort(via[len(via)-1].URL) != cfg.lampac || !lampacStreamPath(req.URL)) {
				return errBlocked
			}
			return nil
		},
	}
}

// dial resolves addr itself and dials only allowed IPs; the dialer's
// Control checks the IP actually dialled again.
func (cfg guardConfig) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errBlocked
	}
	// Lampac's address is reachable only under Lampac's own name
	lampac := map[netip.AddrPort]bool{}
	if strings.EqualFold(addr, cfg.lampac) {
		ips, err := cfg.lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if ap, err := netip.ParseAddrPort(net.JoinHostPort(ip.Unmap().String(), port)); err == nil {
				lampac[ap] = true
			}
		}
	}
	ok := func(ap netip.AddrPort) bool {
		return !addonkit.BlockedAddr(ap.Addr().String()) || lampac[ap] || (cfg.allow != nil && cfg.allow(ap))
	}
	ips, err := cfg.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !ok(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())) {
				return errBlocked
			}
			return nil
		}}
	lastErr := errBlocked
	for _, ip := range ips {
		ap, err := netip.ParseAddrPort(net.JoinHostPort(ip.Unmap().String(), port))
		if err != nil || !ok(ap) {
			continue
		}
		c, err := d.DialContext(ctx, network, ap.String())
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (cfg guardConfig) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{ip}, nil
	}
	return cfg.resolve(ctx, host)
}

// hostPort is u's lower-cased host:port, with the scheme's default port.
func hostPort(u *url.URL) string {
	p := u.Port()
	if p == "" {
		p = "80"
		if u.Scheme == "https" {
			p = "443"
		}
	}
	return strings.ToLower(net.JoinHostPort(u.Hostname(), p))
}

// lampacStreamPath: the Lampac paths a stream may be on (its proxy and
// its sources' own playlists), clean, without encoded separators.
func lampacStreamPath(u *url.URL) bool {
	p := u.EscapedPath()
	if path.Clean(u.Path) != u.Path || strings.Contains(p, "//") || strings.ContainsAny(u.Path, "\\") {
		return false
	}
	lp := strings.ToLower(p)
	if strings.Contains(lp, "%2f") || strings.Contains(lp, "%5c") || strings.Contains(lp, "%2e") {
		return false
	}
	return strings.HasPrefix(u.Path, "/proxy/") || strings.HasPrefix(u.Path, "/lite/")
}

// idleTransport ends a request whose body read waits longer than idle.
type idleTransport struct {
	base http.RoundTripper
	idle time.Duration
}

func (t *idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &idleBody{rc: resp.Body, cancel: cancel, idle: t.idle}
	return resp, nil
}

func (t *idleTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// idleBody arms a deadline for each Read (not while the reader waits on
// its own client).
type idleBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration
	timer  *time.Timer
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.timer == nil {
		b.timer = time.AfterFunc(b.idle, b.cancel)
	} else {
		b.timer.Reset(b.idle)
	}
	n, err := b.rc.Read(p)
	b.timer.Stop()
	return n, err
}

func (b *idleBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	err := b.rc.Close()
	b.cancel()
	return err
}

// upstream fetches stream URLs: through the guarded client, and on the
// Lampac origin with Lampac's auth query (put back on the fetch only:
// URLs handed to clients never hold it).
type upstream struct {
	http   *http.Client
	lampac *Lampac // nil: no Lampac origin
}

// get fetches raw with the headers h (a User-Agent is always sent).
// Errors are categories only: their text never holds the URL.
func (u upstream) get(ctx context.Context, raw string, h http.Header) (*http.Response, error) {
	target, err := u.target(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errBlocked
	}
	for k, vs := range h {
		req.Header[k] = append([]string(nil), vs...)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", UserAgent)
	}
	c := u.http
	if c == nil {
		return nil, errBlocked
	}
	resp, err := c.Do(req)
	if err != nil {
		if errors.Is(err, errBlocked) {
			return nil, errBlocked
		}
		return nil, transportErr(err)
	}
	return resp, nil
}

// target checks raw (http/https, a host, no userinfo or control
// characters) and adds Lampac's auth query on its origin, where only
// stream paths are allowed.
func (u upstream) target(raw string) (string, error) {
	if hasCtl(raw) {
		return "", errBlocked
	}
	p, err := url.Parse(raw)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Hostname() == "" || p.User != nil {
		return "", errBlocked
	}
	if u.lampac == nil || !u.lampac.sameOrigin(p) {
		return raw, nil
	}
	if !lampacStreamPath(p) {
		return "", errBlocked
	}
	q := stripQuery(p.RawQuery, func(k string) bool { return authKeys[k] })
	if bq := stripQuery(u.lampac.Base.RawQuery, func(k string) bool { return k == "rjson" }); bq != "" {
		if q != "" {
			q += "&"
		}
		q += bq
	}
	p.RawQuery = q
	return p.String(), nil
}

// isLampac: u is on the Lampac origin.
func (u upstream) isLampac(p *url.URL) bool {
	return u.lampac != nil && p != nil && u.lampac.sameOrigin(p)
}

// secret is the Lampac user id (what no client output may hold; "": none).
func (u upstream) secret() string {
	if u.lampac == nil || u.lampac.Base == nil {
		return ""
	}
	for k, vs := range u.lampac.Base.Query() {
		if authKeys[strings.ToLower(k)] && len(vs) > 0 && len(vs[0]) >= 4 {
			return vs[0]
		}
	}
	return ""
}

// clean is what a URL of the Lampac origin looks like to clients' blobs:
// without its auth keys.
func (u upstream) clean(raw string) string {
	if u.lampac == nil {
		return raw
	}
	p, err := url.Parse(raw)
	if err != nil || !u.lampac.sameOrigin(p) {
		return raw
	}
	p.RawQuery = stripQuery(p.RawQuery, func(k string) bool { return authKeys[k] })
	return p.String()
}

// NewUpstreamClient is the guarded upstream client for the Lampac of l
// (newUpstreamClient with its host:port).
func NewUpstreamClient(l *Lampac) *http.Client { return upstreamFor(l) }

// upstreamFor is the guarded client for the Lampac of l.
func upstreamFor(l *Lampac) *http.Client {
	if l == nil || l.Base == nil {
		return newUpstreamClient("")
	}
	return newUpstreamClient(hostPort(l.Base))
}
