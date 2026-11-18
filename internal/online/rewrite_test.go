package online

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// testSign is a sign func whose blob is just the base64url of the URL.
func testSign(abs string) string {
	return "/api/online/s/S/" + base64.RawURLEncoding.EncodeToString([]byte(abs))
}

// unsign decodes a testSign path (with an optional ?mk=) back to its URL.
func unsign(t *testing.T, p string) (string, string) {
	t.Helper()
	p, q, _ := strings.Cut(p, "?")
	blob, ok := strings.CutPrefix(p, "/api/online/s/S/")
	if !ok {
		t.Fatalf("not a proxy path: %q", p)
	}
	b, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		t.Fatalf("blob %q: %v", blob, err)
	}
	return string(b), q
}

func TestRewriteM3U8(t *testing.T) {
	master := string(fixture(t, "master.m3u8"))
	base, _ := url.Parse("https://cdn.example/v/42/master.m3u8?tok=1")
	out := rewriteM3U8(master, base, testSign, "")
	var uris []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			i := strings.Index(line, `URI="`)
			if i < 0 {
				t.Fatalf("media group lost its URI: %q", line)
			}
			if !strings.Contains(line, `TYPE=AUDIO`) && !strings.Contains(line, `TYPE=SUBTITLES`) {
				t.Fatalf("media group changed: %q", line)
			}
			v := line[i+5:]
			v = v[:strings.Index(v, `"`)]
			u, _ := unsign(t, v)
			uris = append(uris, u)
		case strings.HasPrefix(line, "#"):
		default:
			u, _ := unsign(t, line)
			uris = append(uris, u)
		}
	}
	want := []string{"https://cdn.example/v/42/a/rus.m3u8", "https://cdn.example/v/42/s/rus.m3u8",
		"https://cdn.example/v/42/1080/index.m3u8", "https://cdn.example/v/42/720/index.m3u8?t=abc"}
	if strings.Join(uris, " ") != strings.Join(want, " ") {
		t.Fatalf("master URIs:\n%v\nwant\n%v", uris, want)
	}
	for _, keep := range []string{"#EXT-X-VERSION:4", "#EXT-X-INDEPENDENT-SEGMENTS", `NAME="Русский"`, `RESOLUTION=1920x1080,AUDIO="aud"`} {
		if !strings.Contains(out, keep) {
			t.Fatalf("lost %q:\n%s", keep, out)
		}
	}

	media := string(fixture(t, "media.m3u8"))
	mbase, _ := url.Parse("https://cdn.example/v/42/1080/index.m3u8")
	out = rewriteM3U8(media, mbase, testSign, "K+1")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var got []string
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-KEY:") || strings.HasPrefix(line, "#EXT-X-MAP:") {
			i := strings.Index(line, `URI="`)
			v := line[i+5:]
			v = v[:strings.Index(v, `"`)]
			u, q := unsign(t, v)
			if q != "mk=K%2B1" {
				t.Fatalf("keyed URI attr without mk: %q", line)
			}
			got = append(got, u)
		} else if !strings.HasPrefix(line, "#") {
			u, q := unsign(t, line)
			if q != "mk=K%2B1" {
				t.Fatalf("keyed segment without mk: %q", line)
			}
			got = append(got, u)
		}
	}
	want = []string{"https://cdn.example/v/42/k.bin", "https://cdn.example/v/42/1080/init.mp4",
		"https://cdn2.example/seg/1.m4s?sig=x", "https://cdn2.example/seg/2.m4s?sig=y"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("media URIs:\n%v\nwant\n%v", got, want)
	}
	if !strings.Contains(out, "# a plain comment\n") || !strings.HasSuffix(out, "#EXT-X-ENDLIST\n") ||
		!strings.Contains(out, ",IV=0x00000000000000000000000000000001") {
		t.Fatalf("comments/ENDLIST/attrs not kept:\n%s", out)
	}
	if strings.Contains(out, "cdn2.example") || strings.Contains(out, "cdn.example") {
		t.Fatalf("an upstream host is left:\n%s", out)
	}

	// CRLF in, CRLF out
	crlf := strings.ReplaceAll(media, "\n", "\r\n")
	out = rewriteM3U8(crlf, mbase, testSign, "")
	if strings.Count(out, "\r\n") != strings.Count(crlf, "\r\n") || strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatalf("line endings changed:\n%q", out)
	}
	if _, q := unsign(t, strings.Split(out, "\r\n")[7]); q != "" {
		t.Fatalf("unkeyed with mk: %q", q)
	}
}

func TestRewriteDropsOtherSchemes(t *testing.T) {
	base, _ := url.Parse("https://cdn.example/x/index.m3u8")
	in := "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://key\"\n#EXTINF:4,\nfile:///etc/passwd\n#EXTINF:4,\nok.ts\n"
	out := rewriteM3U8(in, base, testSign, "")
	if strings.Contains(out, "skd://") || strings.Contains(out, "file:") {
		t.Fatalf("other schemes kept:\n%s", out)
	}
	if !strings.Contains(out, testSign("https://cdn.example/x/ok.ts")) {
		t.Fatalf("http segment lost:\n%s", out)
	}
}
