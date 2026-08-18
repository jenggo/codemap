package query

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

// setupSwallowTestStore indexes the given fixture into a temp database and
// returns the store plus its path, so tests can break specific tables through
// a second sqlite connection while the store's own connection stays open.
func setupSwallowTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	abs, err := filepath.Abs("../testdata/simple")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	parseResult, err := parse.Run(abs)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("store write: %v", err)
	}
	return s, dbPath
}

// setupSwallowGitStore indexes a git repo checkout, returning the store and the
// database path.
func setupSwallowGitStore(t *testing.T, root string) (*store.Store, string) {
	t.Helper()
	parseResult, err := parse.Run(root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s, dbPath
}

// execOnDB runs DDL against the database file through a separate connection so
// a test can break a specific table while the store's connection stays open.
func execOnDB(t *testing.T, dbPath, ddl string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), ddl); err != nil {
		t.Fatalf("exec %q: %v", ddl, err)
	}
}

func TestOverviewPropagatesEdgeError(t *testing.T) {
	s, dbPath := setupSwallowTestStore(t)
	execOnDB(t, dbPath, `DROP TABLE edges`)
	_, err := Overview(s)
	if err == nil {
		t.Fatal("expected error when edge queries fail, not a silent TotalEdges=0")
	}
	if !strings.Contains(err.Error(), "overview") {
		t.Fatalf("expected overview-context error, got: %v", err)
	}
}

func TestOverviewPropagatesWorkspaceHealthError(t *testing.T) {
	s, dbPath := setupSwallowTestStore(t)
	// Break only ListRepos (used by WorkspaceHealth) while packages and edges
	// stay intact: rename repos.indexed_at so ListRepos' SELECT fails but
	// ListPackages (which only reads r.module_path) still works.
	execOnDB(t, dbPath, `ALTER TABLE repos RENAME COLUMN indexed_at TO indexed_at_bak`)
	_, err := Overview(s)
	if err == nil {
		t.Fatal("expected error when WorkspaceHealth fails, not silently missing repos")
	}
	if !strings.Contains(err.Error(), "workspace health") {
		t.Fatalf("expected workspace-health error, got: %v", err)
	}
}

func TestPackagePropagatesEdgeError(t *testing.T) {
	s, dbPath := setupSwallowTestStore(t)
	execOnDB(t, dbPath, `DROP TABLE edges`)
	_, err := Package(s, "codemap/testdata/simple")
	if err == nil {
		t.Fatal("expected error when EdgesFrom fails, not a silent ImportCount=0")
	}
}

func TestImportsOfPropagatesKnownModulesError(t *testing.T) {
	s, dbPath := setupSwallowTestStore(t)
	execOnDB(t, dbPath, `ALTER TABLE repos RENAME COLUMN indexed_at TO indexed_at_bak`)
	_, err := ImportsOf(s, "codemap/testdata/simple")
	if err == nil {
		t.Fatal("expected error when KnownModulePaths fails, not silent empty state")
	}
}

func TestFindPathAbsenceVsError(t *testing.T) {
	s, dbPath := setupSwallowTestStore(t)

	// Healthy store: two unrelated symbols have no path — that is absence.
	_, err := FindPath(s, "codemap/testdata/simple.NewParser", "codemap/testdata/simple.helper", 5)
	if !errors.Is(err, ErrNoPath) {
		t.Fatalf("expected ErrNoPath for absent path, got: %v", err)
	}

	// Broken store: a failed edge fetch is an error, never reported as absence.
	execOnDB(t, dbPath, `DROP TABLE edges`)
	_, err = FindPath(s, "codemap/testdata/simple.NewParser", "codemap/testdata/simple.helper", 5)
	if err == nil {
		t.Fatal("expected error when edge fetch fails")
	}
	if errors.Is(err, ErrNoPath) {
		t.Fatal("edge-fetch failure must not be reported as no-path")
	}
}

func TestContextBundlePropagatesPartialFailure(t *testing.T) {
	s, dbPath := setupSwallowTestStore(t)
	execOnDB(t, dbPath, `DROP TABLE edges`)
	_, err := ContextBundle(s, "codemap/testdata/simple.NewParser", 0)
	if err == nil {
		t.Fatal("expected error when a bundle edge fetch fails, not a silent-nil bundle")
	}
}

func TestGetBlastRadiusMissingSymbol(t *testing.T) {
	s, _ := setupSwallowTestStore(t)

	_, err := GetBlastRadius(s, "codemap/testdata/simple.NoSuch", 3)
	if err == nil {
		t.Fatal("expected error for missing symbol, not a zero-valued blast radius")
	}
	if !errors.Is(err, errSymbolMissing) {
		t.Fatalf("expected symbol-missing error, got: %v", err)
	}

	br, err := GetBlastRadius(s, "codemap/testdata/simple.NewParser", 3)
	if err != nil {
		t.Fatalf("GetBlastRadius on existing symbol: %v", err)
	}
	if br == nil {
		t.Fatal("expected non-nil blast radius")
	}
}

func TestChangedSymbolsPropagatesBlastError(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s, dbPath := setupSwallowGitStore(t, repo)
	execOnDB(t, dbPath, `DROP TABLE edges`)
	_, err := ChangedSymbols(s, repo, "HEAD", true, false, false)
	if err == nil {
		t.Fatal("expected error when a blast-radius edge fetch fails, not a silent nil blast radius")
	}
	if errors.Is(err, errSymbolMissing) {
		t.Fatal("expected a real query error, not symbol-missing")
	}
}
