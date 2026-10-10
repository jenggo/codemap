package query

import "testing"

// TestUnusedSymbolsExcludesRuntimeEntryPoints pins the precision boundary: the
// Go runtime invokes main and init itself, so they have no incoming edge and
// must not be reported as dead. A func main() in a non-main package is real
// dead code and must still be reported.
func TestUnusedSymbolsExcludesRuntimeEntryPoints(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/entrypoints")

	unused, err := UnusedSymbols(s)
	if err != nil {
		t.Fatalf("UnusedSymbols: %v", err)
	}

	got := make(map[string]bool, len(unused))
	for _, u := range unused {
		got[u.QualifiedName] = true
		t.Logf("unused: %s", u.QualifiedName)
	}

	// Hidden: runtime-invoked or blank, never a deletable symbol.
	for _, hidden := range []string{
		"codemap/testdata/entrypoints/cmd.main",
		"codemap/testdata/entrypoints/util.init",
	} {
		if got[hidden] {
			t.Errorf("%s must not be reported as unused (the runtime calls it)", hidden)
		}
	}

	// Reported: genuinely unreachable code in a non-main package.
	for _, want := range []string{
		"codemap/testdata/entrypoints/util.main",
		"codemap/testdata/entrypoints/util.internal",
		"codemap/testdata/entrypoints/util.deadHandler",
	} {
		if !got[want] {
			t.Errorf("%s must be reported as unused", want)
		}
	}
}
