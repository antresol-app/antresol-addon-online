package online

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/subtitles"
	"golang.org/x/sync/singleflight"
)

const (
	// maxSubBytes bounds a subtitle file fetched from a source.
	maxSubBytes = 2 << 20
	// defaultSubsCache caps DataDir/subs/online (oldest deleted first).
	defaultSubsCache = 200 << 20
	// subsBudget bounds the conversion of one track (the background
	// warm-up and a first fetch).
	subsBudget = 30 * time.Second
	// subFailTTL: a track that is not usable (its server refused it, its
	// content is not subtitles) is left out of play answers so long;
	// subRetryTTL one that failed otherwise (a timeout, a server error,
	// the conversions busy).
	subFailTTL  = 10 * time.Minute
	subRetryTTL = time.Minute
	// maxSubs is how many subtitle tracks one play answer offers.
	maxSubs = 8
)

var errSubtitle = errors.New("online: subtitle not usable")

// errSubtitleBusy: no conversion slot in time (errSubtitle, but worth
// asking again soon).
var errSubtitleBusy = fmt.Errorf("%w: conversions busy", errSubtitle)

// subFailFor is how long a failed track is left out: subFailTTL when the
// track itself is not usable, subRetryTTL for a failure that may pass.
func subFailFor(err error) time.Duration {
	if errors.Is(err, errSubtitle) && !errors.Is(err, errSubtitleBusy) && !errors.Is(err, errSubtitleServer) {
		return subFailTTL
	}
	return subRetryTTL
}

// errSubtitleServer: the subtitle's server failed (5xx).
var errSubtitleServer = fmt.Errorf("%w: server error", errSubtitle)

// Subs turns a source's subtitles into WebVTT files, cached by URL in Dir
// (DataDir/subs/online).
type Subs struct {
	Dir      string
	FFmpeg   string       // "": ffmpeg
	HTTP     *http.Client // the upstream client (newUpstreamClient)
	Lampac   *Lampac      // URLs on its origin get its auth query
	MaxCache int64        // 0: 200 MB

	mu sync.Mutex // one prune at a time

	once   sync.Once
	sf     singleflight.Group          // one conversion per URL at a time
	failed *ttlCache[string, struct{}] // URLs that did not convert lately
	warm   sync.WaitGroup              // the warm-ups of play answers
}

// Wait waits for the background conversions of play answers (tests:
// before Dir goes).
func (s *Subs) Wait() { s.warm.Wait() }

// fileOf is where the WebVTT of a subtitle URL is kept.
func (s *Subs) fileOf(rawURL string) string {
	sum := sha1.Sum([]byte(rawURL))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:])+".vtt")
}

func (s *Subs) init() {
	s.once.Do(func() { s.failed = newTTLCache[string, struct{}](2000, subFailTTL) })
}

// cached is the WebVTT file of sub when it is there already ("": not
// yet), and whether sub failed lately.
func (s *Subs) cached(sub Sub) (string, bool) {
	s.init()
	if _, bad := s.failed.get(context.Background(), sub.URL); bad {
		return "", true
	}
	if st, err := os.Stat(s.fileOf(sub.URL)); err == nil && st.Size() > 0 {
		return s.fileOf(sub.URL), false
	}
	return "", false
}

// Shared is VTT with one conversion per URL at a time (a play's warm-up
// and the player's first fetch share it); it runs on for subsBudget when
// its caller goes away, and a failure is remembered for subFailTTL.
func (s *Subs) Shared(ctx context.Context, sub Sub, h http.Header) (string, error) {
	s.init()
	ch := s.sf.DoChan(sub.URL, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subsBudget)
		defer cancel()
		p, err := s.VTT(fctx, sub, h)
		if err != nil {
			s.failed.putFor(sub.URL, struct{}{}, subFailFor(err))
		}
		return p, err
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// VTT is the WebVTT file of sub (fetched with the headers h, at most 2
// MiB): WebVTT is kept as it is, SRT and ASS go through ffmpeg. The file
// is DataDir/subs/online/<sha1(url)>.vtt; a cached one is reused.
func (s *Subs) VTT(ctx context.Context, sub Sub, h http.Header) (string, error) {
	out := s.fileOf(sub.URL)
	if st, err := os.Stat(out); err == nil && st.Size() > 0 {
		now := time.Now()
		os.Chtimes(out, now, now) // recently used: pruned last
		return out, nil
	}
	resp, err := upstream{http: s.HTTP, lampac: s.Lampac}.get(ctx, sub.URL, h)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: HTTP %d", errSubtitleServer, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: HTTP %d", errSubtitle, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSubBytes+1))
	if err != nil {
		return "", transportErr(err)
	}
	if len(b) > maxSubBytes {
		return "", fmt.Errorf("%w: over %d bytes", errSubtitle, maxSubBytes)
	}
	text := subtitles.Decode(b)
	var vtt []byte
	trimmed := strings.TrimLeft(text, " \t\r\n")
	switch {
	case strings.HasPrefix(trimmed, "WEBVTT"):
		vtt = []byte(trimmed)
	case strings.Contains(text, "-->") || strings.Contains(text, "[Script Info]"):
		f := "srt"
		if strings.Contains(text, "[Script Info]") || strings.Contains(text, "[Events]") {
			f = "ass"
		}
		if vtt, err = s.convert(ctx, f, text); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("%w: unknown format", errSubtitle)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.Dir, ".vtt-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(vtt)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), out)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return "", werr
	}
	s.prune()
	return out, nil
}

