package online

import (
	"net/url"
	"strings"
	"testing"
)

func testRefs(t *testing.T) *Refs {
	t.Helper()
	u, _ := url.Parse("http://lampac.test:9118/?uid=secret")
	return NewRefs([]byte("0123456789abcdef0123456789abcdef"), u)
}

func TestRefRoundTripAndTamper(t *testing.T) {
	r := testRefs(t)
	c := RefContext{Kind: "series", TMDBID: 1396, Season: 2, Episode: 3, Source: "alpha", Voice: "Дубляж", File: 4, ID: "abc"}
	ref := r.Mint("/lite/alpha?id=1&uid=secret&token=t&s=2", c)
	if ref == "" || strings.Contains(ref, "secret") {
		t.Fatalf("ref %q", ref)
	}
	p, got, err := r.Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	if p != "/lite/alpha?id=1&s=2" || got != c {
		t.Fatalf("%q %+v", p, got)
	}
	// an absolute URL on the Lampac origin loses scheme and host
	p, _, err = r.Open(r.Mint("http://lampac.test:9118/lite/beta?x=1&account_email=a&box_mac=b", c))
	if err != nil || p != "/lite/beta?x=1" {
		t.Fatalf("%q %v", p, err)
	}
	for i := range ref {
		b := []byte(ref)
		b[i] ^= 1
		if _, _, err := r.Open(string(b)); err != ErrBadRef {
			t.Fatalf("flipped byte %d: %v", i, err)
		}
	}
	for _, bad := range []string{"", ".", "abc", "abc.def", strings.Repeat("a", 20000)} {
		if _, _, err := r.Open(bad); err != ErrBadRef {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// another key does not open it
	u, _ := url.Parse("http://lampac.test:9118/?uid=secret")
	if _, _, err := NewRefs([]byte("another key"), u).Open(ref); err != ErrBadRef {
		t.Fatalf("foreign key: %v", err)
	}
}

func TestRefRejectsNonLite(t *testing.T) {
	r := testRefs(t)
	c := RefContext{Kind: "movie", TMDBID: 1}
	for _, p := range []string{"/admin", "/proxy/x", "http://evil/lite/x", "https://lampac.test:9118/lite/x", "/lite/../admin", "/lite/a b", "lite/x",
		"/externalids?id=1", `/lite/..\admin`, "/lite/..%5Cadmin", "/lite/..%5cadmin", "/lite/a%2Fb", "/lite/a%2f..%2fadmin"} {
		if _, err := r.mint(p, c); err == nil {
			t.Errorf("%q: minted", p)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%q: Mint did not panic in a test", p)
				}
			}()
			r.Mint(p, c)
		}()
	}
	// a file ref without its identity (or an identity without a file) is refused
	for _, bad := range []RefContext{{Kind: "movie", TMDBID: 1, File: 1}, {Kind: "movie", TMDBID: 1, ID: "x"}} {
		tok, _ := r.sign(refPayload{P: "/lite/a", C: bad})
		if _, _, err := r.Open(tok); err != ErrBadRef {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	// a payload signed with the right key but a non-/lite/ path is still refused
	tok, err := r.sign(refPayload{P: "/admin", C: c})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Open(tok); err != ErrBadRef {
		t.Fatalf("%v", err)
	}
}
