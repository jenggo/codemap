package query_test

import (
	"os"
	"path/filepath"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

func setupStore(t *testing.T) *store.Store {
	t.Helper()
	absPath, err := filepath.Abs("../testdata/simple")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)

	files := make(map[string]string)
	for path := range parseResult.Files {
		data, err := os.ReadFile(path)
		if err == nil {
			files[filepath.Base(path)] = string(data)
		}
	}

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult, files, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

func TestSearchTextLiteral(t *testing.T) {
	s := setupStore(t)
	matches, err := query.SearchText(s, "Parser", "", false, 0)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("expected at least one match for 'Parser'")
	}
	found := false
	for _, m := range matches {
		if m.FilePath == "" {
			t.Error("expected non-empty FilePath")
		}
		if m.LineNumber == 0 {
			t.Error("expected non-zero LineNumber")
		}
		if m.Line == "" {
			t.Error("expected non-empty Line")
		}
		if filepath.Base(m.FilePath) == "parser.go" {
			found = true
		}
	}
	if !found {
		t.Error("expected match in parser.go")
	}
}

func TestSearchTextRegex(t *testing.T) {
	t.Skip("REGEXP function not available in this SQLite build")
	s := setupStore(t)
	matches, err := query.SearchText(s, `func\s+\w+`, "", true, 0)
	if err != nil {
		t.Fatalf("SearchText regex: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("expected at least one regex match")
	}
}

func TestSearchTextNoMatches(t *testing.T) {
	s := setupStore(t)
	matches, err := query.SearchText(s, "zzz_nonexistent_zzz", "", false, 0)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}
}

func TestSearchTextContextLines(t *testing.T) {
	s := setupStore(t)
	matches, err := query.SearchText(s, "Parse", "", false, 1)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
	hasContext := false
	for _, m := range matches {
		if m.ContextBefore != "" || m.ContextAfter != "" {
			hasContext = true
		}
	}
	if !hasContext {
		t.Error("expected at least one match with context lines")
	}
}

func TestContextBundleBasic(t *testing.T) {
	s := setupStore(t)
	bundle, err := query.ContextBundle(s, "codemap/testdata/simple.NewParser", 0)
	if err != nil {
		t.Fatalf("ContextBundle: %v", err)
	}
	if bundle == nil {
		t.Fatal("expected non-nil bundle")
	}
	if bundle.QualifiedName != "codemap/testdata/simple.NewParser" {
		t.Errorf("expected qualified name 'codemap/testdata/simple.NewParser', got %q", bundle.QualifiedName)
	}
	if bundle.Body == "" {
		t.Error("expected non-empty body")
	}
	if bundle.TokenEstimate == 0 {
		t.Error("expected non-zero token estimate")
	}
}

func TestContextBundleNotFound(t *testing.T) {
	s := setupStore(t)
	_, err := query.ContextBundle(s, "nonexistent.Symbol", 0)
	if err == nil {
		t.Error("expected error for non-existent symbol")
	}
}

func TestContextBundleBudget(t *testing.T) {
	s := setupStore(t)
	// Verify that budget parameter is accepted and trimming doesn't error.
	// Body is always preserved; only callees/callers/sameFile are trimmed.
	bundle, err := query.ContextBundle(s, "codemap/testdata/simple.NewParser", 100)
	if err != nil {
		t.Fatalf("ContextBundle: %v", err)
	}
	if bundle == nil {
		t.Fatal("expected non-nil bundle")
	}
	if bundle.Body == "" {
		t.Error("expected non-empty body even with small budget")
	}
	// With a small budget, surrounding context should be trimmed
	defaultBundle, _ := query.ContextBundle(s, "codemap/testdata/simple.NewParser", 0)
	if len(bundle.Callees) > len(defaultBundle.Callees) {
		t.Error("expected trimmed callees <= default callees")
	}
	if len(bundle.Callers) > len(defaultBundle.Callers) {
		t.Error("expected trimmed callers <= default callers")
	}
	if len(bundle.SameFile) > len(defaultBundle.SameFile) {
		t.Error("expected trimmed sameFile <= default sameFile")
	}
}

func TestHotspotsDefault(t *testing.T) {
	s := setupStore(t)
	hotspots, err := query.Hotspots(s, 0, 0, 0)
	if err != nil {
		t.Fatalf("Hotspots: %v", err)
	}
	// simple fixture has no git history, so churn=0 and risk=0 for all
	// but symbols with complexity>0 still pass minComplexity=0 filter
	for _, h := range hotspots {
		if h.RiskScore != 0 {
			t.Errorf("expected risk score 0 with no churn, got %f", h.RiskScore)
		}
	}
}

func TestHotspotsTopN(t *testing.T) {
	s := setupStore(t)
	hotspots, err := query.Hotspots(s, 5, 0, 0)
	if err != nil {
		t.Fatalf("Hotspots: %v", err)
	}
	if len(hotspots) > 5 {
		t.Errorf("expected at most 5 hotspots, got %d", len(hotspots))
	}
}

func TestSymbolImportanceDefault(t *testing.T) {
	s := setupStore(t)
	entries, err := query.SymbolImportance(s, 0, 0)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}
	if entries == nil {
		t.Fatal("expected non-nil entries")
	}
	// May be empty if no import edges, which is fine
	for _, e := range entries {
		if e.QualifiedName == "" {
			t.Error("expected non-empty QualifiedName")
		}
		if e.Importance < 0 {
			t.Errorf("expected non-negative importance, got %f", e.Importance)
		}
	}
}

func TestSymbolImportanceTopN(t *testing.T) {
	s := setupStore(t)
	entries, err := query.SymbolImportance(s, 3, 0)
	if err != nil {
		t.Fatalf("SymbolImportance: %v", err)
	}
	if len(entries) > 3 {
		t.Errorf("expected at most 3 entries, got %d", len(entries))
	}
}
