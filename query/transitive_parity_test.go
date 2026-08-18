package query_test

import (
	"path/filepath"
	"sort"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

func fixtureStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	res := resolve.Run(pr)

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func edgeKeys(edges []query.EdgeDetail) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.FromRef+"|"+e.ToRef)
	}
	sort.Strings(out)
	return out
}

func storeEdgeKeys(edges []store.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.FromRef+"|"+e.ToRef)
	}
	sort.Strings(out)
	return out
}

// TestTransitiveImportsParityThreePaths verifies the three transitive-import
// consumers observe the same edge set on a self-contained fixture: the query
// TransitiveImports tool, the DependencyFlow composite, and the store-level
// method used by the legacy fallback.
func TestTransitiveImportsParityThreePaths(t *testing.T) {
	s := fixtureStore(t, "../testdata/depfixture")

	for _, pkg := range []string{
		"codemap/testdata/depfixture/top",
		"codemap/testdata/depfixture/middle",
		"codemap/testdata/depfixture/base",
	} {
		fromQuery, err := query.TransitiveImports(s, pkg)
		if err != nil {
			t.Fatalf("%s: TransitiveImports: %v", pkg, err)
		}
		flow, err := query.DependencyFlow(s, pkg)
		if err != nil {
			t.Fatalf("%s: DependencyFlow: %v", pkg, err)
		}
		fromStore, err := s.TransitiveImports(pkg)
		if err != nil {
			t.Fatalf("%s: store.TransitiveImports: %v", pkg, err)
		}

		want := edgeKeys(fromQuery)
		if got := edgeKeys(flow.TransitiveImports); !sameStrings(got, want) {
			t.Errorf("%s: DependencyFlow transitive imports differ from TransitiveImports:\n got=%v\nwant=%v", pkg, got, want)
		}
		if got := storeEdgeKeys(fromStore); !sameStrings(got, want) {
			t.Errorf("%s: store.TransitiveImports differ from query TransitiveImports:\n got=%v\nwant=%v", pkg, got, want)
		}
	}

	// top reaches middle and base (2 import edges); middle reaches base (1).
	if got := edgeKeys(mustTransitive(t, s, "codemap/testdata/depfixture/top")); len(got) != 2 {
		t.Errorf("top should have 2 transitive import edges (top->middle, middle->base), got %v", got)
	}
	if got := edgeKeys(mustTransitive(t, s, "codemap/testdata/depfixture/middle")); len(got) != 1 {
		t.Errorf("middle should have 1 transitive import edge (middle->base), got %v", got)
	}
	if got := edgeKeys(mustTransitive(t, s, "codemap/testdata/depfixture/base")); len(got) != 0 {
		t.Errorf("base should have 0 transitive import edges, got %v", got)
	}
}

func mustTransitive(t *testing.T, s *store.Store, pkg string) []query.EdgeDetail {
	t.Helper()
	edges, err := query.TransitiveImports(s, pkg)
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
