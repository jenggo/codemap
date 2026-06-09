package codemap_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/render"
	"codemap/resolve"
	"codemap/store"
)

const satisfiesEdgeType = "satisfies"

var updateGolden = flag.Bool("update", false, "update golden files")

func TestGoldenSimple(t *testing.T) {
	runGoldenTest(t, "testdata/simple", "simple")
}

func TestGoldenInterfaces(t *testing.T) {
	runGoldenTest(t, "testdata/interfaces", "interfaces")
}

func TestGoldenEmbedding(t *testing.T) {
	runGoldenTest(t, "testdata/embedding", "embedding")
}

func TestGoldenMultipackage(t *testing.T) {
	runGoldenTest(t, "testdata/multipackage", "multipackage")
}

func runGoldenTest(t *testing.T, fixturePath string, name string) {
	t.Helper()

	absPath, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(parseResult.Errors) > 0 {
		for _, e := range parseResult.Errors {
			t.Logf("parse warning: %s: %s", e.File, e.Err)
		}
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	output := buildTestOutput(t, s)

	goldenPath := filepath.Join("testdata", name+".golden.json")

	if *updateGolden {
		if err := os.WriteFile(goldenPath, []byte(output), 0644); err != nil {
			t.Fatalf("failed to write golden file: %v", err)
		}
		t.Logf("updated golden file: %s", goldenPath)
		return
	}

	goldenData, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("failed to read golden file: %v (run with -update to create)", err)
	}

	if strings.TrimSpace(string(goldenData)) != strings.TrimSpace(output) {
		t.Errorf("output mismatch for %s\n\nGot:\n%s\n\nExpected (golden):\n%s", name, output, string(goldenData))
	}
}

func buildTestOutput(t *testing.T, s *store.Store) string {
	t.Helper()

	overview, err := query.Overview(s)
	if err != nil {
		t.Fatalf("overview error: %v", err)
	}

	pkgs, err := query.ListPackages(s)
	if err != nil {
		t.Fatalf("list packages error: %v", err)
	}

	type testOutput struct {
		Packages int                    `json:"packages"`
		Symbols  int                    `json:"symbols"`
		Edges    int                    `json:"edges"`
		Overview []query.PackageSummary `json:"overview"`
		ListPkgs []store.Package        `json:"list_packages"`
	}

	allPkgs, _ := s.ListPackages()
	allSyms := 0
	for _, p := range allPkgs {
		syms, err := s.SymbolsByPackage(p.Path, true)
		if err == nil {
			allSyms += len(syms)
		}
	}
	allEdges, _ := s.AllEdges()

	output := testOutput{
		Packages: len(pkgs),
		Symbols:  allSyms,
		Edges:    len(allEdges),
		Overview: overview.Packages,
		ListPkgs: pkgs,
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		t.Fatalf("json marshal error: %v", err)
	}

	return string(data)
}

func setupTestStore(t *testing.T, fixturePath string) *store.Store {
	t.Helper()

	absPath, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	return s
}

func TestIntegrationFullPipeline(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(parseResult.Packages) == 0 {
		t.Fatal("expected at least one package")
	}

	if len(parseResult.Files) == 0 {
		t.Fatal("expected at least one file")
	}

	resolveResult := resolve.Run(parseResult)

	if len(resolveResult.Symbols) == 0 {
		t.Fatal("expected at least one symbol")
	}

	s := setupTestStore(t, "testdata/simple")

	pkgs, err := s.ListPackages()
	if err != nil {
		t.Fatalf("list packages error: %v", err)
	}

	if len(pkgs) == 0 {
		t.Fatal("expected at least one package in store")
	}

	syms, err := s.SymbolsByPackage(pkgs[0].Path, false)
	if err != nil {
		t.Fatalf("symbols by package error: %v", err)
	}

	if len(syms) == 0 {
		t.Fatal("expected at least one symbol in store")
	}

	_, err = query.Overview(s)
	if err != nil {
		t.Fatalf("overview error: %v", err)
	}

	_, err = query.Search(s, "Parser")
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
}

func TestSearchResultHasNewFields(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	results, err := query.Search(s, "Parser")
	if err != nil {
		t.Fatalf("search error: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one search result")
	}

	for _, r := range results {
		if r.QualifiedName == "" {
			t.Error("expected non-empty QualifiedName")
		}
		if r.Kind == "" {
			t.Error("expected non-empty Kind")
		}
		if r.Signature == "" {
			t.Error("expected non-empty Signature")
		}
		if r.PosFile == "" {
			t.Error("expected non-empty PosFile")
		}
		if r.PosLine == 0 {
			t.Error("expected non-zero PosLine")
		}
	}
}

