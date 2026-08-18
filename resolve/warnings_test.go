package resolve

import (
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
)

// TestDependencyTypeErrorAttributedAndDeduped verifies that a dependency that
// fails type-checking surfaces its concrete error in the warning stream with
// package attribution (not collapsed to a generic "could not type-check"), and
// that the same warning never appears twice.
func TestDependencyTypeErrorAttributedAndDeduped(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "broken-dep"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "example.com/brokendep/broken") && strings.Contains(w, "cannot use") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected the concrete dependency error attributed to the package, warnings: %v", res.Warnings)
	}

	seen := make(map[string]bool)
	for _, w := range res.Warnings {
		if seen[w] {
			t.Fatalf("duplicate warning: %q", w)
		}
		seen[w] = true
	}
}
