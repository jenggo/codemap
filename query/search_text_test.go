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

	matches, corrections, hint, err := query.SearchTextWithCorrections(s, "SrchLexicon", "", false, 0)
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
	if hint != "" {
		t.Fatalf("hint must stay empty when results exist, got %q", hint)
	}
	if !hasMatch(matches, "lex.go", "SearchLexicon") {
		t.Fatalf("corrected search did not find the symbol: %+v", matches)
	}
}

// TestSearchTextAlternationMixedTypoCorrected pins per-alternative
// correction: with pattern BuildMCPKeyMap|Enabel the good alternative
// already surfaces results, and the correction pass still reports the
// replacement for the misspelled alternative without changing the results.
func TestSearchTextAlternationMixedTypoCorrected(t *testing.T) {
	s := buildTextSearchStore(t, map[string]string{
		"keymap.go": "package fused\n\n// BuildMCPKeyMap builds keys\nfunc BuildMCPKeyMap() {}\n",
		"enable.go": "package fused\n\n// Enable enables things\nfunc Enable() {}\n",
	})

	matches, corrections, hint, err := query.SearchTextWithCorrections(s, "BuildMCPKeyMap|Enabel", "", false, 0)
	if err != nil {
		t.Fatalf("SearchTextWithCorrections: %v", err)
	}
	if !hasMatch(matches, "keymap.go", "BuildMCPKeyMap") {
		t.Fatalf("good alternative did not surface results: %+v", matches)
	}
	want := []fuse.Correction{{Original: "Enabel", Corrected: "enable"}}
	if len(corrections) != 1 || corrections[0] != want[0] {
		t.Fatalf("corrections = %+v, want %+v", corrections, want)
	}
	if hint != "" {
		t.Fatalf("hint must stay empty when results exist, got %q", hint)
	}
}

// TestSearchTextSufficientResultsNoCorrection pins the floor behavior: a
// query that already returns results never triggers the correction pass.
func TestSearchTextSufficientResultsNoCorrection(t *testing.T) {
	s := buildTextSearchStore(t, map[string]string{
		"exact.go": "package fused\n\n// alpha beta exact\nfunc Exact() {}\n",
	})

	matches, corrections, _, err := query.SearchTextWithCorrections(s, "alpha", "", false, 0)
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

// TestSearchTextAlternationEndToEnd pins the fused-pipeline alternation
// contract over a real indexed module: literal `A|B` returns matches for
// either side (zero results before the fix), `A B|C` respects (A AND B) OR
// C precedence, and every returned line is consistent with what the pattern
// claims.
func TestSearchTextAlternationEndToEnd(t *testing.T) {
	s := buildTextSearchStore(t, map[string]string{
		"alpha.go": "package fused\n\n// Alpha does alpha things\nfunc Alpha() {}\n",
		"beta.go":  "package fused\n\n// Beta does beta things\nfunc Beta() {}\n",
		"gamma.go": "package fused\n\n// Gamma standalone\nfunc Gamma() {}\n",
	})

	t.Run("either side matches", func(t *testing.T) {
		matches, err := query.SearchText(s, "Alpha|Beta", "", false, 0)
		if err != nil {
			t.Fatalf("SearchText alternation: %v", err)
		}
		if !hasMatch(matches, "alpha.go", "Alpha") {
			t.Fatalf("alternation missed the Alpha side: %+v", matches)
		}
		if !hasMatch(matches, "beta.go", "Beta") {
			t.Fatalf("alternation missed the Beta side: %+v", matches)
		}
		for _, m := range matches {
			lower := strings.ToLower(m.Line)
			if !strings.Contains(lower, "alpha") && !strings.Contains(lower, "beta") {
				t.Fatalf("returned line matches neither alternative: %+v", m)
			}
		}
	})

	t.Run("mixed AND within OR", func(t *testing.T) {
		matches, err := query.SearchText(s, "Alpha Beta|Gamma", "", false, 0)
		if err != nil {
			t.Fatalf("SearchText mixed alternation: %v", err)
		}
		if !hasMatch(matches, "gamma.go", "Gamma") {
			t.Fatalf("OR alternative missed gamma.go: %+v", matches)
		}
		for _, m := range matches {
			if strings.Contains(m.FilePath, "alpha.go") || strings.Contains(m.FilePath, "beta.go") {
				t.Fatalf("single-side file over-matched the AND alternative: %+v", m)
			}
		}
	})

	t.Run("empty result on metacharacter pattern gets regex hint", func(t *testing.T) {
		_, corrections, hint, err := query.SearchTextWithCorrections(s, "Zilch|Nothing", "", false, 0)
		if err != nil {
			t.Fatalf("SearchTextWithCorrections: %v", err)
		}
		if hint == "" {
			t.Fatal("expected a regex-metacharacter hint on an empty literal result")
		}
		if !strings.Contains(hint, "is_regex") {
			t.Fatalf("hint should point at is_regex: %q", hint)
		}
		_ = corrections
	})

	t.Run("clean-pattern miss gets no hint", func(t *testing.T) {
		_, _, hint, err := query.SearchTextWithCorrections(s, "zzzqqqwwww", "", false, 0)
		if err != nil {
			t.Fatalf("SearchTextWithCorrections: %v", err)
		}
		if hint != "" {
			t.Fatalf("clean pattern must not carry a hint, got %q", hint)
		}
	})
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
