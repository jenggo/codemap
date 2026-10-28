package query_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codemap/extract"
	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

// buildGraphStore parses a synthetic module (or workspace of modules), replaces
// its resolve-level edges with the given list for precise graph control, and
// writes everything to a fresh store. When repos is non-nil the workspace write
// path is used so symbols/edges carry repo attribution.
func buildGraphStore(t *testing.T, modules map[string]map[string]string, edges []extract.Edge, repos []store.RepoSpec) *store.Store {
	t.Helper()

	var combined *resolve.Result
	allFiles := map[string]string{}

	for module, files := range modules {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+module+"\n\ngo 1.21\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		for name, content := range files {
			p := filepath.Join(root, name)
			dir := filepath.Dir(p)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		parseRes, err := parse.Run(root)
		if err != nil {
			t.Fatalf("parse %s: %v", module, err)
		}
		res := resolve.Run(parseRes)
		if combined == nil {
			combined = res
		} else {
			combined.Packages = append(combined.Packages, res.Packages...)
			combined.Symbols = append(combined.Symbols, res.Symbols...)
			combined.Warnings = append(combined.Warnings, res.Warnings...)
		}
		maps.Copy(allFiles, parse.FileContents(parseRes))
	}

	resolvedEdges := make([]resolve.ResolvedEdge, len(edges))
	for i, e := range edges {
		resolvedEdges[i] = resolve.ResolvedEdge{Edge: e}
	}
	combined.Edges = resolvedEdges

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if repos == nil {
		if err := s.Write(combined, allFiles, nil); err != nil {
			t.Fatalf("write: %v", err)
		}
	} else {
		if err := s.WriteWorkspace(combined, allFiles, nil, repos); err != nil {
			t.Fatalf("write workspace: %v", err)
		}
	}
	return s
}

func symEdge(from, to, edgeType string) extract.Edge {
	return extract.Edge{FromRef: from, ToRef: to, EdgeType: edgeType, Pos: extract.Position{File: "/fixture/a.go", Line: 1}}
}

// TestSymbolImportanceHubOutranksLeaf: pkg.A called by 50 symbols outranks
// pkg.Z called by 1 symbol (spec: hub symbol outranks leaf symbol).
func TestSymbolImportanceHubOutranksLeaf(t *testing.T) {
	graph := `package graph

func A() {}

func Z() {}

`
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "func Call%d() { A() }\n", i)
	}
	sb.WriteString("func CallZ() { Z() }\n")
	graph += sb.String()

	var edges []extract.Edge
	for i := 1; i <= 50; i++ {
		edges = append(edges, symEdge(fmt.Sprintf("example.com/graph.Call%d", i), "example.com/graph.A", "calls"))
	}
	edges = append(edges, symEdge("example.com/graph.CallZ", "example.com/graph.Z", "calls"))

	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/graph": {"a.go": graph},
	}, edges, nil)

	entries, err := query.SymbolImportance(s, 0, 0)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}

	rankByName := map[string]float64{}
	for _, e := range entries {
		rankByName[e.QualifiedName] = e.Importance
	}
	rankA := rankByName["example.com/graph.A"]
	rankZ := rankByName["example.com/graph.Z"]
	if rankA <= rankZ {
		t.Errorf("hub symbol A (%f) should outrank leaf Z (%f)", rankA, rankZ)
	}
}

// TestSymbolImportanceIsolatedBelowHub: isolated symbols get a non-zero but
// strictly lower rank than a symbol with incoming edges (spec scenarios 1-2).
func TestSymbolImportanceIsolatedBelowHub(t *testing.T) {
	graph := `package graph

func Hub() {}

func Isolated() {}

func Caller() { Hub() }

`
	edges := []extract.Edge{
		symEdge("example.com/graph.Caller", "example.com/graph.Hub", "calls"),
	}

	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/graph": {"a.go": graph},
	}, edges, nil)

	entries, err := query.SymbolImportance(s, 10000, 0)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}

	rankByName := map[string]float64{}
	for _, e := range entries {
		rankByName[e.QualifiedName] = e.Importance
	}
	hub := rankByName["example.com/graph.Hub"]
	iso := rankByName["example.com/graph.Isolated"]
	if hub <= 0 || iso <= 0 {
		t.Fatalf("all ranks should be non-zero (teleport not dropped): hub=%f iso=%f", hub, iso)
	}
	if iso >= hub {
		t.Errorf("isolated symbol (%f) should rank strictly below hub (%f)", iso, hub)
	}
}

