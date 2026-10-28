package query_test

import (
	"path/filepath"
	"testing"

	"codemap/query"
)

// A call through an interface is recorded against the interface's declaration,
// so every implementer's method looks uncalled. Reporting those as dead code
// would have an agent delete a method that a live call path reaches.
func TestUnusedCreditsInterfaceDispatch(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-unexported")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	unused, err := query.UnusedSymbols(s)
	if err != nil {
		t.Fatalf("UnusedSymbols: %v", err)
	}

	got := make(map[string]bool, len(unused))
	for _, u := range unused {
		got[u.QualifiedName] = true
	}
	// deliver() calls t.getIDs() through the task constraint, so Job.getIDs is
	// reached by a live path even though no symbol names it directly.
	if got["example.com/iface-unexported.Job.getIDs"] {
		t.Errorf("Job.getIDs is called through the task interface but reported dead")
	}
}
