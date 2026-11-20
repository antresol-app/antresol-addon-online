package store

import (
	"bytes"
	"context"
	"regexp"
	"testing"
)

// TestLampacUID_FitsTelegramBot: a new id is 6–20 characters of
// [a-zA-Z0-9_-] (Lampac's Telegram bot takes nothing else), kept once made;
// an id saved before in the older long form stays as it is; renew makes a
// new one of the new form.
func TestLampacUID_FitsTelegramBot(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	s := Settings{DB: d.DB}
	ok := regexp.MustCompile(`^antresol[a-z0-9]{8}$`)
	uid, err := s.LampacUID(ctx, false)
	if err != nil || !ok.MatchString(uid) || len(uid) < 6 || len(uid) > 20 {
		t.Fatalf("new uid %q %v", uid, err)
	}
	if again, _ := s.LampacUID(ctx, false); again != uid {
		t.Fatalf("not kept: %q then %q", uid, again)
	}
	old := "antresol-0123456789abcdef0123456789abcdef"
	if _, err := d.Exec(`UPDATE settings SET value=? WHERE key='lampac_uid'`, old); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LampacUID(ctx, false); got != old {
		t.Fatalf("saved uid changed: %q", got)
	}
	if nu, _ := s.LampacUID(ctx, true); nu == old || !ok.MatchString(nu) {
		t.Fatalf("renewed %q", nu)
	}
	seen := map[string]bool{}
	for range 200 {
		v, err := NewLampacUID()
		if err != nil || !ok.MatchString(v) || seen[v] {
			t.Fatalf("NewLampacUID %q %v", v, err)
		}
		seen[v] = true
	}
}

// TestSettings_EnabledDefaultOn: no row is on; a saved value is read
// back; a value that is not 1 or 0 is an error.
func TestSettings_EnabledDefaultOn(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	s := Settings{DB: d.DB}
	if on, err := s.Enabled(ctx); err != nil || !on {
		t.Fatalf("no row: %v %v, want on", on, err)
	}
	if err := s.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if on, err := s.Enabled(ctx); err != nil || on {
		t.Fatalf("saved off: %v %v", on, err)
	}
	if _, err := d.Exec(`UPDATE settings SET value='yes' WHERE key='enabled'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enabled(ctx); err == nil {
		t.Fatal("a bad value read as a setting")
	}
}

// TestSettings_KeysCreatedOnceAndKept: the ref and proxy keys are 32
// random bytes each, different, made on first use and the same after a
// reopen.
func TestSettings_KeysCreatedOnceAndKept(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	ref, proxy, err := Settings{DB: d.DB}.Keys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ref) != 32 || len(proxy) != 32 || bytes.Equal(ref, proxy) {
		t.Fatalf("keys %x %x", ref, proxy)
	}
	dir := d.Dir
	d.Close()
	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	ref2, proxy2, err := Settings{DB: d2.DB}.Keys(ctx)
	if err != nil || !bytes.Equal(ref, ref2) || !bytes.Equal(proxy, proxy2) {
		t.Fatalf("keys changed on reopen: %v", err)
	}
	if _, err := d2.Exec(`UPDATE settings SET value='zz' WHERE key='ref_key'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Settings{DB: d2.DB}).Keys(ctx); err == nil {
		t.Fatal("a stored key that is not hex was used")
	}
}
