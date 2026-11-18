package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// coreTableSQL is SQL that reads or writes one of the core's tables: after
// FROM, JOIN, INTO or UPDATE (UPDATE OR IGNORE …), the table itself or one
// of a comma list (FROM items i, files f), maybe quoted or as
// main.<table>. online_progress is not in the list: the addon's own
// database has a table of that name (the core's copy of it stays in
// vault.db for a rollback, and nothing here touches it).
var coreTableSQL = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE(?:\s+OR\s+\w+)?)\s+` +
	`(?:[\w."` + "`" + `]+(?:\s+(?:AS\s+)?\w+)?\s*,\s*)*` +
	`["` + "`" + `\[]?(?:main\.)?["` + "`" + `\[]?` +
	`(items|item_titles|files|users|progress|progress_cleared|series_tracks|tracked_titles|title_marks|item_owners|import_groups|import_tracks|` +
	// the core's tracking, fit and letters tables (internal/db/migrations)
	`episode_sources|title_episodes|title_choices|tracked_scans|external_audio_fit|watchlist|kid_requests|` +
	`notify_addon_prefs|notify_events|notify_prefs|notify_seen|notify_state|notify_verify_log)\b`)

// sqlGuarded are the packages that reach the core only through addonkit
// (a folder of internal/ that is not written yet is skipped). The module's
// own folder, the program's main package, is always read.
var sqlGuarded = []string{"online", "store", "boot", "hooks", "coreadapt", "importer"}

// sqlAllowed are the only core-table literals a guarded package may hold,
// by file (relative to internal/, a file of the module's folder by its
// name): the one-time import's reads of its snapshot of vault.db. Each
// entry must still be there (a stale one fails).
var sqlAllowed = map[string][]string{
	"store/import.go": {`SELECT id, uid FROM users`},
	// a fixture profile without a uid, on the test's own vault.db
	"store/store_test.go": {`UPDATE users SET uid=NULL WHERE id=3`},
}

// The addon never queries a core table: no string of a guarded package
// (each literal, and each +-chain of literals folded into one; tests
// included; //go:build ignore files, which do not build, left out) names
// one in SQL, but for sqlAllowed.
func TestNoCoreTableSQL(t *testing.T) {
	bad, walked, seen, err := guardScan(".", sqlGuarded, sqlAllowed)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Error(b)
	}
	if walked == 0 {
		t.Fatal("no Go files walked")
	}
	for file, lits := range sqlAllowed {
		for _, s := range lits {
			if !seen[file+"\x00"+s] {
				t.Errorf("sqlAllowed: %s no longer holds %q: drop the entry", file, s)
			}
		}
	}
}

// guardScan reads the Go files of the module's own folder (the main
// package, no folder under it) and of internal/<pkg> for each pkg (a
// folder not written yet is skipped), all under base. bad holds a line
// per core-table literal not in allowed, walked the files read, seen the
// file + "\x00" + literal of allowed that were met.
func guardScan(base string, pkgs []string, allowed map[string][]string) (bad []string, walked int, seen map[string]bool, err error) {
	seen = map[string]bool{}
	roots := []string{"."}
	for _, pkg := range pkgs {
		roots = append(roots, filepath.Join("internal", pkg))
	}
	for _, root := range roots {
		if _, serr := os.Stat(filepath.Join(base, root)); os.IsNotExist(serr) {
			continue // not written yet
		}
		err = filepath.WalkDir(filepath.Join(base, root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if root == "." && p != base {
					return fs.SkipDir // the module's folder only: the packages are the other roots
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || (root == "." && d.Name() == "sql_guard_test.go") {
				return nil // this file's own samples are core-table SQL
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			if ignored(f) {
				return nil
			}
			walked++
			rel, _ := filepath.Rel(base, p)
			rel = strings.TrimPrefix(filepath.ToSlash(rel), "internal/")
			check := func(pos token.Pos, s string) {
				m := coreTableSQL.FindString(s)
				if m == "" {
					return
				}
				for _, ok := range allowed[rel] {
					if s == ok {
						seen[rel+"\x00"+s] = true
						return
					}
				}
				bad = append(bad, fset.Position(pos).String()+": SQL on a core table ("+strconv.Quote(m)+"): use the core's API (coreadapt)")
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BinaryExpr:
					if x.Op == token.ADD {
						if s, ok := folded(x); ok {
							check(x.Pos(), s)
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						check(x.Pos(), unquote(x))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, 0, nil, err
		}
	}
	return bad, walked, seen, nil
}

// folded is a +-chain with a string literal in it as one string, other
// operands as "?" (so "FROM " + "items" and "FROM "+q+" JOIN items" are
// read whole); ok false for a chain without a string literal.
func folded(e *ast.BinaryExpr) (string, bool) {
	var b strings.Builder
	lit := false
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		switch v := x.(type) {
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				walk(v.X)
				walk(v.Y)
				return
			}
		case *ast.ParenExpr:
			walk(v.X)
			return
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				b.WriteString(unquote(v))
				lit = true
				return
			}
		}
		b.WriteString("?")
	}
	walk(e)
	return b.String(), lit
}

func unquote(lit *ast.BasicLit) string {
	if s, err := strconv.Unquote(lit.Value); err == nil {
		return s
	}
	return lit.Value
}

// ignored: the file starts with //go:build ignore.
func ignored(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.TrimSpace(c.Text) == "//go:build ignore" {
				return true
			}
		}
	}
	return false
}

// The guard's pattern catches the spellings a query may use, and not the
// addon's own tables.
func TestCoreTableSQLPattern(t *testing.T) {
	for _, s := range []string{
		"SELECT 1 FROM files", "UPDATE OR IGNORE files SET x=1", "SELECT * FROM main.items",
		`SELECT * FROM "items"`, "SELECT 1 FROM online_progress p, items i WHERE p.tmdb_id=i.tmdb_id",
		"INSERT OR REPLACE INTO users(id) VALUES(1)", "x JOIN item_titles t ON 1",
		"SELECT * FROM episode_sources WHERE kind='online'", "UPDATE title_episodes SET watched=1", "SELECT 1 FROM tracked_scans",
		"INSERT INTO title_choices(user_id) VALUES(1)", "SELECT 1 FROM external_audio_fit", "SELECT 1 FROM notify_events",
		"SELECT position_ms FROM progress WHERE user_id=?", "INSERT INTO progress_cleared(user_id) VALUES(1)",
	} {
		if !coreTableSQL.MatchString(s) {
			t.Errorf("not caught: %q", s)
		}
	}
	for _, s := range []string{
		"SELECT 1 FROM online_progress", "UPDATE online_progress SET position_ms=1", "INSERT INTO online_progress(user_uid) VALUES(?)",
		"DELETE FROM online_progress WHERE user_uid NOT IN (?)", "the files from the stream",
	} {
		if coreTableSQL.MatchString(s) {
			t.Errorf("caught: %q", s)
		}
	}
}

// The guard reads the module's own folder (the main package) and the
// guarded packages, not a folder it does not guard, and a literal of
// sqlAllowed passes only where it is listed.
func TestGuardScan(t *testing.T) {
	base := t.TempDir()
	put := func(name, body string) {
		t.Helper()
		p := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("main.go", "package main\n\nconst q = \"SELECT 1 FROM users\"\n")
	put("internal/store/s.go", "package store\n\nconst ok = \"SELECT 1 FROM online_progress\"\nvar q = \"SELECT 1 FROM \" + \"items\"\n")
	put("internal/corecopy/c.go", "package c\n\nconst q = \"SELECT 1 FROM files\"\n")  // not guarded
	put("internal/other/o.go", "package other\n\nconst q = \"SELECT 1 FROM files\"\n") // not guarded
	put("sub/deeper.go", "package sub\n\nconst q = \"SELECT 1 FROM files\"\n")         // a folder under the module's: not its own package
	put("internal/boot/b.go", "//go:build ignore\n\npackage boot\n\nconst q = \"SELECT 1 FROM files\"\n")
	bad, walked, _, err := guardScan(base, []string{"store", "boot", "hooks"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if walked != 2 || len(bad) != 2 || !strings.Contains(bad[0], "main.go") || !strings.Contains(bad[1], "s.go") {
		t.Fatalf("walked %d, bad %q", walked, bad)
	}
	bad, _, seen, err := guardScan(base, []string{"store"}, map[string][]string{"main.go": {"SELECT 1 FROM users"}})
	if err != nil || len(bad) != 1 || !strings.Contains(bad[0], "s.go") || !seen["main.go\x00SELECT 1 FROM users"] {
		t.Fatalf("allowed: %v %q %v", err, bad, seen)
	}
}
