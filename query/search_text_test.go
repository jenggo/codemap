package query_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codemap/fuse"
	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

// buildTextSearchStore indexes a temporary module for the search_text
// pipeline tests, mirroring the store package's writeModule/parseModule
// helpers through the public parse/resolve API.
func buildTextSearchStore(t *testing.T, files map[string]string) *store.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fused.example\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), parse.FileContents(pr), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

func hasMatch(matches []store.FileMatch, pathSubstr, lineSubstr string) bool {
	for _, m := range matches {
		if strings.Contains(m.FilePath, pathSubstr) && strings.Contains(m.Line, lineSubstr) {
			return true
		}
	}
	return false
}

// TestSearchTextFindsScatteredTerms pins the recall gain of fusion: a
// multi-term pattern whose terms sit on different lines returns the file,
// which the pre-change FTS branch (verbatim whole-pattern per line) missed.
func TestSearchTextFindsScatteredTerms(t *testing.T) {
	s := buildTextSearchStore(t, map[string]string{
		"exact.go":   "package fused\n\n// alpha beta exact\nfunc Exact() {}\n",
		"scatter.go": "package fused\n\n// alpha opener\nfunc Scattered() {}\n\n// beta closer\nfunc More() {}\n",
	})

	matches, err := query.SearchText(s, "alpha beta", "", false, 0)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if !hasMatch(matches, "scatter.go", "alpha") {
		t.Fatalf("scattered-terms file not recalled: %+v", matches)
	}
	if !hasMatch(matches, "exact.go", "alpha beta") {
		t.Fatalf("verbatim file not recalled: %+v", matches)
	}
	// The file both strategies agree on ranks first.
	if !strings.Contains(matches[0].FilePath, "exact.go") {
		t.Fatalf("agreement file should rank first, got %+v", matches[0])
	}
}

// TestSearchTextMisspelledQueryIsCorrected pins the typo-correction
// scenario: a misspelled symbol returns nothing on the first pass, one
// correction re-run finds the symbol, and the corrections are surfaced.
func TestSearchTextMisspelledQueryIsCorrected(t *testing.T) {
	s := buildTextSearchStore(t, map[string]string{
		"lex.go":   "package fused\n\n// SearchLexicon lists known terms\nfunc SearchLexicon() {}\n",
		"other.go": "package fused\n\n// UnrelatedHelper does something else\nfunc UnrelatedHelper() {}\n",
	})

	matches, corrections, err := query.SearchTextWithCorrections(s, "SrchLexicon", "", false, 0)
	if err != nil {
		t.Fatalf("SearchTextWithCorrections: %v", err)
	}
	if len(corrections) != 1 {
		t.Fatalf("expected one correction, got %+v", corrections)
	}
	want := fuse.Correction{Original: "SrchLexicon", Corrected: "searchlexicon"}
	if corrections[0] != want {
		t.Fatalf("correction = %+v, want %+v", corrections[0], want)
	}
	if !hasMatch(matches, "lex.go", "SearchLexicon") {
		t.Fatalf("corrected search did not find the symbol: %+v", matches)
	}
}

// TestSearchTextSufficientResultsNoCorrection pins the floor behavior: a
// query that already returns results never triggers the correction pass.
func TestSearchTextSufficientResultsNoCorrection(t *testing.T) {
	s := buildTextSearchStore(t, map[string]string{
		"exact.go": "package fused\n\n// alpha beta exact\nfunc Exact() {}\n",
	})

	matches, corrections, err := query.SearchTextWithCorrections(s, "alpha", "", false, 0)
	if err != nil {
		t.Fatalf("SearchTextWithCorrections: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("expected matches for a present term")
	}
	if corrections != nil {
		t.Fatalf("no correction should run when results suffice, got %+v", corrections)
	}
}

// TestSearchTextRegexModeUnchanged pins the regex guarantee: regex mode
// dispatches to the original store path with no fusion, reranking, or
// correction, producing exactly the pre-change matches.
func TestSearchTextRegexModeUnchanged(t *testing.T) {
	s := buildTextSearchStore(t, map[string]string{
		"regex.go": "package fused\n\nfunc Alpha() {}\nfunc Beta() {}\n",
	})

	matches, err := query.SearchText(s, `func [A-Z]`, "", true, 1)
	if err != nil {
		t.Fatalf("SearchText regex: %v", err)
	}

	var regexFile string
	// The files table keys are the paths parse.Run observed; resolve the
	// fixture's expected content against whatever key holds regex.go.
	for _, m := range matches {
		if strings.Contains(m.FilePath, "regex.go") {
			regexFile = m.FilePath
		}
	}
	want := []store.FileMatch{
		{
			FilePath:      regexFile,
			LineNumber:    3,
			Line:          "func Alpha() {}",
			ContextBefore: "",
			ContextAfter:  "func Beta() {}",
		},
		{
			FilePath:      regexFile,
			LineNumber:    4,
			Line:          "func Beta() {}",
			ContextBefore: "func Alpha() {}",
		},
	}
	if !reflect.DeepEqual(matches, want) {
		t.Fatalf("regex output changed:\ngot  %+v\nwant %+v", matches, want)
	}
}