func TestSearchKindFilter(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	results, err := query.Search(s, "Parser", query.WithKind("function"))
	if err != nil {
		t.Fatalf("search error: %v", err)
	}

	for _, r := range results {
		if r.Kind != "function" {
			t.Errorf("expected kind 'function', got '%s' for %s", r.Kind, r.QualifiedName)
		}
	}
}

func TestSearchExportedFilter(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	results, err := query.Search(s, "Parser", query.WithExported(true))
	if err != nil {
		t.Fatalf("search error: %v", err)
	}

	for _, r := range results {
		if !r.Exported {
			t.Errorf("expected exported=true for %s", r.QualifiedName)
		}
	}
}

func TestSearchRelevanceOrdering(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	results, err := query.Search(s, "Parse")
	if err != nil {
		t.Fatalf("search error: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	exactMatchLast := -1
	prefixMatchLast := -1
	for i, r := range results {
		if r.QualifiedName == "codemap/testdata/simple.Parser.Parse" {
			exactMatchLast = i
		}
	}

	for i, r := range results {
		if r.QualifiedName == "codemap/testdata/simple.Parser" {
			prefixMatchLast = i
		}
	}

	if exactMatchLast >= prefixMatchLast {
		t.Error("expected exact match to appear before prefix match")
	}
}

func TestPackageQuery(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	pkg, err := query.Package(s, "codemap/testdata/simple")
	if err != nil {
		t.Fatalf("package query error: %v", err)
	}

	if pkg.Path != "codemap/testdata/simple" {
		t.Errorf("expected path 'codemap/testdata/simple', got '%s'", pkg.Path)
	}
	if pkg.Name != "simple" {
		t.Errorf("expected name 'simple', got '%s'", pkg.Name)
	}
	if len(pkg.ExportedSymbols) == 0 {
		t.Fatal("expected at least one exported symbol")
	}

	for _, sym := range pkg.ExportedSymbols {
		if sym.PosFile == "" {
			t.Errorf("expected PosFile for %s", sym.QualifiedName)
		}
		if sym.PosLine == 0 {
			t.Errorf("expected PosLine for %s", sym.QualifiedName)
		}
	}
}

func TestPackageQueryNotFound(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	_, err = query.Package(s, "nonexistent/pkg")
	if err == nil {
		t.Fatal("expected error for non-existent package")
	}
}

func TestMethodQuery(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	methods, err := query.MethodsOf(s, "Parser")
	if err != nil {
		t.Fatalf("methods of error: %v", err)
	}

	if len(methods) == 0 {
		t.Fatal("expected at least one method")
	}

	for _, m := range methods {
		if m.Kind != "method" {
			t.Errorf("expected kind 'method', got '%s' for %s", m.Kind, m.QualifiedName)
		}
		if m.Receiver == "" {
			t.Errorf("expected non-empty receiver for %s", m.QualifiedName)
		}
	}
}

func TestMethodQueryNotFound(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	_, err = query.MethodsOf(s, "NonExistentType")
	if err == nil {
		t.Fatal("expected error for non-existent type")
	}
}

func TestEdgeTypeFiltering(t *testing.T) {
	absPath, err := filepath.Abs("testdata/interfaces")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	edges, err := query.CallersOf(s, "codemap/testdata/interfaces.Reader")
	if err != nil {
		t.Fatalf("callers of error: %v", err)
	}

	hasSatisfies := false
	for _, e := range edges {
		if e.EdgeType == "satisfies" {
			hasSatisfies = true
			break
		}
	}
	if !hasSatisfies {
		t.Error("expected 'satisfies' edge type for interface callers")
	}

	edgesOnlyCalls, err := query.CallersOf(s, "codemap/testdata/interfaces.Reader", query.WithEdgeTypes("calls"))
	if err != nil {
		t.Fatalf("callers of with edge types error: %v", err)
	}

	for _, e := range edgesOnlyCalls {
		if e.EdgeType != "calls" {
			t.Errorf("expected only 'calls' edges, got '%s'", e.EdgeType)
		}
	}
}

func assertEdgePositions(t *testing.T, edges []query.EdgeDetail, label string) {
	t.Helper()
	for _, e := range edges {
		if e.EdgeType == satisfiesEdgeType {
			continue
		}
		if e.PosFile == "" {
			t.Errorf("expected PosFile for %s edge %s -> %s (type: %s)", label, e.FromRef, e.ToRef, e.EdgeType)
		}
		if e.PosLine == 0 {
			t.Errorf("expected non-zero PosLine for %s edge %s -> %s (type: %s)", label, e.FromRef, e.ToRef, e.EdgeType)
		}
	}
}

func TestEdgeDetailHasPositions(t *testing.T) {
	s := setupTestStore(t, "testdata/interfaces")

	edges, err := query.CallersOf(s, "codemap/testdata/interfaces.Buffer")
	if err != nil {
		t.Fatalf("callers of error: %v", err)
	}
	assertEdgePositions(t, edges, "caller")

	callees, err := query.CalleesOf(s, "codemap/testdata/interfaces.Process")
	if err != nil {
		t.Fatalf("callees of error: %v", err)
	}
	assertEdgePositions(t, callees, "callee")

	showResult, err := query.Show(s, "codemap/testdata/interfaces.Buffer")
	if err != nil {
		t.Fatalf("show error: %v", err)
	}
	assertEdgePositions(t, showResult.IncomingEdges, "incoming")

	if len(showResult.OutgoingEdges) > 0 {
		for _, e := range showResult.OutgoingEdges {
			if e.PosFile != "" {
				break
			}
		}
	}
}

func TestNullToEmptySlice(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	showResult, err := query.Show(s, "codemap/testdata/simple.Parser.Parse")
	if err != nil {
		t.Fatalf("show error: %v", err)
	}

	data, err := json.Marshal(showResult)
	if err != nil {
		t.Fatalf("json marshal error: %v", err)
	}

	var parsed struct {
		IncomingEdges []any `json:"incoming_edges"`
		OutgoingEdges []any `json:"outgoing_edges"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json unmarshal error: %v", err)
	}
	if parsed.IncomingEdges == nil {
		t.Error("incoming_edges should not be null, should be empty array")
	}
	if parsed.OutgoingEdges == nil {
		t.Error("outgoing_edges should not be null, should be empty array")
	}

	callers, err := query.CallersOf(s, "codemap/testdata/simple.NewParser")
	if err != nil {
		t.Fatalf("callers of error: %v", err)
	}
	callersData, err := json.Marshal(callers)
	if err != nil {
		t.Fatalf("json marshal error: %v", err)
	}
	if string(callersData) == "null" {
		t.Error("callers result should not be null")
	}
}

func TestEdgesByTypeQuery(t *testing.T) {
	absPath, err := filepath.Abs("testdata/interfaces")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	satisfies, err := query.EdgesByType(s, "satisfies")
	if err != nil {
		t.Fatalf("edges by type error: %v", err)
	}
	for _, e := range satisfies {
		if e.EdgeType != "satisfies" {
			t.Errorf("expected edge type 'satisfies', got '%s'", e.EdgeType)
		}
	}

	calls, err := query.EdgesByType(s, "calls")
	if err != nil {
		t.Fatalf("edges by type error: %v", err)
	}
	for _, e := range calls {
		if e.PosFile == "" {
			t.Error("expected PosFile on calls edge")
		}
		if e.PosLine == 0 {
			t.Error("expected non-zero PosLine on calls edge")
		}
	}

	all, err := query.AllEdges(s)
	if err != nil {
		t.Fatalf("all edges error: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("expected at least one edge")
	}
	for _, e := range all {
		if e.FromRef == "" || e.ToRef == "" {
			t.Errorf("expected non-empty from/to refs")
		}
	}
}

func TestImportQueries(t *testing.T) {
	absPath, err := filepath.Abs("testdata/multipackage")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	importers, err := query.ImportersOf(s, "codemap/testdata/multipackage/pkgB")
	if err != nil {
		t.Fatalf("importers of error: %v", err)
	}
	if len(importers) == 0 {
		t.Fatal("expected at least one importer for pkgB")
	}
	for _, e := range importers {
		if e.EdgeType != "imports" {
			t.Errorf("expected edge type 'imports', got '%s'", e.EdgeType)
		}
	}

	imports, err := query.ImportsOf(s, "pkgA")
	if err != nil {
		t.Fatalf("imports of error: %v", err)
	}
	if len(imports) == 0 {
		t.Fatal("expected at least one import for pkgA")
	}
	for _, e := range imports {
		if e.EdgeType != "imports" {
			t.Errorf("expected edge type 'imports', got '%s'", e.EdgeType)
		}
	}

	none, err := query.ImportersOf(s, "nonexistent")
	if err != nil {
		t.Fatalf("importers of non-existent error: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected empty result for non-existent package, got %d edges", len(none))
	}
}

func TestPackageWithUnexported(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	pkg, err := query.Package(s, "codemap/testdata/simple")
	if err != nil {
		t.Fatalf("package query error: %v", err)
	}

	exportedCount := len(pkg.ExportedSymbols)

	pkgWithUnexported, err := query.Package(s, "codemap/testdata/simple", query.WithUnexported())
	if err != nil {
		t.Fatalf("package query with unexported error: %v", err)
	}

	if len(pkgWithUnexported.ExportedSymbols) <= exportedCount {
		t.Errorf("expected WithUnexported to return more symbols than without (%d <= %d)",
			len(pkgWithUnexported.ExportedSymbols), exportedCount)
	}

	hasUnexported := false
	for _, sym := range pkgWithUnexported.ExportedSymbols {
		if !sym.Exported {
			hasUnexported = true
			break
		}
	}
	if !hasUnexported {
		t.Error("expected WithUnexported to include unexported symbols")
	}
}

func TestOverviewAggregatesAndFullDocs(t *testing.T) {
	s := setupTestStore(t, "testdata/multipackage")

	result, err := query.Overview(s)
	if err != nil {
		t.Fatalf("overview error: %v", err)
	}

	if result.TotalPackages == 0 {
		t.Error("expected non-zero TotalPackages")
	}
	if result.TotalSymbols == 0 {
		t.Error("expected non-zero TotalSymbols")
	}
	if result.TotalEdges == 0 {
		t.Error("expected non-zero TotalEdges")
	}
	if len(result.Packages) != result.TotalPackages {
		t.Errorf("TotalPackages %d doesn't match packages count %d", result.TotalPackages, len(result.Packages))
	}

	jsonFullDocs := render.RenderOverview(result, render.WithFormat(render.FormatJSON), render.WithFullDocs())
	jsonShortDocs := render.RenderOverview(result, render.WithFormat(render.FormatJSON))

	var parsedFull, parsedShort struct {
		Summary struct {
			TotalPackages int `json:"total_packages"`
			TotalSymbols  int `json:"total_symbols"`
			TotalEdges    int `json:"total_edges"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(jsonFullDocs), &parsedFull); err != nil {
		t.Fatalf("failed to parse full docs JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(jsonShortDocs), &parsedShort); err != nil {
		t.Fatalf("failed to parse short docs JSON: %v", err)
	}
	if parsedFull.Summary.TotalPackages == 0 {
		t.Error("expected non-zero total_packages in JSON summary")
	}

	text := render.RenderOverview(result, render.WithFormat(render.FormatText))
	if !strings.Contains(text, "Summary:") {
		t.Error("expected text output to contain aggregate summary")
	}
}

func TestEdgeRenderersIncludePositions(t *testing.T) {
	absPath, err := filepath.Abs("testdata/interfaces")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	edges, err := query.CallersOf(s, "codemap/testdata/interfaces.Buffer", query.WithEdgeTypes("calls"))
	if err != nil {
		t.Fatalf("callers of error: %v", err)
	}

	if len(edges) > 0 {
		jsonOutput := render.RenderCallers(edges, render.WithFormat(render.FormatJSON))
		if !strings.Contains(jsonOutput, "pos_file") {
			t.Error("expected pos_file in JSON output")
		}
		if !strings.Contains(jsonOutput, "pos_line") {
			t.Error("expected pos_line in JSON output")
		}

		textOutput := render.RenderCallers(edges, render.WithFormat(render.FormatText))
		if !strings.Contains(textOutput, ".go:") {
			t.Error("expected position info in text output")
		}
	}
}

func TestNoSyntacticEdges(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	allEdges, err := s.AllEdges()
	if err != nil {
		t.Fatalf("all edges error: %v", err)
	}
	for _, e := range allEdges {
		if e.EdgeType == "calls_syntactic" || e.EdgeType == "references_syntactic" {
			t.Errorf("unexpected edge type '%s' in store", e.EdgeType)
		}
	}
}

func TestSatisfiesCountRemoved(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	overview, err := query.Overview(s)
	if err != nil {
		t.Fatalf("overview error: %v", err)
	}

	data, err := json.Marshal(overview)
	if err != nil {
		t.Fatalf("json marshal error: %v", err)
	}
	if strings.Contains(string(data), "SatisfiesCount") {
		t.Error("output should not contain SatisfiesCount")
	}
}

func TestRenderFormats(t *testing.T) {
	absPath, err := filepath.Abs("testdata/simple")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	resolveResult := resolve.Run(parseResult)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	overview, err := query.Overview(s)
	if err != nil {
		t.Fatalf("overview error: %v", err)
	}

	text := render.RenderOverview(overview, render.WithFormat(render.FormatText))
	if text == "" {
		t.Error("expected non-empty text output")
	}

	json := render.RenderOverview(overview, render.WithFormat(render.FormatJSON))
	if json == "" {
		t.Error("expected non-empty JSON output")
	}

	compact := render.RenderOverview(overview, render.WithFormat(render.FormatCompact))
	if compact == "" {
		t.Error("expected non-empty compact output")
	}

	toon := render.RenderOverview(overview, render.WithFormat(render.FormatTOON))
	if toon == "" {
		t.Error("expected non-empty TOON output")
	}
}
