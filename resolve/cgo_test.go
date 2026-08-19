package resolve

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"codemap/parse"
)

func TestCgoReferenceEdges(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "cgo"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	t.Logf("Total edges: %d", len(res.Edges))
	for _, e := range res.Edges {
		t.Logf("  edge: %s -> %s [%s] at %s:%d",
			e.Edge.FromRef, e.Edge.ToRef, e.Edge.EdgeType,
			e.Edge.Pos.File, e.Edge.Pos.Line)
	}

	key := func(to, ty string, line int) string {
		return to + "|" + ty + "|" + strconv.Itoa(line)
	}
	keys := map[string]bool{}
	for _, e := range res.Edges {
		keys[key(e.Edge.ToRef, e.Edge.EdgeType, e.Edge.Pos.Line)] = true
	}

	// C.malloc at line 9 → references edge to cgo/malloc.
	if !keys[key("cgo/malloc", "references", 9)] {
		t.Error("C.malloc should emit a references edge to cgo/malloc")
	}

	// C.free at line 13 → references edge to cgo/free.
	if !keys[key("cgo/free", "references", 13)] {
		t.Error("C.free should emit a references edge to cgo/free")
	}
}

func TestCgoWarning(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "cgo"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "cgo") && strings.Contains(w, "pseudo-symbols") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected cgo warning, got warnings: %v", res.Warnings)
	}
}

func TestCgoSymbolRows(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "cgo"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	cgoSymbols := map[string]bool{}
	for _, s := range res.Symbols {
		if strings.HasPrefix(s.Symbol.QualifiedName, "cgo/") {
			cgoSymbols[s.Symbol.QualifiedName] = true
		}
	}

	if !cgoSymbols["cgo/malloc"] {
		t.Error("expected cgo/malloc symbol row")
	}
	if !cgoSymbols["cgo/free"] {
		t.Error("expected cgo/free symbol row")
	}
}
