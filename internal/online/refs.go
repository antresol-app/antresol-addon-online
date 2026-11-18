package online

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"strings"
)

// mintStrict makes Mint panic on a bad path (set by the tests).
var mintStrict bool

// ErrBadRef: a ref that is forged, damaged or not a Lampac /lite/ path.
var ErrBadRef = errors.New("Неверная ссылка")

// RefContext is the title context a ref carries: the gate re-checks
// TMDBID on every use, and progress uses season, episode, source and voice.
// File (1-based) and ID point to one file or episode of the level at the
// ref's path (ID is its identity, File only where it was); 0 is the level
// itself.
type RefContext struct {
	Kind    string `json:"k"`
	TMDBID  int    `json:"t"`
	Season  int    `json:"s,omitempty"`
	Episode int    `json:"e,omitempty"`
	Source  string `json:"src,omitempty"`
	Voice   string `json:"v,omitempty"`
	File    int    `json:"f,omitempty"`
	ID      string `json:"i,omitempty"`
	// L is the identity without the quality names (a file whose
	// qualities changed is still found; ID breaks ties).
	L string `json:"l,omitempty"`
}

// Refs signs Lampac paths for clients: base64url(JSON) + "." +
// base64url(HMAC-SHA256[:16]). The path never holds the Lampac origin or
// its user id (both are added back by Lampac.Get).
type Refs struct {
	key  []byte
	lamp *Lampac // only its Base origin is used
}

// NewRefs makes the signer for refs on base (ANTRESOL_ONLINE_LAMPAC_URL) with key
// (the online_ref_key setting).
func NewRefs(key []byte, base *url.URL) *Refs {
	return &Refs{key: append([]byte(nil), key...), lamp: &Lampac{Base: base}}
}

type refPayload struct {
	P string     `json:"p"`
	C RefContext `json:"c"`
}

const (
	refMACLen = 16
	maxRef    = 8 << 10
)

func litePath(p string) bool { return strings.HasPrefix(p, "/lite/") }

// clean is the path a ref holds: under /lite/ on the Lampac origin, with
// no scheme, host or user id.
func (r *Refs) clean(p string) (string, error) {
	rel, err := r.lamp.relative(p, litePath)
	if err != nil {
		return "", err
	}
	path, q, _ := strings.Cut(rel, "?")
	if q = stripQuery(q, func(k string) bool { return authKeys[k] }); q != "" {
		path += "?" + q
	}
	return path, nil
}

func (r *Refs) mint(p string, c RefContext) (string, error) {
	clean, err := r.clean(p)
	if err != nil {
		return "", err
	}
	return r.sign(refPayload{P: clean, C: c})
}

// Mint signs a Lampac /lite/ path with its context. A path that is not
// one panics in tests and returns "" (logged) otherwise: callers mint only
// paths from Lampac answers, which the service checks first.
func (r *Refs) Mint(p string, c RefContext) string {
	ref, err := r.mint(p, c)
	if err != nil {
		if mintStrict {
			panic("online: Mint: " + err.Error())
		}
		log.Printf("online: refused to sign a non-/lite/ path: %v", err)
		return ""
	}
	return ref
}

func (r *Refs) mac(payload string) []byte {
	m := hmac.New(sha256.New, r.key)
	m.Write([]byte(payload))
	return m.Sum(nil)[:refMACLen]
}

func (r *Refs) sign(p refPayload) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(r.mac(body)), nil
}

// Open checks a ref and returns its path and context.
func (r *Refs) Open(ref string) (string, RefContext, error) {
	if len(ref) > maxRef {
		return "", RefContext{}, ErrBadRef
	}
	body, sig, ok := strings.Cut(ref, ".")
	if !ok || body == "" {
		return "", RefContext{}, ErrBadRef
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil || !hmac.Equal(got, r.mac(body)) {
		return "", RefContext{}, ErrBadRef
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return "", RefContext{}, ErrBadRef
	}
	var p refPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", RefContext{}, ErrBadRef
	}
	clean, err := r.clean(p.P)
	if err != nil || clean != p.P || !validKind(p.C.Kind) || p.C.TMDBID <= 0 || p.C.File < 0 || (p.C.File > 0) != (p.C.ID != "") {
		return "", RefContext{}, ErrBadRef
	}
	return p.P, p.C, nil
}

func validKind(k string) bool { return k == "movie" || k == "series" }
