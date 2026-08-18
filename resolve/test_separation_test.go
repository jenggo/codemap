package resolve

import (
	"path/filepath"
	"testing"

	"codemap/parse"
)

// TestExternalTestPackageCleanAndReachable verifies that a correctly indexed
// module with an external test package type-checks with a clean warning stream
// (no "expected package ..._test" cross-clause errors) and that symbols from
// the `_test` package are reachable under the qualified _test import path.
func TestExternalTestPackageCleanAndReachable(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "sep"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	for _, w := range res.Warnings {
		t.Fatalf("unexpected warning for correctly-classified external test: %s", w)
	}

	found := false
	for _, s := range res.Symbols {
		if s.Symbol.QualifiedName == "example.com/sep_test.ServiceConstructor" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected example.com/sep_test.ServiceConstructor symbol to be reachable")
	}
}
