package online

import (
	"bytes"
	"net/url"
	"strings"
)

// Bounds of one playlist rewrite: the live maximum was ~3000 segments
// (~5 MB in, ~14 MB out), so these leave room for long films and cut a
// hostile playlist (16 MiB of 2-byte lines would be ~1 GB out) short.
const (
	maxPlaylistURIs = 20_000
	maxRewritten    = 48 << 20
)

// uriFunc proxies one URI of a playlist: abs is it resolved against the
// playlist's URL, raw as written. ok false: the URI is dropped (a URI line
// becomes "#", a URI attribute becomes empty).
type uriFunc func(abs *url.URL, raw string) (out string, ok bool)

// resolveURI resolves ref against base: an absolute http(s) URL with a
// host and no userinfo, and the string to sign (an absolute URI as
// given: a CDN signature may depend on its exact encoding).
func resolveURI(base *url.URL, ref string) (*url.URL, string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || hasCtl(ref) {
		return nil, "", false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return nil, "", false
	}
	abs := base.ResolveReference(u)
	if (abs.Scheme != "http" && abs.Scheme != "https") || abs.Hostname() == "" || abs.User != nil {
		return nil, "", false
	}
	if u.IsAbs() {
		return abs, ref, true
	}
	return abs, abs.String(), true
}

// ccNone is what a master playlist says of closed captions when it carries
// none. Without it AVFoundation cannot know and offers a «CC» subtitle option
// (a closedCaption legible option) for any master, one that shows nothing.
const ccNone = "CLOSED-CAPTIONS=NONE"

// eachAttr calls f with each name and value (as written, quotes kept) of the
// attribute list of an HLS tag; f true stops. Values in quotes may hold
// commas and "=".
func eachAttr(tag string, f func(name, value string) bool) {
	colon := strings.IndexByte(tag, ':')
	if colon < 0 {
		return
	}
	rest := tag[colon+1:]
	for len(rest) > 0 {
		eq := strings.IndexAny(rest, "=,")
		if eq < 0 {
			return
		}
		if rest[eq] == ',' {
			rest = rest[eq+1:]
			continue
		}
		name := strings.TrimSpace(rest[:eq])
		rest = rest[eq+1:]
		var value string
		if strings.HasPrefix(rest, `"`) {
			if end := strings.IndexByte(rest[1:], '"'); end < 0 { // unterminated: the rest is the value
				value, rest = rest, ""
			} else {
				value, rest = rest[:end+2], rest[end+2:]
			}
		} else if c := strings.IndexByte(rest, ','); c >= 0 {
			value, rest = rest[:c], rest[c:]
		} else {
			value, rest = rest, ""
		}
		if f(name, value) {
			return
		}
		rest = strings.TrimPrefix(rest, ",")
	}
}

// sayNoCaptions reports whether body is a master playlist that says nothing
// of closed captions: it has variants, none with a CLOSED-CAPTIONS attribute
// and no closed-captions group. HLS wants NONE on every variant or on none, so
// a master that mentions captions anywhere is left as it is.
func sayNoCaptions(body []byte) bool {
	if !bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
		return false
	}
	variants := 0
	for len(body) > 0 {
		var line []byte
		if i := bytes.IndexByte(body, '\n'); i < 0 {
			line, body = body, nil
		} else {
			line, body = body[:i], body[i+1:]
		}
		tag := string(bytes.TrimRight(line, "\r"))
		switch {
		case strings.HasPrefix(tag, "#EXT-X-STREAM-INF:"):
			variants++
			said := false
			eachAttr(tag, func(name, _ string) bool { said = name == "CLOSED-CAPTIONS"; return said })
			if said {
				return false
			}
		case strings.HasPrefix(tag, "#EXT-X-MEDIA:"):
			cc := false
			eachAttr(tag, func(name, value string) bool { cc = name == "TYPE" && value == "CLOSED-CAPTIONS"; return cc })
			if cc {
				return false
			}
		}
	}
	return variants > 0
}

