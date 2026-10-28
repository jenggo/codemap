package resolve

import (
	"path/filepath"
	"testing"

	"codemap/parse"
)

// A call through a constraint interface must point at the indexed interface
// method. When unexported interface methods were not extracted, the edge landed
// on a target that did not exist in the symbol table.
func TestCallThroughConstraintInterfaceResolves(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-unexported")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(absPath)
	if err != nil {
		t.Fatal(err)
	}
	result := Run(pr)

	const want = "example.com/iface-unexported.task.getIDs"

	declared := false
	for _, sym := range result.Symbols {
		if sym.Symbol.QualifiedName == want {
			declared = true
			break
		}
	}
	if !declared {
		t.Fatalf("interface method %s is not a symbol, so call edges into it cannot resolve", want)
	}

	found := false
	for _, e := range result.Edges {
		if e.Edge.EdgeType == "calls" && e.Edge.ToRef == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no calls edge targets %s", want)
	}
}

// A union-only constraint references each named type in the union; that is how
// the type set is discoverable without a dedicated edge type.
func TestUnionTermsEmitReferences(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-unexported")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(absPath)
	if err != nil {
		t.Fatal(err)
	}
	result := Run(pr)

	const from = "example.com/iface-unexported.Payload"
	got := make(map[string]bool)
	for _, e := range result.Edges {
		if e.Edge.FromRef == from && e.Edge.EdgeType == "references" {
			got[e.Edge.ToRef] = true
		}
	}
	for _, want := range []string{
		"example.com/iface-unexported.Job",
		"example.com/iface-unexported.Cache",
	} {
		if !got[want] {
			t.Errorf("union term %s produced no references edge from %s (got %v)", want, from, got)
		}
	}
}
