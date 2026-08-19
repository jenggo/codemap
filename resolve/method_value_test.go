package resolve

import (
	"path/filepath"
	"strconv"
	"testing"

	"codemap/parse"
)

// TestMethodExpressionCallEdge verifies that a method expression used as a
// call (T.M(x)) produces a `calls` edge to T.M.
func TestMethodExpressionCallEdge(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "method-value"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	key := func(to, ty string, line int) string {
		return to + "|" + ty + "|" + strconv.Itoa(line)
	}
	keys := map[string]bool{}
	for _, e := range res.Edges {
		keys[key(e.Edge.ToRef, e.Edge.EdgeType, e.Edge.Pos.Line)] = true
	}

	// Method expression call T.M(t, 1) at line 12 → calls edge.
	if !keys[key("example.com/mv.T.M", "calls", 12)] {
		t.Error("method expression call (T.M(t,1)) should emit a calls edge")
	}
}

// TestMethodValueAsCallback verifies that a method value passed as a
// callback produces a `references` edge to the method.
func TestMethodValueAsCallback(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "method-value"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	key := func(to, ty string, line int) string {
		return to + "|" + ty + "|" + strconv.Itoa(line)
	}
	keys := map[string]bool{}
	for _, e := range res.Edges {
		keys[key(e.Edge.ToRef, e.Edge.EdgeType, e.Edge.Pos.Line)] = true
	}

	// Method value f := t.M (line 11) → references.
	if !keys[key("example.com/mv.T.M", "references", 11)] {
		t.Error("method value (f := t.M) should emit a references edge")
	}

	// Method value as callback do(t.M) (line 13) → references.
	if !keys[key("example.com/mv.T.M", "references", 13)] {
		t.Error("method value as callback (do(t.M)) should emit a references edge")
	}
}