// rewriteLines goes through the lines of body: tags with URI attributes
// and URI lines go through f, everything is handed to emit in order
// (line endings as they were); a master playlist that says nothing of closed
// captions gets CLOSED-CAPTIONS=NONE on its variants. emit false stops.
func rewriteLines(body []byte, base *url.URL, f uriFunc, emit func(s string) bool) {
	noCC := sayNoCaptions(body)
	proxied := func(ref string) (string, bool) {
		abs, raw, ok := resolveURI(base, ref)
		if !ok {
			return "", false
		}
		return f(abs, raw)
	}
	for len(body) > 0 {
		i := bytes.IndexByte(body, '\n')
		var line []byte
		if i < 0 {
			line, body = body, nil
		} else {
			line, body = body[:i+1], body[i+1:]
		}
		text := bytes.TrimRight(line, "\r\n")
		eol := string(line[len(text):])
		var out string
		switch {
		case len(bytes.TrimSpace(text)) == 0, text[0] == '#' && !bytes.HasPrefix(text, []byte("#EXT")):
			out = string(text)
		case text[0] == '#':
			out = rewriteTag(string(text), proxied)
			if noCC && bytes.HasPrefix(text, []byte("#EXT-X-STREAM-INF:")) {
				if !strings.HasSuffix(out, ",") {
					out += ","
				}
				out += ccNone
			}
		default:
			out = "#"
			if p, ok := proxied(string(text)); ok {
				out = p
			}
		}
		if !emit(out) || !emit(eol) {
			return
		}
	}
}

// rewriteTag replaces the URI="…" attribute values of an HLS tag. The
// attribute list is read as HLS defines it (name=value, values in quotes
// may hold commas), so a quoted value of another attribute is never
// taken for a URI.
func rewriteTag(tag string, proxied func(string) (string, bool)) string {
	colon := strings.IndexByte(tag, ':')
	if colon < 0 || !strings.Contains(tag[colon:], `URI="`) {
		return tag
	}
	var b strings.Builder
	b.WriteString(tag[:colon+1])
	rest := tag[colon+1:]
	for len(rest) > 0 {
		eq := strings.IndexAny(rest, "=,")
		if eq < 0 || rest[eq] == ',' { // not an attribute: as it is
			if eq < 0 {
				b.WriteString(rest)
				break
			}
			b.WriteString(rest[:eq+1])
			rest = rest[eq+1:]
			continue
		}
		name := rest[:eq]
		b.WriteString(rest[:eq+1])
		rest = rest[eq+1:]
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 { // unterminated: as it is
				b.WriteString(rest)
				break
			}
			value, rest = rest[:end+2], rest[end+2:]
			if strings.TrimSpace(name) == "URI" {
				p, ok := proxied(value[1 : len(value)-1])
				if !ok {
					p = ""
				}
				value = `"` + p + `"`
			}
		} else if c := strings.IndexByte(rest, ','); c >= 0 {
			value, rest = rest[:c], rest[c:]
		} else {
			value, rest = rest, ""
		}
		b.WriteString(value)
		if strings.HasPrefix(rest, ",") {
			b.WriteByte(',')
			rest = rest[1:]
		}
	}
	return b.String()
}

// measureM3U8 is how many URIs a rewrite of body proxies and how big it
// is (sizeOf: the length of a proxied URI; false: dropped), measured
// before a byte is written. It stops once past maxPlaylistURIs or
// maxRewritten (a hostile playlist costs no more than that).
func measureM3U8(body []byte, base *url.URL, sizeOf func(abs *url.URL, raw string) (int, bool)) (uris, size int) {
	rewriteLines(body, base, func(abs *url.URL, raw string) (string, bool) {
		n, ok := sizeOf(abs, raw)
		if ok {
			uris++
			size += n
		}
		return "", ok
	}, func(s string) bool { size += len(s); return uris <= maxPlaylistURIs && size <= maxRewritten })
	return uris, size
}

// rewriteM3U8 makes an upstream HLS playlist point at the proxy: every
// URI line and every URI="…" attribute is resolved against base and
// replaced by sign(abs) (a <Proxy.Base>… path), with ?mk= when mk is
// set (AVPlayer and AirPlay fetch them without the app's headers).
// Comments, tags, their other attributes and the line endings stay as
// they are, but for the variants of a master that says nothing of closed
// captions (CLOSED-CAPTIONS=NONE is added). A URI that is not http(s) is dropped: the line becomes a bare
// "#" (a URI attribute becomes empty).
func rewriteM3U8(body string, base *url.URL, sign func(abs string) string, mk string) string {
	q := mkQuery(mk)
	var b strings.Builder
	rewriteLines([]byte(body), base, func(_ *url.URL, raw string) (string, bool) { return sign(raw) + q, true },
		func(s string) bool { b.WriteString(s); return true })
	return b.String()
}

func mkQuery(mk string) string {
	if mk == "" {
		return ""
	}
	return "?mk=" + url.QueryEscape(mk)
}

// echoes reports whether a playlist's tags or comments (outside its URI
// attributes, which are proxied) hold secret.
func echoes(body []byte, secret string) bool {
	if secret == "" || !bytes.Contains(body, []byte(secret)) {
		return false
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(line) == 0 || line[0] != '#' {
			continue
		}
		t := rewriteTag(string(line), func(string) (string, bool) { return "", true })
		if strings.Contains(t, secret) {
			return true
		}
	}
	return false
}
