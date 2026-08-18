package store

import (
	"context"
	"path/filepath"
	"testing"

	"codemap/extract"
	"codemap/parse"
	"codemap/resolve"
)

// TestSearchByTypeParity verifies the minimal three-clause LIKE set returns the
// identical rows the former six-clause set (three '%.' || ? variants) returned.
func TestSearchByTypeParity(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "package p\n" +
			"type Widget struct{}\n" +
			"func Use(w Widget, n int) {}\n" +
			"func Consume(w Widget) {}\n" +
			"func Make() *Widget { return nil }\n",
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

	typeName := "Widget"
	escaped := escapeLike(typeName)

	// The previous six-clause query, reproduced for comparison.
	oldQuery := `SELECT ` + symbolColumns + ` ` + symbolFrom + `
		WHERE s.signature LIKE '%' || ? || ' %' ESCAPE '\'
		   OR s.signature LIKE '%' || ? || ')%' ESCAPE '\'
		   OR s.signature LIKE '%' || ? || ',%' ESCAPE '\'
		   OR s.signature LIKE '%.' || ? || ' %' ESCAPE '\'
		   OR s.signature LIKE '%.' || ? || ')%' ESCAPE '\'
		   OR s.signature LIKE '%.' || ? || ',%' ESCAPE '\'
		ORDER BY s.qualified_name`
	oldRows, err := s.db.QueryContext(context.Background(), oldQuery,
		escaped, escaped, escaped, escaped, escaped, escaped)
	if err != nil {
		t.Fatal(err)
	}
	oldSyms, err := scanSymbols(oldRows)
	if err != nil {
		t.Fatal(err)
	}

	newSyms, err := s.SearchByType(typeName, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(newSyms) != len(oldSyms) {
		t.Fatalf("SearchByType(%q): %d results, old query had %d", typeName, len(newSyms), len(oldSyms))
	}
	for i := range newSyms {
		if newSyms[i].QualifiedName != oldSyms[i].QualifiedName {
			t.Fatalf("result %d mismatch: new %q, old %q", i, newSyms[i].QualifiedName, oldSyms[i].QualifiedName)
		}
	}
	if len(newSyms) == 0 {
		t.Fatal("expected at least one match for type Widget")
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
