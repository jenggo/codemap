package render

import (
	"strings"
	"testing"

	"codemap/query"
)

// TestRenderEntryPointsDoesNotDuplicateRows pins that each entry appears once.
// The renderer used to emit the flat list and a by_reason grouping over the same
// rows, doubling the payload and making any count taken from it wrong by 2x.
func TestRenderEntryPointsDoesNotDuplicateRows(t *testing.T) {
	entries := []query.EntryPoint{
		{QualifiedName: "m.main", Kind: "function", Reason: "main", PosFile: "main.go", PosLine: 10},
		{QualifiedName: "p.Helper", Kind: "function", Reason: "uncalled_exported", PosFile: "p.go", PosLine: 5},
		{QualifiedName: "p.Other", Kind: "function", Reason: "uncalled_exported", PosFile: "p.go", PosLine: 20},
	}
	out := RenderEntryPoints(entries)
	for _, e := range entries {
		if got := strings.Count(out, e.QualifiedName); got != 1 {
			t.Errorf("%s appears %d times, want 1:\n%s", e.QualifiedName, got, out)
		}
	}
}
