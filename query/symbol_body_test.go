package query

import (
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

func setupQueryTestStore(t *testing.T, fixturePath string) *store.Store {
	t.Helper()
	abs, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	parseResult, err := parse.Run(abs)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store create: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("store write: %v", err)
	}
	return s
}

func TestGetSymbolBodyFunction(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/simple")
	result, err := GetSymbolBody(s, "codemap/testdata/simple.NewParser", 0, false)
	if err != nil {
		t.Fatalf("GetSymbolBody: %v", err)
	}
	if result.QualifiedName != "codemap/testdata/simple.NewParser" {
		t.Errorf("qualified_name: got %s", result.QualifiedName)
	}
	if !strings.Contains(result.Body, "func NewParser(input string) *Parser {") {
		t.Errorf("body does not contain func decl\nbody: %s", result.Body)
	}
	if !strings.Contains(result.Body, "return &Parser{input: input}") {
		t.Errorf("body missing return\nbody: %s", result.Body)
	}
}

func TestGetSymbolBodyMethod(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/simple")
	result, err := GetSymbolBody(s, "codemap/testdata/simple.Parser.Parse", 0, true)
	if err != nil {
		t.Fatalf("GetSymbolBody: %v", err)
	}
	if !strings.Contains(result.Body, "Parse processes the input") {
		t.Errorf("body missing doc\nbody: %s", result.Body)
	}
	if !strings.Contains(result.Body, "func (p *Parser) Parse() (string, error) {") {
		t.Errorf("body missing method decl\nbody: %s", result.Body)
	}
}

func TestGetSymbolBodyGroupedVar(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/generics")
	result, err := GetSymbolBody(s, "codemap/testdata/generics.Pi", 0, true)
	if err != nil {
		t.Fatalf("GetSymbolBody: %v", err)
	}
	if !result.PartOfGroup {
		t.Error("expected part_of_group=true")
	}
	if result.GroupMembers != 3 {
		t.Errorf("group_members: got %d want 3", result.GroupMembers)
	}
	if !strings.Contains(result.Body, "const (") {
		t.Errorf("body missing const ( group\nbody: %s", result.Body)
	}
	if !strings.Contains(result.Body, "Pi     = 3.14") {
		t.Errorf("body missing Pi decl\nbody: %s", result.Body)
	}
}

func TestGetSymbolBodyIncludeDocFalse(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/simple")
	result, err := GetSymbolBody(s, "codemap/testdata/simple.NewParser", 0, false)
	if err != nil {
		t.Fatalf("GetSymbolBody: %v", err)
	}
	if strings.Contains(result.Body, "NewParser creates") {
		t.Errorf("body should not include doc\nbody: %s", result.Body)
	}
	if !strings.Contains(result.Body, "func NewParser") {
		t.Errorf("body missing func decl\nbody: %s", result.Body)
	}
}

func TestGetSymbolBodyContextLines(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/simple")
	result, err := GetSymbolBody(s, "codemap/testdata/simple.NewParser", 2, false)
	if err != nil {
		t.Fatalf("GetSymbolBody: %v", err)
	}
	if result.ContextBefore == "" {
		t.Error("expected non-empty context_before")
	}
	if result.ContextAfter == "" {
		t.Error("expected non-empty context_after")
	}
}

func TestGetSymbolBodyNotFound(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/simple")
	_, err := GetSymbolBody(s, "codemap/testdata/simple.NoSuch", 0, true)
	if err == nil {
		t.Fatal("expected error for missing symbol")
	}
}

func TestGetSymbolBodyInterface(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/interfaces")
	result, err := GetSymbolBody(s, "codemap/testdata/interfaces.Reader", 0, true)
	if err != nil {
		t.Fatalf("GetSymbolBody: %v", err)
	}
	if !strings.Contains(result.Body, "type Reader interface") {
		t.Errorf("body missing type Reader interface\nbody: %s", result.Body)
	}
}

func TestSymbolMissSuggestions(t *testing.T) {
	s := setupQueryTestStore(t, "../testdata/simple")

	t.Run("show suggests the right package prefix", func(t *testing.T) {
		_, err := Show(s, "codemap/testdata/wrong.NewParser")
		if err == nil {
			t.Fatal("expected error for wrong package prefix")
		}
		msg := err.Error()
		if !strings.Contains(msg, "symbol not found: codemap/testdata/wrong.NewParser") {
			t.Errorf("miss message lost the requested name: %q", msg)
		}
		if !strings.Contains(msg, "did you mean:") || !strings.Contains(msg, "codemap/testdata/simple.NewParser") {
			t.Errorf("miss message lacks the correct candidate: %q", msg)
		}
	})

	t.Run("get_symbol_body suggests the same candidates", func(t *testing.T) {
		_, err := GetSymbolBody(s, "codemap/testdata/wrong.NewParser", 0, false)
		if err == nil {
			t.Fatal("expected error for wrong package prefix")
		}
		if !strings.Contains(err.Error(), "did you mean:") || !strings.Contains(err.Error(), "codemap/testdata/simple.NewParser") {
			t.Errorf("miss message lacks the correct candidate: %q", err)
		}
	})

	t.Run("unknown name stays a plain miss", func(t *testing.T) {
		_, err := GetSymbolBody(s, "codemap/testdata/simple.ZzzNotAThing", 0, false)
		if err == nil {
			t.Fatal("expected error for unknown symbol")
		}
		if strings.Contains(err.Error(), "did you mean") {
			t.Errorf("unknown name should not suggest candidates: %q", err)
		}
	})
}

func TestRankSymbolCandidates(t *testing.T) {
	syms := []store.Symbol{
		{QualifiedName: "pkg/a.NewParserX", Name: "NewParserX"},
		{QualifiedName: "pkg/b.NewParser", Name: "NewParser"},
		{QualifiedName: "pkg/b.NewParser", Name: "NewParser"},
		{QualifiedName: "pkg/b.OtherParser", Name: "OtherParser"},
	}

	got := rankSymbolCandidates(syms, "NewParser", "pkg/b", 5)
	if len(got) != 3 {
		t.Fatalf("candidates = %v, want 3 unique", got)
	}
	if got[0] != "pkg/b.NewParser" {
		t.Errorf("exact short-name match must rank first, got %v", got)
	}
	if got[1] != "pkg/b.OtherParser" {
		t.Errorf("same-package match must rank before unrelated matches, got %v", got)
	}
	if got[2] != "pkg/a.NewParserX" {
		t.Errorf("unrelated match must rank last, got %v", got)
	}

	capped := rankSymbolCandidates(syms, "NewParser", "", 2)
	if len(capped) != 2 {
		t.Errorf("cap not applied: %v", capped)
	}
}