// TestSymbolImportanceImportsDoNotCount: package imports edges (package-path
// endpoints) contribute nothing; a symbol with one real caller outranks a
// symbol that is only imported by packages.
func TestSymbolImportanceImportsDoNotCount(t *testing.T) {
	graph := `package graph

func A() {}

func W() {}

func Caller() { W() }

`
	edges := []extract.Edge{
		symEdge("example.com/graph/b1", "example.com/graph", "imports"),
		symEdge("example.com/graph/b2", "example.com/graph", "imports"),
		symEdge("example.com/graph/b3", "example.com/graph", "imports"),
		symEdge("example.com/graph.Caller", "example.com/graph.W", "calls"),
	}

	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/graph": {"a.go": graph},
	}, edges, nil)

	entries, err := query.SymbolImportance(s, 10000, 0)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}

	rankByName := map[string]float64{}
	for _, e := range entries {
		rankByName[e.QualifiedName] = e.Importance
	}
	if rankByName["example.com/graph.W"] <= rankByName["example.com/graph.A"] {
		t.Errorf("W with one real caller (%f) should outrank A with only imports (%f)",
			rankByName["example.com/graph.W"], rankByName["example.com/graph.A"])
	}
}

// TestSymbolImportanceScopeNoOp: scope is reserved and must not change results.
func TestSymbolImportanceScopeNoOp(t *testing.T) {
	graph := `package graph

func A() {}

func B() {}

func Caller() { A() }

`
	edges := []extract.Edge{
		symEdge("example.com/graph.Caller", "example.com/graph.A", "calls"),
	}
	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/graph": {"a.go": graph},
	}, edges, nil)

	zero, err := query.SymbolImportance(s, 100, 0)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}
	other, err := query.SymbolImportance(s, 100, 7)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}
	if !reflect.DeepEqual(zero, other) {
		t.Errorf("scope=0 and scope=7 returned different rankings:\n%+v\n%+v", zero, other)
	}
}

// TestSymbolImportanceEmptyIndex: empty index returns a non-nil empty slice that
// marshals to [].
func TestSymbolImportanceEmptyIndex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	entries, err := query.SymbolImportance(s, 0, 0)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}
	if entries == nil {
		t.Fatal("expected non-nil empty entries")
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
	if data, err := json.Marshal(entries); err != nil || string(data) != "[]" {
		t.Errorf("empty importance should marshal to [], got %s (err %v)", data, err)
	}
}

// TestSymbolImportanceRepoScoped: WithRepo filters nodes and edges to one
// workspace member.
func TestSymbolImportanceRepoScoped(t *testing.T) {
	alpha := `package graph

func A() {}

func Caller() { A() }

`
	beta := `package graph

func B() {}

func B1() { B() }
func B2() { B() }
func B3() { B() }

`
	edges := []extract.Edge{
		symEdge("example.com/alpha.Caller", "example.com/alpha.A", "calls"),
		symEdge("example.com/beta.B1", "example.com/beta.B", "calls"),
		symEdge("example.com/beta.B2", "example.com/beta.B", "calls"),
		symEdge("example.com/beta.B3", "example.com/beta.B", "calls"),
	}
	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/alpha": {"a.go": alpha},
		"example.com/beta":  {"a.go": beta},
	}, edges, []store.RepoSpec{
		{ModulePath: "example.com/alpha", Dir: "/alpha"},
		{ModulePath: "example.com/beta", Dir: "/beta"},
	})

	scoped, err := query.SymbolImportance(s, 10000, 0, query.WithRepo("example.com/alpha"))
	if err != nil {
		t.Fatalf("SymbolImportance scoped: %v", err)
	}
	if len(scoped) == 0 {
		t.Fatal("expected scoped entries")
	}
	for _, e := range scoped {
		if e.QualifiedName != "example.com/alpha.A" && e.QualifiedName != "example.com/alpha.Caller" {
			t.Errorf("repo scope leaked symbol %q", e.QualifiedName)
		}
	}

	// B has 3 callers, A has 1: unscoped ranking puts B above A.
	unscoped, err := query.SymbolImportance(s, 10000, 0)
	if err != nil {
		t.Fatalf("SymbolImportance unscoped: %v", err)
	}
	rankByName := map[string]float64{}
	for _, e := range unscoped {
		rankByName[e.QualifiedName] = e.Importance
	}
	if rankByName["example.com/beta.B"] <= rankByName["example.com/alpha.A"] {
		t.Errorf("unscoped B (%f) should outrank A (%f)",
			rankByName["example.com/beta.B"], rankByName["example.com/alpha.A"])
	}
}

