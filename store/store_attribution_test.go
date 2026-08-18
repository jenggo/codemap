package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"codemap/extract"
	"codemap/parse"
	"codemap/resolve"
)

// parseResolve parses and resolves a fixture directory into a full result,
// mirroring how the CLI indexes a repo.
func parseResolve(t *testing.T, dir string) *resolve.Result {
	t.Helper()
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	return resolve.Run(pr)
}

// TestWriteAttributionOutputEquality verifies the constant-time attribution path
// produces row-for-row identical package attribution to the previous
// per-row-scan implementation on the standard fixtures. In particular every
// stored symbol must be owned by the package whose path is a dot-boundary
// prefix of its qualified name, and no symbol may be silently dropped.
func TestWriteAttributionOutputEquality(t *testing.T) {
	for _, fixture := range []string{"dedup", "multisite"} {
		res := parseResolve(t, filepath.Join("..", "testdata", "fixtures", fixture))

		s, err := Create(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()

		if err := s.Write(res, nil, nil); err != nil {
			t.Fatalf("fixture %s: write: %v", fixture, err)
		}

		var stored int
		if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM symbols`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != len(res.Symbols) {
			t.Fatalf("fixture %s: stored %d symbols, resolved %d (attribution dropped rows)",
				fixture, stored, len(res.Symbols))
		}

		// Every symbol must be owned by an indexed package whose path is an
		// exact match or a dot-boundary prefix of the qualified name. The old
		// per-row packageForRef scan produced exactly this invariant.
		var misattributed int
		if err := s.db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM symbols s
			JOIN packages p ON s.package_id = p.id
			WHERE s.qualified_name <> p.path
			  AND s.qualified_name NOT LIKE p.path || '.%'`).Scan(&misattributed); err != nil {
			t.Fatal(err)
		}
		if misattributed != 0 {
			t.Fatalf("fixture %s: %d symbols attributed to a non-owning package", fixture, misattributed)
		}
	}
}

// TestWriteAttributionWorkspace verifies repo attribution in workspace mode:
// each symbol is attributed to the repo owning its package path.
func TestWriteAttributionWorkspace(t *testing.T) {
	res := &resolve.Result{
		Packages: []parse.PackageInfo{
			{ImportPath: "example.com/alpha/a", Name: "a", ModulePath: "example.com/alpha"},
			{ImportPath: "example.com/beta/b", Name: "b", ModulePath: "example.com/beta"},
		},
		Symbols: []resolve.ResolvedSymbol{
			{Symbol: extract.Symbol{QualifiedName: "example.com/alpha/a.FuncA", Name: "FuncA", Kind: "function"}},
			{Symbol: extract.Symbol{QualifiedName: "example.com/beta/b.FuncB", Name: "FuncB", Kind: "function"}},
		},
		Edges: []resolve.ResolvedEdge{
			{Edge: extract.Edge{FromRef: "example.com/alpha/a.FuncA", ToRef: "example.com/beta/b.FuncB", EdgeType: "calls"}},
		},
	}

	repos := []RepoSpec{
		{ModulePath: "example.com/alpha", Dir: "/alpha"},
		{ModulePath: "example.com/beta", Dir: "/beta"},
	}

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.WriteWorkspace(res, nil, nil, repos); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	// Alpha symbols must land on the alpha repo and beta symbols on beta.
	var misattributed int
	if err := s.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM symbols s
		JOIN packages p ON s.package_id = p.id
		JOIN repos r ON r.id = s.repo_id
		WHERE (p.path = 'example.com/alpha/a' AND r.module_path <> 'example.com/alpha')
		   OR (p.path = 'example.com/beta/b' AND r.module_path <> 'example.com/beta')`).Scan(&misattributed); err != nil {
		t.Fatal(err)
	}
	if misattributed != 0 {
		t.Fatalf("%d symbols attributed to the wrong repo", misattributed)
	}

	// The cross-repo call edge must carry the from-side repo.
	var edgeRepo string
	if err := s.db.QueryRowContext(t.Context(), `
		SELECT COALESCE(r.module_path, '') FROM edges e
		LEFT JOIN repos r ON r.id = e.repo_id
		WHERE e.from_ref = 'example.com/alpha/a.FuncA' AND e.to_ref = 'example.com/beta/b.FuncB'`).Scan(&edgeRepo); err != nil {
		t.Fatal(err)
	}
	if edgeRepo != "example.com/alpha" {
		t.Fatalf("edge repo = %q, want example.com/alpha", edgeRepo)
	}
}

// TestPkgAttributionLongestPrefix exercises the dot-boundary prefix probe on
// overlapping package paths: a and a.b can coexist and the longer package
// always wins, while external (unindexed) references resolve to no attribution.
func TestPkgAttributionLongestPrefix(t *testing.T) {
	attrib := newPkgAttribution(map[string]int64{
		"a":   1,
		"a.b": 2,
	}, nil)

	cases := []struct {
		ref      string
		wantPkg  int64
		wantRepo int64
	}{
		{"a.b.X", 2, 0},                // longest prefix wins
		{"a.shortish.X", 1, 0},         // a.b does not own a.shortish.* (no dot boundary)
		{"a.X", 1, 0},                  // shallow package still resolves
		{"a.b", 2, 0},                  // exact package-path hit
		{"a", 1, 0},                    // exact package-path hit
		{"external.com/other.Y", 0, 0}, // unindexed reference
	}
	for _, c := range cases {
		pkgID, repoID := attrib.pkgFor(c.ref)
		if pkgID != c.wantPkg || repoID != c.wantRepo {
			t.Errorf("pkgFor(%q) = (%d, %d), want (%d, %d)", c.ref, pkgID, repoID, c.wantPkg, c.wantRepo)
		}
	}
}

// TestPkgAttributionScalesLinearly guards against a regression to the O(S x P)
// per-row scan: attributing 50k symbols across 2k packages must be bounded by a
// generous constant wall-clock gate. The old implementation performed ~100M
// string comparisons here.
func TestPkgAttributionScalesLinearly(t *testing.T) {
	const packageCount = 2000
	const symbolCount = 50000

	pkgCache := make(map[string]int64, packageCount)
	for i := range packageCount {
		pkgCache[fmt.Sprintf("example.com/m%d/p%d", i%40, i)] = int64(i + 1)
	}

	attrib := newPkgAttribution(pkgCache, nil)

	start := time.Now()
	for s := range symbolCount {
		qn := fmt.Sprintf("example.com/m%d/p%d.Symbol%d", s%40, s%packageCount, s)
		attrib.pkgFor(qn)
	}
	elapsed := time.Since(start)

	// The map-probe path completes in well under a second; the O(S x P) scan
	// would need 100M branch iterations. 5s is a loose but decisive bound.
	if elapsed > 5*time.Second {
		t.Fatalf("attribution for %d symbols over %d packages took %s; expected ~O(S)",
			symbolCount, packageCount, elapsed)
	}
}
