package query

import (
	"strings"
	"testing"
)

// TestCallersOfRejectsUnknownSymbol pins that an unknown start node is an
// error, not an empty list. A typo or a receiver-prefixed free function used
// to traverse from a non-existent node and return [], which reads as
// "no callers".
func TestCallersOfRejectsUnknownSymbol(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/entrypoints")

	for _, name := range []string{
		"codemap/testdata/entrypoints/util.Nope.X",
		"codemap/testdata/entrypoints/nosuchpkg.Thing",
	} {
		_, err := CallersOf(s, name)
		if err == nil {
			t.Errorf("CallersOf(%q) = nil error, want symbol-not-found", name)
			continue
		}
		if !strings.Contains(err.Error(), "symbol not found") {
			t.Errorf("CallersOf(%q) error = %v, want it to mention symbol not found", name, err)
		}
	}

	// A symbol that exists but has no callers is still a valid empty result.
	edges, err := CallersOf(s, "codemap/testdata/entrypoints/util.Helper")
	if err != nil {
		t.Fatalf("CallersOf(Helper): %v", err)
	}
	if len(edges) == 0 {
		t.Log("Helper has no callers, which is a legitimate empty result")
	}
}

func TestCalleesOfRejectsUnknownSymbol(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/entrypoints")

	if _, err := CalleesOf(s, "codemap/testdata/entrypoints/util.Nope"); err == nil {
		t.Error("CalleesOf on an unknown symbol = nil error, want symbol-not-found")
	}
}