// TestFindPathFollowsForwardEdgesOnly pins that find_path answers reachability,
// not connectivity. Following in-edges made it return a chain whose hop only
// existed as "B references A", which reads as a call path that does not exist.
func TestFindPathFollowsForwardEdgesOnly(t *testing.T) {
	mod := map[string]string{
		"p.go": "package p\n\nfunc A() { B() }\n\nfunc B() {}\n\nfunc C() { D() }\n\nfunc D() {}\n",
	}
	edges := []extract.Edge{
		symEdge("example.com/p.A", "example.com/p.B", "calls"),
		symEdge("example.com/p.C", "example.com/p.D", "calls"),
	}
	s := buildGraphStore(t, map[string]map[string]string{"example.com/p": mod}, edges, nil)

	path, err := query.FindPath(s, "example.com/p.A", "example.com/p.B", 5)
	if err != nil {
		t.Fatalf("forward path: %v", err)
	}
	if len(path) != 1 || path[0].From != "example.com/p.A" || path[0].To != "example.com/p.B" {
		t.Fatalf("forward path = %+v", path)
	}

	// B cannot reach A: the only edge runs A -> B. A bidirectional walk used to
	// return it anyway, reversed.
	if _, err := query.FindPath(s, "example.com/p.B", "example.com/p.A", 5); !errors.Is(err, query.ErrNoPath) {
		t.Fatalf("reverse lookup err = %v, want query.ErrNoPath", err)
	}

	// A is not connected to the unrelated C/D pair at all.
	if _, err := query.FindPath(s, "example.com/p.A", "example.com/p.D", 5); !errors.Is(err, query.ErrNoPath) {
		t.Fatalf("disconnected lookup err = %v, want query.ErrNoPath", err)
	}
}

// TestFindPathRejectsUnknownSymbol pins that a mistyped endpoint errors instead
// of reporting "no path", which reads as "these symbols are unrelated".
func TestFindPathRejectsUnknownSymbol(t *testing.T) {
	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/p": {"p.go": "package p\n\nfunc A() {}\n\nfunc B() {}\n"},
	}, nil, nil)

	if _, err := query.FindPath(s, "example.com/p.Nope", "example.com/p.B", 5); err == nil {
		t.Fatal("unknown from = nil error, want symbol-not-found")
	}
	if _, err := query.FindPath(s, "example.com/p.A", "example.com/p.Nope", 5); err == nil {
		t.Fatal("unknown to = nil error, want symbol-not-found")
	}
}

// TestTypeUsageAcceptsQualifiedName pins that the qualified form works as well
// as the short one. Matching runs against signatures, which drop the package
// qualifier, so "example.com/p.Widget" used to find nothing.
func TestTypeUsageAcceptsQualifiedName(t *testing.T) {
	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/p": {"p.go": "package p\n\ntype Widget struct{}\n\nfunc Use(w Widget) {}\n"},
	}, nil, nil)

	short, err := query.TypeUsage(s, "Widget")
	if err != nil {
		t.Fatalf("TypeUsage(Widget): %v", err)
	}
	qualified, err := query.TypeUsage(s, "example.com/p.Widget")
	if err != nil {
		t.Fatalf("TypeUsage(example.com/p.Widget): %v", err)
	}
	if len(short) == 0 {
		t.Fatal("short name found no usages")
	}
	if len(qualified) != len(short) {
		t.Fatalf("qualified form returned %d usages, short form %d", len(qualified), len(short))
	}
}

