package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"codemap/extract"
	"codemap/parse"
	"codemap/resolve"
)

// TestSearchByTypeCoversParametersReturnsAndFields pins that a type is found
// wherever it is declared in a signature, not only as a parameter: a bare
// return type (which ends the signature) and a struct field type were both
// invisible, so type_usage and blast_radius under-reported usages.
func TestSearchByTypeCoversParametersReturnsAndFields(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "package p\n" +
			"type Widget struct{}\n" +
			"type WidgetExtra struct{}\n" +
			"type Holder struct {\n\tW Widget\n}\n" +
			"func Use(w Widget, n int) {}\n" +
			"func Consume(w Widget) {}\n" +
			"func Make() *Widget { return nil }\n" +
			"func TakesExtra(w WidgetExtra) {}\n" +
			"func Unrelated() {}\n",
	})
	res, _ := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}

	syms, err := s.SearchByType("Widget", false)
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]bool, len(syms))
	for _, sym := range syms {
		got[sym.QualifiedName] = true
	}
	for _, want := range []string{"atomic.example.Use", "atomic.example.Consume", "atomic.example.Make", "atomic.example.Holder"} {
		if !got[want] {
			t.Errorf("%s uses Widget but SearchByType missed it (got %v)", want, got)
		}
	}
	for _, notWant := range []string{"atomic.example.TakesExtra", "atomic.example.Unrelated", "atomic.example.WidgetExtra"} {
		if got[notWant] {
			t.Errorf("%s does not use Widget but SearchByType returned it", notWant)
		}
	}
}