// convert runs ffmpeg on text (srt or ass) piped in, WebVTT piped out:
// the same arguments as the external subtitles of local files.
func (s *Subs) convert(ctx context.Context, format, text string) ([]byte, error) {
	ff := s.FFmpeg
	if ff == "" {
		ff = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := convSlots.acquire(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", errSubtitleBusy, err)
	}
	defer convSlots.release()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-protocol_whitelist", "pipe",
		"-f", format, "-i", "pipe:0", "-c:s", "webvtt", "-f", "webvtt", "pipe:1")
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil { // out of time, not a bad file
			return nil, fmt.Errorf("online: subtitle ffmpeg: %w", ctx.Err())
		}
		return nil, fmt.Errorf("%w: ffmpeg: %v", errSubtitle, err)
	}
	if !bytes.HasPrefix(stdout.Bytes(), []byte("WEBVTT")) {
		return nil, fmt.Errorf("%w: ffmpeg gave no WebVTT", errSubtitle)
	}
	return stdout.Bytes(), nil
}

// prune deletes the oldest files of Dir until it holds at most MaxCache.
func (s *Subs) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	max := s.MaxCache
	if max <= 0 {
		max = defaultSubsCache
	}
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".vtt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{filepath.Join(s.Dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= max {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// SubLang is a subtitle's language: from its label ("Русские", "eng"),
// else sniffed from the file ("ru" for Cyrillic text), else "".
func SubLang(label, path string) string {
	if l := labelLang(label); l != "" {
		return l
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return subtitles.SniffLang(b)
}

// labelLang is the language a subtitle's label names ("Русские", "eng",
// "English (Forced)"), else "".
func labelLang(label string) string {
	for _, w := range strings.FieldsFunc(label, func(r rune) bool { return r == ' ' || r == '(' || r == ')' || r == '[' || r == ']' || r == ',' }) {
		if l := subtitles.NormLang(w); l != "" {
			return l
		}
	}
	return subtitles.NormLang(label)
}

// PlaySubtitles is r's subtitles for a play answer (at most 8) without
// waiting for them: a track already converted has its file (and sniffed
// language), the others are converted on their first fetch (ServeSub)
// and by the warm-up, which the caller starts once the session is open
// (not for a play that failed); a track that failed lately is left out.
func (s *Service) PlaySubtitles(ctx context.Context, r Resolved) (subs []Sub, warm func()) {
	warm = func() {}
	if s.Subs == nil || len(r.Subtitles) == 0 {
		return nil, warm
	}
	all := r.Subtitles
	if len(all) > maxSubs {
		all = all[:maxSubs]
	}
	out := []Sub{}
	var cold []Sub
	for _, sub := range all {
		p, bad := s.Subs.cached(sub)
		if bad {
			continue
		}
		sub.File, sub.Lang = p, SubLang(sub.Label, p)
		if p == "" {
			cold = append(cold, sub)
		}
		out = append(out, sub)
	}
	warm = func() {
		for _, sub := range cold {
			s.Subs.warm.Add(1)
			go func() { // the player asks for it in a moment
				defer s.Subs.warm.Done()
				if _, err := s.Subs.Shared(context.WithoutCancel(ctx), sub, SubHeaders(r, sub)); err != nil {
					s.log().Info("online: subtitle", "source", r.Ctx.Source, "err", err)
				}
			}()
		}
	}
	return out, warm
}

// SubHeaders are the request headers of a subtitle of r (Cookie only to
// the source's site).
func SubHeaders(r Resolved, sub Sub) http.Header { return forSite(r.Headers, r.CookieSite, sub.URL) }
