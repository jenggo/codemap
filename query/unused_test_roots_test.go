package query

import "testing"

// TestUnusedSymbolsStableAcrossIncludeTests pins that the report does not
// depend on whether test symbols are listed: test functions are liveness roots
// either way, so toggling includeTests only adds test symbols to the result,
// it never reshuffles which production symbols are judged reachable.
//
// A regression here means test-only helpers started being reported as dead.
func TestUnusedSymbolsStableAcrossIncludeTests(t *testing.T) {
	s := setupQueryStoreAt(t, "..")

	without, err := UnusedSymbols(s)
	if err != nil {
		t.Fatalf("UnusedSymbols: %v", err)
	}
	with, err := UnusedSymbols(s, WithTests())
	if err != nil {
		t.Fatalf("UnusedSymbols(includeTests): %v", err)
	}

	inWith := make(map[string]bool, len(with))
	for _, u := range with {
		inWith[u.QualifiedName] = true
	}

	// Every symbol reported without tests must still be reported with them:
	// adding roots cannot make a production symbol reachable.
	for _, u := range without {
		if !inWith[u.QualifiedName] {
			t.Errorf("%s reported with includeTests=false but not =true", u.QualifiedName)
		}
	}
}
