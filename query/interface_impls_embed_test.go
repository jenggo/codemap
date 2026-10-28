package query_test

import (
	"path/filepath"
	"testing"

	"codemap/query"
)

// A composed interface reports every method its implementers must supply,
// including the ones declared by embedded interfaces.
func TestInterfaceImplementationsIncludesEmbeddedMethods(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-embed")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	impls, err := query.InterfaceImplementations(s, "example.com/iface-embed.ReadWriter")
	if err != nil {
		t.Fatalf("InterfaceImplementations: %v", err)
	}

	got := make(map[string]bool, len(impls))
	for _, impl := range impls {
		got[impl.ImplMethod] = true
	}
	for _, want := range []string{
		"example.com/iface-embed.Console.Log",
		"example.com/iface-embed.Console.Close",
		"example.com/iface-embed.Console.Read",
		"example.com/iface-embed.Console.Write",
	} {
		if !got[want] {
			t.Errorf("missing implementing method %q (got %v)", want, got)
		}
	}
}
