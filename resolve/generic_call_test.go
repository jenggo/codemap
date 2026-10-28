package resolve

import (
	"path/filepath"
	"testing"

	"codemap/parse"
)

// A generic call writes its callee as f[T](...) or f[A, B](...), so the call
// expression's Fun is an IndexExpr/IndexListExpr rather than an Ident. Without
// unwrapping it, no calls edge was emitted at all and the callee looked dead.
func TestGenericCallEmitsCallsEdge(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/generic-call")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(absPath)
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]bool)
	for _, e := range Run(pr).Edges {
		if e.Edge.EdgeType == "calls" {
			got[e.Edge.ToRef] = true
		}
	}

	for _, want := range []string{
		"example.com/generic-call.Identity", // f[T](...)  — IndexExpr
		"example.com/generic-call.Zip",      // f[A, B](...) — IndexListExpr
	} {
		if !got[want] {
			t.Errorf("no calls edge to %s; generic call sites were not resolved (edges: %v)", want, got)
		}
	}
}