// TestTypeUsageMatchesExactType pins that a short name matches only the type it
// names. A substring scan of signatures reported "*gitlab.Response" and
// "fantasy.ToolResponse" for Response, and matched the field "FirstRun" for Run.
func TestTypeUsageMatchesExactType(t *testing.T) {
	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/p": {"p.go": "package p\n\n" +
			"type Response struct{}\n" +
			"type ToolResponse struct{}\n" +
			"type Holder struct { FirstRun bool }\n" +
			"func TakeResponse(r Response) {}\n" +
			"func TakeOther(r ToolResponse) {}\n"},
		"example.com/q": {"q.go": "package q\n\n" +
			"type Response struct{}\n" +
			"func TakeForeign(r Response) {}\n"},
	}, nil, nil)

	got, err := query.TypeUsage(s, "example.com/p.Response")
	if err != nil {
		t.Fatalf("TypeUsage: %v", err)
	}
	names := map[string]bool{}
	for _, r := range got {
		names[r.QualifiedName] = true
	}
	if !names["example.com/p.TakeResponse"] {
		t.Errorf("expected the real user example.com/p.TakeResponse, got %v", names)
	}
	if names["example.com/p.TakeOther"] {
		t.Errorf("ToolResponse must not count as a use of Response: %v", names)
	}
	if names["example.com/q.TakeForeign"] {
		t.Errorf("another package's Response must not count: %v", names)
	}

	if _, err := query.TypeUsage(s, "Run"); err == nil {
		t.Error("a name that declares no type must error, not match field names")
	}
}

// TestTypeUsageRejectsUnknownPackage pins that a stale or typo'd package
// qualifier errors instead of silently answering about a same-named type.
func TestTypeUsageRejectsUnknownPackage(t *testing.T) {
	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/p": {"p.go": "package p\n\ntype Widget struct{}\n\nfunc Use(w Widget) {}\n"},
	}, nil, nil)

	if _, err := query.TypeUsage(s, "example.com/nonexistent.Widget"); err == nil {
		t.Fatal("expected an error for a package that does not exist")
	}
}

// TestTypeNameFormsAgreeAcrossTools pins that interface_impls and methods_of
// accept the same two name forms. Each used to work with exactly one and return
// an empty list for the other, which reads as "no implementers".
func TestTypeNameFormsAgreeAcrossTools(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-impls")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	var wantImpls, wantMethods int
	for i, name := range []string{"Runner", "example.com/iface-impls/a.Runner"} {
		impls, err := query.InterfaceImplementations(s, name)
		if err != nil {
			t.Fatalf("interface_impls(%q): %v", name, err)
		}
		methods, err := query.MethodsOf(s, name)
		if err != nil {
			t.Fatalf("methods_of(%q): %v", name, err)
		}
		if i == 0 {
			wantImpls, wantMethods = len(impls), len(methods)
			if wantImpls == 0 || wantMethods == 0 {
				t.Fatalf("fixture yielded no results: %d impls, %d methods", wantImpls, wantMethods)
			}
			continue
		}
		if len(impls) != wantImpls {
			t.Errorf("interface_impls(%q) = %d rows, short form gave %d", name, len(impls), wantImpls)
		}
		if len(methods) != wantMethods {
			t.Errorf("methods_of(%q) = %d methods, short form gave %d", name, len(methods), wantMethods)
		}
	}

	// A type with no methods is not the same answer as a type that does not
	// exist: the first returns an empty list, the second errors.
	if got, err := query.MethodsOf(s, "example.com/iface-impls/a.Service"); err != nil || len(got) == 0 {
		t.Errorf("methods_of(a.Service) = %d, %v; want its 3 methods", len(got), err)
	}
	if _, err := query.MethodsOf(s, "NoSuchType"); err == nil {
		t.Error("methods_of(NoSuchType) should error")
	}
	if _, err := query.InterfaceImplementations(s, "NoSuchType"); err == nil {
		t.Error("interface_impls(NoSuchType) should error")
	}
}