// TestRepoIDForRefMappedParity verifies the in-memory repo attribution used for
// contract rows matches the previous per-row SQL lookup on grounded package rows.
func TestRepoIDForRefMappedParity(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	for _, row := range []struct {
		path   string
		repoID int64
	}{
		{"example.com/alpha/a", 10},
		{"example.com/beta/b", 20},
		{"example.com/alpha/a/sub", 10},
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO packages (path, name, dir, repo_id) VALUES (?, 'p', '/d', ?)`,
			row.path, row.repoID); err != nil {
			t.Fatal(err)
		}
	}

	refs := []string{
		"example.com/alpha/a.Func",
		"example.com/alpha/a.Type.Method",
		"example.com/alpha/a/sub.Deep",
		"example.com/beta/b.Other",
		"external.com/unknown.X", // no owning package in the index
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	repoByPath, err := readPackageRepoMap(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}

	for _, ref := range refs {
		mapped := repoIDForRefMapped(ref, repoByPath)

		// The legacy SQL lookup, reproduced for comparison.
		var legacy int64
		err := tx.QueryRowContext(ctx, `
			SELECT p.repo_id FROM packages p
			WHERE ? = p.path OR ? LIKE p.path || '.%'
			ORDER BY length(p.path) DESC LIMIT 1`, ref, ref).Scan(&legacy)

		var expected int64
		if err == nil {
			expected = legacy
		}
		if mapped != expected {
			t.Errorf("repoIDForRefMapped(%q) = %d, legacy SQL gave %d", ref, mapped, expected)
		}
	}
}

// TestCountHelpersSingleQuery verifies the Overview-support queries each map to
// one prepared statement, where the pre-change code issued one query per
// package plus a full edge list load.
func TestCountHelpersSingleQuery(t *testing.T) {
	res := parseResolve(t, filepath.Join("..", "testdata", "fixtures", "dedup"))

	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}

	c.prepares.Store(0)
	if _, err := s.ImportCounts(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CountEdges(); err != nil {
		t.Fatal(err)
	}
	if got := c.prepares.Load(); got != 2 {
		t.Fatalf("ImportCounts + CountEdges used %d queries, want 2 (one each)", got)
	}
}

// TestOverviewImportCounts verifies buildPackageSummary's import counts equal
// the actual per-package import edge counts.
func TestOverviewImportCounts(t *testing.T) {
	res := &resolve.Result{
		Packages: []parse.PackageInfo{
			{ImportPath: "dep/base", Name: "base"},
			{ImportPath: "dep/top", Name: "top"},
		},
		Edges: []resolve.ResolvedEdge{
			{Edge: extract.Edge{FromRef: "dep/top", ToRef: "dep/base", EdgeType: "imports"}},
		},
	}

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}

	counts, err := s.ImportCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts["dep/top"] != 1 || counts["dep/base"] != 0 {
		t.Fatalf("import counts = %+v, want dep/top=1, dep/base=0", counts)
	}
	total, err := s.CountEdges()
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("CountEdges = %d, want 1", total)
	}
}

// TestDuplicateInitSymbolsSurviveIndexing pins that identity is the declaration
// site, not the qualified name. Every file in a registration-style package
// declares `func init()`, so keying on the qualified name alone collapsed them
// into one row and left the other files looking empty.
func TestDuplicateInitSymbolsSurviveIndexing(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a.go": "package p\n\nfunc init() {}\n\nfunc A() {}\n",
		"b.go": "package p\n\nfunc init() {}\n",
		"c.go": "package p\n\nfunc init() {}\n",
	})
	res, _ := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}

	inits, err := s.SearchByQualifiedNamePrefix("atomic.example.init", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inits) != 3 {
		t.Fatalf("expected 3 init declarations, got %d: %+v", len(inits), inits)
	}
	files := map[string]bool{}
	for _, sym := range inits {
		files[sym.PosFile] = true
	}
	if len(files) != 3 {
		t.Fatalf("expected one init per file, got files %v", files)
	}

	for _, name := range []string{"b.go", "c.go"} {
		syms, err := s.SearchSymbolsByFile(name, "", nil, false, GeneratedAny)
		if err != nil {
			t.Fatal(err)
		}
		if len(syms) != 1 {
			t.Errorf("%s: expected its init symbol, got %d rows", name, len(syms))
		}
	}
}

// TestReindexIsIdempotentWithDuplicateNames pins that the declaration key still
// makes a second write of the same index a no-op rather than a duplicate.
func TestReindexIsIdempotentWithDuplicateNames(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a.go": "package p\n\nfunc init() {}\n",
		"b.go": "package p\n\nfunc init() {}\n",
	})
	res, _ := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, first, _, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, second, _, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("re-index changed symbol count: %d -> %d", first, second)
	}
}

// TestFileContentsByRepoIncludesSingleRepoBucket pins that a single-repo index,
// whose rows carry no repo assignment, still yields its file contents. Dropping
// the "" bucket made runtime-contract extraction see zero files.
func TestFileContentsByRepoIncludesSingleRepoBucket(t *testing.T) {
	dir := writeModule(t, map[string]string{"p.go": "package p\n\nfunc A() {}\n"})
	res, _ := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, map[string]string{dir + "/p.go": "package p\n\nfunc A() {}\n"}, nil); err != nil {
		t.Fatal(err)
	}

	files, err := s.FileContentsByRepo()
	if err != nil {
		t.Fatal(err)
	}
	if len(files[""]) != 1 {
		t.Fatalf("single-repo bucket = %v, want one file", files)
	}
}

// TestConcurrentWritersShareOneDatabase pins that two Store handles on the same
// file can both write. Deferred transactions let both start as readers and then
// deadlock on upgrade, which busy_timeout cannot break; the loser saw
// "database is locked (5) (SQLITE_BUSY)". Immediate transactions take the write
// lock at BEGIN, so the second writer waits instead of failing.
func TestConcurrentWritersShareOneDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	first, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	// Each transaction reads first, then writes. With deferred transactions both
	// hold a SHARED lock and both then ask to upgrade; SQLite reports that as a
	// deadlock and returns SQLITE_BUSY immediately, without consulting
	// busy_timeout. Immediate transactions take RESERVED at BEGIN, so the second
	// one waits for the first to commit instead.
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, st := range []*Store{first, second} {
		go func(st *Store) {
			<-start
			errs <- st.withTx(context.Background(), func(tx *sql.Tx) error {
				var n int
				if err := tx.QueryRowContext(context.Background(),
					`SELECT COUNT(*) FROM meta`).Scan(&n); err != nil {
					return err
				}
				time.Sleep(50 * time.Millisecond)
				if _, err := tx.ExecContext(context.Background(),
					`INSERT OR REPLACE INTO meta (key, value) VALUES ('concurrent_writer', '1')`); err != nil {
					return err
				}
				return nil
			})
		}(st)
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent writer failed: %v", err)
		}
	}
}

// TestImportCountsExcludeTestFiles pins that a _test.go import is not part of
// the package's runtime dependency surface. Counting it made overview and
// package report emak/config as 12 imports where go list says 10.
func TestImportCountsExcludeTestFiles(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a/a.go":      "package a\n\nimport \"fmt\"\n\nfunc Use() { fmt.Println() }\n",
		"a/a_test.go": "package a\n\nimport (\n\t\"testing\"\n\t\"github.com/only/test\"\n)\n\nfunc TestUse(t *testing.T) { _ = testing.Short() }\n",
	})
	res, files := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, files, nil); err != nil {
		t.Fatal(err)
	}

	counts, err := s.ImportCounts()
	if err != nil {
		t.Fatal(err)
	}
	var pkgPath string
	for p := range counts {
		pkgPath = p
	}
	if counts[pkgPath] != 1 {
		t.Errorf("import count for %s = %d, want 1 (fmt only; testing and the test-only import do not count)",
			pkgPath, counts[pkgPath])
	}
}
