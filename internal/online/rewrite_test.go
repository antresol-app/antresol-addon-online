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

// An HLS master that says nothing of closed captions makes AVFoundation
// offer a «CC» subtitle option that carries nothing (measured: a caption-less
// master without the attribute gives a legible group with one closedCaption
// option; with CLOSED-CAPTIONS=NONE there is no group). The proxy says it.
func TestRewriteDeclaresNoClosedCaptions(t *testing.T) {
	base, _ := url.Parse("https://cdn.example/v/42/master.m3u8")
	streamInfs := func(out string) []string {
		var l []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
				l = append(l, strings.TrimRight(line, "\r"))
			}
		}
		return l
	}
	t.Run("a master without captions gets NONE on every variant", func(t *testing.T) {
		master := string(fixture(t, "master.m3u8"))
		out := rewriteM3U8(master, base, testSign, "")
		infs := streamInfs(out)
		if len(infs) != 2 {
			t.Fatalf("variants: %v", infs)
		}
		for _, l := range infs {
			if !strings.HasSuffix(l, ",CLOSED-CAPTIONS=NONE") || strings.Count(l, "CLOSED-CAPTIONS") != 1 {
				t.Fatalf("variant without NONE: %q", l)
			}
		}
		// its other attributes stay
		if !strings.Contains(infs[0], `BANDWIDTH=5000000,RESOLUTION=1920x1080,AUDIO="aud",SUBTITLES="sub"`) {
			t.Fatalf("attributes lost: %q", infs[0])
		}
		// idempotent: a playlist that says it is left as it is
		again := rewriteM3U8(out, base, func(abs string) string { return abs }, "")
		if strings.Count(again, "CLOSED-CAPTIONS=NONE") != 2 {
			t.Fatalf("said twice:\n%s", again)
		}
		// CRLF stays
		crlf := rewriteM3U8(strings.ReplaceAll(master, "\n", "\r\n"), base, testSign, "")
		if strings.Contains(strings.ReplaceAll(crlf, "\r\n", ""), "\n") || strings.Contains(crlf, "\r\r") {
			t.Fatalf("line endings changed:\n%q", crlf)
		}
		if got := streamInfs(crlf); len(got) != 2 || !strings.HasSuffix(got[0], ",CLOSED-CAPTIONS=NONE") {
			t.Fatalf("crlf variants: %q", got)
		}
	})
	left := map[string]string{
		"a master that has captions": "#EXTM3U\n" +
			"#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",NAME=\"English\",INSTREAM-ID=\"CC1\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,CLOSED-CAPTIONS=\"cc\"\na.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2,CLOSED-CAPTIONS=\"cc\"\nb.m3u8\n",
		"captions declared in the group only": "#EXTM3U\n" +
			"#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",INSTREAM-ID=\"CC1\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1\na.m3u8\n",
		"a master that says NONE": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CLOSED-CAPTIONS=NONE\na.m3u8\n",
		// NONE on one variant and a group on another is not valid HLS: not made worse
		"a master of mixed variants": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CLOSED-CAPTIONS=\"cc\"\na.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2\nb.m3u8\n",
		// a quoted value that only looks like the attribute
		"a quoted look-alike": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,NAME=\"x,CLOSED-CAPTIONS=NONE\"\na.m3u8\n",
	}
	for name, in := range left {
		t.Run(name, func(t *testing.T) {
			out := rewriteM3U8(in, base, func(abs string) string { return abs }, "")
			want := strings.ReplaceAll(in, "a.m3u8", "https://cdn.example/v/42/a.m3u8")
			want = strings.ReplaceAll(want, "b.m3u8", "https://cdn.example/v/42/b.m3u8")
			if name == "a quoted look-alike" {
				// the look-alike is not the attribute: the variant says NONE (once, as its own)
				if !strings.HasSuffix(strings.Split(out, "\n")[1], `NAME="x,CLOSED-CAPTIONS=NONE",CLOSED-CAPTIONS=NONE`) {
					t.Fatalf("look-alike taken for the attribute:\n%s", out)
				}
				return
			}
			if out != want {
				t.Fatalf("changed:\n%s\nwant\n%s", out, want)
			}
		})
	}
	t.Run("a media playlist is not touched", func(t *testing.T) {
		media := string(fixture(t, "media.m3u8"))
		if out := rewriteM3U8(media, base, func(abs string) string { return abs }, ""); strings.Contains(out, "CLOSED-CAPTIONS") {
			t.Fatalf("media playlist changed:\n%s", out)
		}
	})
	t.Run("the bounds count what is added", func(t *testing.T) {
		master := string(fixture(t, "master.m3u8"))
		_, size := measureM3U8([]byte(master), base, func(_ *url.URL, raw string) (int, bool) { return len(testSign(raw)), true })
		if out := rewriteM3U8(master, base, testSign, ""); size != len(out) {
			t.Fatalf("measured %d, written %d", size, len(out))
		}
	})
}
