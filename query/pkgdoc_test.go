package query_test

import (
	"path/filepath"
	"testing"

	"codemap/query"
)

// TestPackageDoc pins that Package copies the store's package doc through: the
// first sentence of the clause comment when one exists, an empty string when it
// does not — never a symbol's doc.
func TestPackageDoc(t *testing.T) {
	abs, err := filepath.Abs("../testdata/fixtures/pkgdoc")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	s := setupFixtureStore(t, abs)

	documented, err := query.Package(s, "example.com/pkgdoc/documented")
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	want := "Package documented carries a clause comment so the query layer can surface its first sentence."
	if documented.Doc != want {
		t.Errorf("doc = %q, want %q", documented.Doc, want)
	}

	plain, err := query.Package(s, "example.com/pkgdoc/plain")
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if plain.Doc != "" {
		t.Errorf("doc = %q, want empty (must not borrow a symbol doc)", plain.Doc)
	}
}