// TestMethodsOfScopesToTheResolvedPackage pins that a qualified name returns
// only that package's methods, not those of a same-named type elsewhere.
func TestMethodsOfScopesToTheResolvedPackage(t *testing.T) {
	s := buildGraphStore(t, map[string]map[string]string{
		"example.com/p": {"p.go": "package p\n\ntype Dup struct{}\n\nfunc (d *Dup) FromP() {}\n"},
		"example.com/q": {"q.go": "package q\n\ntype Dup struct{}\n\nfunc (d *Dup) FromQ() {}\n"},
	}, nil, nil)

	methods, err := query.MethodsOf(s, "example.com/p.Dup")
	if err != nil {
		t.Fatalf("methods_of: %v", err)
	}
	if len(methods) != 1 || methods[0].QualifiedName != "example.com/p.Dup.FromP" {
		t.Fatalf("expected only example.com/p.Dup.FromP, got %+v", methods)
	}

	// An unqualified ambiguous name must say so instead of guessing.
	if _, err := query.MethodsOf(s, "Dup"); err == nil {
		t.Error("methods_of(Dup) should error when two packages declare Dup")
	}
}

// TestImportsOfExcludesTestSites pins that imports_of does not list a
// test-only import and does not attribute a shared import to its test site.
func TestImportsOfExcludesTestSites(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/imp\n\ngo 1.21\n")
	writeFile("p/p.go", "package p\n\nimport \"fmt\"\n\nfunc Use() { fmt.Println() }\n")
	writeFile("p/p_test.go", "package p\n\nimport (\n\t\"testing\"\n\t\"github.com/only/test\"\n)\n\nfunc TestUse(t *testing.T) { _ = testing.Short() }\n")

	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	edges, err := query.ImportsOf(s, "example.com/imp/p")
	if err != nil {
		t.Fatalf("ImportsOf: %v", err)
	}
	if len(edges) != 1 || edges[0].ToRef != "fmt" {
		names := make([]string, len(edges))
		for i, e := range edges {
			names[i] = e.ToRef
		}
		t.Fatalf("imports_of = %v, want just fmt", names)
	}
	for _, site := range edges[0].Sites {
		if strings.HasSuffix(site.File, "_test.go") {
			t.Errorf("test site %s should have been dropped", site.File)
		}
	}
}

