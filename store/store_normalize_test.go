package store

import (
	"path/filepath"
	"testing"
)

// TestNormalizeQualifiedName covers the helper's handling of Go-style receiver
// wrappers and idempotency on already-normalized names.
func TestNormalizeQualifiedName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"example.com/pkg.Type.Method", "example.com/pkg.Type.Method"},
		{"example.com/pkg.(*Type).Method", "example.com/pkg.Type.Method"},
		{"example.com/pkg.(Type).Method", "example.com/pkg.Type.Method"},
		{"example.com/pkg.(*Type[T]).Method", "example.com/pkg.Type[T].Method"},
		{"example.com/pkg.Other", "example.com/pkg.Other"},
		{"plain", "plain"},
	}
	for _, c := range cases {
		if got := NormalizeQualifiedName(c.in); got != c.want {
			t.Errorf("NormalizeQualifiedName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReceiverParenLookup verifies that symbol and edge lookups tolerate the
// Go-style receiver-paren qualified name form ("pkg.(*Type).Method"), which
// otherwise fails an exact-match against the indexed "pkg.Type.Method".
func TestReceiverParenLookup(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "package p\n" +
			"type Widget struct{}\n" +
			"func (w *Widget) Size() int { return 1 }\n" +
			"func Make() *Widget { return nil }\n" +
			"func Use(w *Widget) {}\n",
	})
	res, _ := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}

	// The stored qualified name for a method is "pkg.Type.Method"; the
	// receiver-paren form is what Go tooling and #symbol lookups often use.
	stored := "atomic.example.Widget.Size"
	parens := "atomic.example.(*Widget).Size"

	sym, err := s.SymbolByName(parens)
	if err != nil {
		t.Fatalf("SymbolByName(%q): %v", parens, err)
	}
	if sym.QualifiedName != stored {
		t.Fatalf("SymbolByName(%q).QualifiedName = %q, want %q", parens, sym.QualifiedName, stored)
	}

	// Make() calls the receiver method; Use() takes a *Widget. Verify edge
	// lookups succeed with the parens form on both directions.
	if _, err := s.EdgesTo(parens); err != nil {
		t.Fatalf("EdgesTo(%q): %v", parens, err)
	}
	if _, err := s.EdgesFrom(parens); err != nil {
		t.Fatalf("EdgesFrom(%q): %v", parens, err)
	}
	if _, err := s.EdgesForNodes([]string{parens}, nil, false); err != nil {
		t.Fatalf("EdgesForNodes([%q]): %v", parens, err)
	}

	// Idempotency: the normalized name still resolves.
	if _, err := s.SymbolByName(stored); err != nil {
		t.Fatalf("SymbolByName(%q): %v", stored, err)
	}

	// A non-method symbol with no receiver wrapper is unaffected.
	if _, err := s.SymbolByName("atomic.example.Make"); err != nil {
		t.Fatalf("SymbolByName(%q): %v", "atomic.example.Make", err)
	}
}
