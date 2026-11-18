package online

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	_ "github.com/antresol-app/antresol-addon-online/internal/strings" // the English catalogs
	_ "modernc.org/sqlite"
)

func init() { mintStrict = true }

// fixture reads a synthetic Lampac answer from testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeLampac serves h as http://lampac.test:9118 (with ?uid=secret): every
// dial, whatever the host, reaches the test server.
func fakeLampac(t *testing.T, h http.Handler) *Lampac {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	l, err := NewLampac("http://lampac.test:9118/?uid=secret")
	if err != nil {
		t.Fatal(err)
	}
	l.HTTP = &http.Client{Transport: tr}
	return l
}

// testDB is an empty addon database with the addon's migrations
// (internal/store/migrations) applied in order.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "online.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	files, err := filepath.Glob(filepath.Join("..", "store", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return d
}