// TestImportersOfExcludesTestOnlyImporters pins the inverse relation: a package
// imported only from a _test.go file must not be reported as an importer, since
// ImportsOf already omits that same import from the importing package's own
// edge list. The asymmetry made "who depends on this" name a package that does
// not, in any built binary.
func TestImportersOfExcludesTestOnlyImporters(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/rev\n\ngo 1.21\n")
	writeFile("target/target.go", "package target\n\nfunc T() int { return 1 }\n")
	// Runtime importer.
	writeFile("real/real.go", "package real\n\nimport \"example.com/rev/target\"\n\nfunc R() int { return target.T() }\n")
	// Test-only importer.
	writeFile("tester/tester.go", "package tester\n\nfunc Nothing() {}\n")
	writeFile("tester/tester_test.go", "package tester\n\nimport (\n\t\"testing\"\n\t\"example.com/rev/target\"\n)\n\nfunc TestX(t *testing.T) { _ = target.T() }\n")

	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	edges, err := query.ImportersOf(s, "example.com/rev/target")
	if err != nil {
		t.Fatalf("ImportersOf: %v", err)
	}
	got := make([]string, 0, len(edges))
	for _, e := range edges {
		got = append(got, e.FromRef)
	}
	if len(got) != 1 || got[0] != "example.com/rev/real" {
		t.Fatalf("importers_of = %v, want only example.com/rev/real", got)
	}

	// The relation is now symmetric: the runtime importer lists the import and
	// the test-only importer does not.
	out, err := query.ImportsOf(s, "example.com/rev/tester")
	if err != nil {
		t.Fatalf("ImportsOf: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("imports_of tester = %d edges, want 0 (target is test-only)", len(out))
	}
}

// TestSearchFiltersApplyInEveryMode pins that kind and exported narrow prefix
// and method results as well as substring results. They used to be read only in
// the substring branch, so a prefix sweep returned unexported rows and other
// kinds with no hint that the filter had been dropped.
func TestSearchFiltersApplyInEveryMode(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/filt\n\ngo 1.21\n")
	writeFile("p/p.go", `package p

type Usage = int

type Exported struct{}

type hidden struct{}

func ExportedFn() {}

func hiddenFn() {}

func (Exported) Do() {}

func (hidden) Do() {}
`)
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	check := func(label string, results []query.SearchResult, wantExported bool) {
		t.Helper()
		for _, r := range results {
			if r.Exported != wantExported {
				t.Errorf("%s: %s exported=%t, want %t", label, r.QualifiedName, r.Exported, wantExported)
			}
		}
	}

	prefix, err := query.SearchByPrefix(s, "example.com/filt/p.", query.WithExported(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix) == 0 {
		t.Fatal("prefix search returned nothing")
	}
	check("prefix", prefix, true)

	methods, err := query.MethodSearch(s, "Do", query.WithExported(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) == 0 {
		t.Fatal("method search returned nothing")
	}
	check("method", methods, true)

	// kind:"type" also covers interfaces and aliases: both name a type.
	types, err := query.Search(s, "Usage", query.WithKind("type"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range types {
		if r.Kind == "alias" {
			found = true
		}
		if r.Kind != "type" && r.Kind != "interface" && r.Kind != "alias" {
			t.Errorf("kind filter let through %s of kind %s", r.QualifiedName, r.Kind)
		}
	}
	if !found {
		t.Errorf("kind=type dropped the alias Usage; got %d rows", len(types))
	}
}

// TestCalleesMarksExternalRefs pins the resolvable/external distinction. Every
// graph read used to end in a follow-up show that errored for stdlib targets,
// so a reader had to know Go's stdlib by heart to interpret a to_ref.
func TestCalleesMarksExternalRefs(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/ext\n\ngo 1.21\n")
	writeFile("p/p.go", `package p

import "strings"

func Local() string { return "x" }

func Caller(v string) string {
	_ = Local()
	return strings.TrimSpace(v)
}
`)
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	edges, err := query.CalleesOf(s, "example.com/ext/p.Caller")
	if err != nil {
		t.Fatalf("CalleesOf: %v", err)
	}
	got := map[string]bool{}
	for _, e := range edges {
		got[e.ToRef] = e.External
	}
	if ext, ok := got["example.com/ext/p.Local"]; !ok || ext {
		t.Errorf("Local = external %v (present %v), want in-project", ext, ok)
	}
	if ext, ok := got["strings.TrimSpace"]; !ok || !ext {
		t.Errorf("strings.TrimSpace = external %v (present %v), want external", ext, ok)
	}
}

// TestCalleesDropsPredeclaredRefs pins that a call through a predeclared type's
// method is not emitted as a ref. `err.Error()` produced to_ref ".error.Error",
// which reads as a package named "." and resolves to nothing.
func TestCalleesDropsPredeclaredRefs(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/pre\n\ngo 1.21\n")
	// A call through a variable of interface type: the receiver's method has no
	// package of its own, which is the shape that produced ".error.Error".
	writeFile("p/p.go", `package p

import "os"

func Caller() string {
	f, err := os.Open("x")
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	return ""
}
`)
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	edges, err := query.CalleesOf(s, "example.com/pre/p.Caller")
	if err != nil {
		t.Fatalf("CalleesOf: %v", err)
	}
	for _, e := range edges {
		if strings.HasPrefix(e.ToRef, ".") {
			t.Errorf("emitted a ref with no package prefix: %q", e.ToRef)
		}
	}
}
