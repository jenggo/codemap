package query_test

import (
	"encoding/json"
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
