package query_test

import (
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

// TestFindDeclarationByPosition covers every position-mode answer: inside a
// body, inside a method, inside a function literal, on a declaration line, and
// outside every symbol.
func declarationStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir, err := filepath.Abs("../testdata/fixtures/declaration")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	parsed, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(resolve.Run(parsed), parse.FileContents(parsed), nil); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return st, dir
}

func TestFindDeclarationByPosition(t *testing.T) {
	st, dir := declarationStore(t)
	file := filepath.Join(dir, "decl.go")

	tests := []struct {
		name   string
		line   int
		wantQN string
	}{
		{name: "inside a function body", line: 20, wantQN: "declfixture.Outer"},
		{name: "inside a method body", line: 11, wantQN: "declfixture.Gadget.Describe"},
		{name: "inside a nested function literal", line: 18, wantQN: "declfixture.Outer"},
		{name: "on a declaration line", line: 16, wantQN: "declfixture.Outer"},
		{name: "inside a struct type", line: 6, wantQN: "declfixture.Gadget"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := query.FindDeclaration(st, file, tt.line, 0, "")
			if err != nil {
				t.Fatalf("find_declaration: %v", err)
			}
			if result.Symbol == nil {
				t.Fatalf("expected a symbol at %s:%d, got %+v", file, tt.line, result)
			}
			if result.Symbol.QualifiedName != tt.wantQN {
				t.Errorf("line %d resolved to %s, want %s", tt.line, result.Symbol.QualifiedName, tt.wantQN)
			}
			if result.Symbol.File == "" || result.Symbol.Line == 0 {
				t.Errorf("declaration location must be populated, got %+v", result.Symbol)
			}
		})
	}

	t.Run("outside every symbol", func(t *testing.T) {
		result, err := query.FindDeclaration(st, file, 2, 0, "")
		if err != nil {
			t.Fatalf("a position outside every symbol must not error: %v", err)
		}
		if result.Symbol != nil {
			t.Errorf("expected no symbol, got %+v", result.Symbol)
		}
		if !strings.Contains(result.Message, "no indexed symbol contains") {
			t.Errorf("expected a no-symbol message, got %q", result.Message)
		}
	})

	t.Run("line out of range", func(t *testing.T) {
		_, err := query.FindDeclaration(st, file, 500, 0, "")
		if err == nil {
			t.Fatal("expected an out-of-range error")
		}
		if !strings.Contains(err.Error(), "24 lines") {
			t.Errorf("the error must report the actual line count, got %q", err.Error())
		}
	})

	t.Run("file not indexed", func(t *testing.T) {
		_, err := query.FindDeclaration(st, filepath.Join(dir, "missing.go"), 1, 0, "")
		if err == nil {
			t.Fatal("expected a not-indexed error")
		}
		if !strings.Contains(err.Error(), "not indexed") {
			t.Errorf("the error must say the file is not indexed, got %q", err.Error())
		}
	})
}

// TestFindDeclarationByOccurrence covers occurrence mode: an imported symbol, a
// same-file declaration, an ambiguous name, and an unresolvable name.
func TestFindDeclarationByOccurrence(t *testing.T) {
	st, dir := declarationStore(t)

	t.Run("imported symbol", func(t *testing.T) {
		result, err := query.FindDeclaration(st, filepath.Join(dir, "uses_one.go"), 0, 0, "Imported")
		if err != nil {
			t.Fatalf("find_declaration: %v", err)
		}
		if result.Symbol == nil {
			t.Fatalf("expected a declaration, got %+v", result)
		}
		if result.Symbol.QualifiedName != "declfixture/other.Imported" {
			t.Errorf("resolved to %s, want declfixture/other.Imported", result.Symbol.QualifiedName)
		}
		if !strings.HasSuffix(result.Symbol.File, filepath.Join("other", "other.go")) {
			t.Errorf("expected the declaration in another file, got %s", result.Symbol.File)
		}
	})

	t.Run("same-file declaration", func(t *testing.T) {
		result, err := query.FindDeclaration(st, filepath.Join(dir, "decl.go"), 0, 0, "Helper")
		if err != nil {
			t.Fatalf("find_declaration: %v", err)
		}
		if result.Symbol == nil {
			t.Fatalf("expected a declaration, got %+v", result)
		}
		if result.Symbol.QualifiedName != "declfixture.Helper" {
			t.Errorf("resolved to %s, want declfixture.Helper", result.Symbol.QualifiedName)
		}
	})

	t.Run("ambiguous name", func(t *testing.T) {
		// uses.go references two packages that both declare Imported, so the
		// bare name resolves to two indexed symbols and both are returned.
		result, err := query.FindDeclaration(st, filepath.Join(dir, "uses.go"), 0, 0, "Imported")
		if err != nil {
			t.Fatalf("find_declaration: %v", err)
		}
		if result.Symbol != nil {
			t.Errorf("an ambiguous name must not pick one symbol, got %+v", result.Symbol)
		}
		if len(result.Candidates) != 2 {
			t.Fatalf("expected 2 candidates, got %+v", result.Candidates)
		}
		for _, c := range result.Candidates {
			if c.File == "" || c.Line == 0 {
				t.Errorf("candidate must carry file:line, got %+v", c)
			}
		}
	})

	t.Run("unresolvable name", func(t *testing.T) {
		result, err := query.FindDeclaration(st, filepath.Join(dir, "decl.go"), 0, 0, "NoSuchSymbol")
		if err != nil {
			t.Fatalf("an unresolvable name must not error: %v", err)
		}
		if result.Symbol != nil || len(result.Candidates) != 0 {
			t.Errorf("expected no resolution, got %+v", result)
		}
		if !strings.Contains(result.Message, "could not resolve") {
			t.Errorf("expected a could-not-resolve message, got %q", result.Message)
		}
	})
}

// TestFindDeclarationAgreesWithPattern asserts the shared enclosing-symbol walk
// makes both tools report the same symbol for the same node.
func TestFindDeclarationAgreesWithPattern(t *testing.T) {
	st, dir := declarationStore(t)
	file := filepath.Join(dir, "decl.go")

	matches, err := query.PatternSearch(st, "return Helper()", "", "")
	if err != nil {
		t.Fatalf("pattern search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one match, got %+v", matches)
	}
	if matches[0].Symbol == "" {
		t.Fatalf("pattern match must be attributed to a symbol, got %+v", matches[0])
	}

	result, err := query.FindDeclaration(st, file, matches[0].Line, 0, "")
	if err != nil {
		t.Fatalf("find_declaration: %v", err)
	}
	if result.Symbol == nil {
		t.Fatalf("expected a symbol at the match line, got %+v", result)
	}
	if result.Symbol.QualifiedName != matches[0].Symbol {
		t.Errorf("pattern says %s, find_declaration says %s", matches[0].Symbol, result.Symbol.QualifiedName)
	}
}
