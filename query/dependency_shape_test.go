package query

import (
	"path/filepath"
	"sort"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

func setupQueryStoreAt(t *testing.T, root string) *store.Store {
	t.Helper()
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	parseResult, err := parse.Run(abs)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store create: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("store write: %v", err)
	}
	return s
}

func TestDependencyLayersFixture(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/depfixture")

	result, err := DependencyLayers(s, false, 5)
	if err != nil {
		t.Fatalf("DependencyLayers: %v", err)
	}

	if len(result.Layers) == 0 {
		t.Fatal("expected non-empty layers")
	}
	levelByPkg := make(map[string]int)
	for _, l := range result.Layers {
		for _, p := range l.Packages {
			levelByPkg[p] = l.Level
		}
	}
	t.Logf("layers: %+v", result.Layers)
	t.Logf("hubs: %+v", result.Hubs)
}

func TestDependencyLayersExcludesExternal(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/depfixture")

	result, err := DependencyLayers(s, false, 5)
	if err != nil {
		t.Fatalf("DependencyLayers: %v", err)
	}
	hasExternal := false
	for _, l := range result.Layers {
		for _, p := range l.Packages {
			if p == "fmt" || p == "testing" {
				hasExternal = true
			}
		}
	}
	if hasExternal {
		t.Errorf("expected no external packages in layers, got: %+v", result.Layers)
	}
}

func TestDependencyLayersHubsOrdered(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/depfixture")

	result, err := DependencyLayers(s, false, 5)
	if err != nil {
		t.Fatalf("DependencyLayers: %v", err)
	}
	if len(result.Hubs) == 0 {
		t.Fatal("expected non-empty hubs")
	}
	for i := 1; i < len(result.Hubs); i++ {
		if result.Hubs[i-1].FanIn < result.Hubs[i].FanIn {
			t.Errorf("hubs not sorted by fan_in desc: %+v then %+v", result.Hubs[i-1], result.Hubs[i])
		}
	}
}

func TestDependencyFlowFixture(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/depfixture")
	pkgs, _ := s.ListPackages()
	var middle string
	for _, p := range pkgs {
		if p.Name == "middle" {
			middle = p.Path
		}
	}
	if middle == "" {
		t.Fatal("could not find middle package")
	}

	result, err := DependencyFlow(s, middle)
	if err != nil {
		t.Fatalf("DependencyFlow: %v", err)
	}
	if result.Package != middle {
		t.Errorf("package: got %s want %s", result.Package, middle)
	}
	imports := map[string]bool{}
	for _, e := range result.Imports {
		imports[e.ToRef] = true
	}
	hasBaseImport := false
	for _, e := range result.Imports {
		t.Logf("import: %s -> %s", e.FromRef, e.ToRef)
		if e.ToRef == "codemap/testdata/depfixture/base" {
			hasBaseImport = true
		}
	}
	if !hasBaseImport {
		t.Errorf("expected base in imports, got %v", sortedKeys(imports))
	}
	importers := map[string]bool{}
	for _, e := range result.Importers {
		importers[e.FromRef] = true
	}
	hasTopImporter := false
	for _, e := range result.Importers {
		t.Logf("importer: %s -> %s", e.FromRef, e.ToRef)
		if e.FromRef == "top" {
			hasTopImporter = true
		}
	}
	if !hasTopImporter {
		t.Errorf("expected top as importer, got %v", sortedKeys(importers))
	}
	if len(result.TransitiveImports) == 0 {
		t.Error("expected non-empty transitive imports")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestDependencyFlowNotFound(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/depfixture")
	_, err := DependencyFlow(s, "nonexistent/pkg")
	if err == nil {
		t.Fatal("expected error for missing package")
	}
}
