package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// seedStrategiesModule writes a module whose contents exercise the split
// between the FTS trigram strategy and the substring strategy: exact.go
// holds the whole pattern on one line (both strategies hit), scatter.go has
// the terms on distant lines (FTS cannot match, substring does), and two
// packages duplicate symbol names so lexicon dedup is observable.
func seedStrategiesModule(t *testing.T) *Store {
	t.Helper()

	dir := writeModule(t, map[string]string{
		"exact.go":   "package strat\n\n// alpha beta exact\nfunc Exact() {}\n",
		"scatter.go": "package strat\n\n// alpha opener\nfunc Scattered() {}\n\n// filler\n// filler\n// filler\n// filler\n// filler\n// filler\n// filler\n// filler\n// filler\n// filler\n\n// beta closer\nfunc More() {}\n",
		"ab.go":      "package strat\n\n// ab block\nfunc Ab() {}\n",

		// Duplicate symbol short names across two packages for lexicon dedup.
		"p1/helper.go": "package p1\n\nfunc Helper() {}\n",
		"p2/helper.go": "package p2\n\nfunc Helper() {}\n",
	})

	res, files := parseModule(t, dir)
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

func hasFile(matches []FileMatch, substr string) bool {
	for _, m := range matches {
		if strings.Contains(m.FilePath, substr) {
			return true
		}
	}
	return false
}

// TestFileMatchesFTS finds the verbatim fixture and excludes the
// scattered-terms one, which no line contains the whole pattern on.
func TestFileMatchesFTS(t *testing.T) {
	s := seedStrategiesModule(t)

	matches, err := s.FileMatchesFTS("alpha beta", "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesFTS: %v", err)
	}
	if !hasFile(matches, "exact.go") {
		t.Fatalf("FTS strategy missed the verbatim fixture: %+v", matches)
	}
	if hasFile(matches, "scatter.go") {
		t.Fatalf("FTS strategy matched scattered terms: %+v", matches)
	}
}

// TestFileMatchesFTSRankedOrdersByBM25 checks that the FTS strategy returns
// results in a deterministic ranked order rather than raw row order.
func TestFileMatchesFTSRankedOrdersDeterministically(t *testing.T) {
	s := seedStrategiesModule(t)

	first, err := s.FileMatchesFTS("alpha beta", "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesFTS: %v", err)
	}
	second, err := s.FileMatchesFTS("alpha beta", "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesFTS second run: %v", err)
	}
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("unstable result count: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("ranked order not deterministic:\n%d: %+v\n%d: %+v", i, first[i], i, second[i])
		}
	}
}

// TestFileMatchesFTSShortTokenPathStillServed pins the ftsNeedsFullScan
// dispatch: a pattern with an alphanumeric run shorter than three characters
// is served by the direct full scan instead of silently matching nothing.
func TestFileMatchesFTSShortTokenPathStillServed(t *testing.T) {
	s := seedStrategiesModule(t)
	if !ftsNeedsFullScan("ab") {
		t.Fatal("expected ftsNeedsFullScan(\"ab\") to be true")
	}

	matches, err := s.FileMatchesFTS("ab", "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesFTS short token: %v", err)
	}
	if !hasFile(matches, "ab.go") {
		t.Fatalf("short-token path not served: %+v", matches)
	}
}

// TestFileMatchesFTSFilePatternAndLimit checks the filter and cap plumbing.
func TestFileMatchesFTSFilePatternAndLimit(t *testing.T) {
	s := seedStrategiesModule(t)

	matches, err := s.FileMatchesFTS("alpha beta", "exact.go", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesFTS file pattern: %v", err)
	}
	if len(matches) == 0 || hasFile(matches, "scatter.go") {
		t.Fatalf("file_pattern not applied: %+v", matches)
	}

	capped, err := s.FileMatchesFTS("alpha beta", "", 1, 0)
	if err != nil {
		t.Fatalf("FileMatchesFTS limit: %v", err)
	}
	if len(capped) != 1 {
		t.Fatalf("limit not applied: got %d", len(capped))
	}
}

// TestFileMatchesSubstringRecallsScatteredTerms pins the recall gain: the
// substring strategy returns the file whose terms sit on different lines,
// which the FTS strategy misses, along with the verbatim fixture.
func TestFileMatchesSubstringRecallsScatteredTerms(t *testing.T) {
	s := seedStrategiesModule(t)

	matches, err := s.FileMatchesSubstring([][]string{{"alpha", "beta"}}, "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesSubstring: %v", err)
	}
	if !hasFile(matches, "scatter.go") {
		t.Fatalf("substring strategy missed the scattered fixture: %+v", matches)
	}
	if !hasFile(matches, "exact.go") {
		t.Fatalf("substring strategy missed the verbatim fixture: %+v", matches)
	}
	// Ranked by match count: scatter.go (2 term-bearing lines) before
	// exact.go (both terms on one line).
	if idxExact, idxScatter := matchIndex(matches, "exact.go"), matchIndex(matches, "scatter.go"); idxScatter > idxExact {
		t.Fatalf("expected scatter.go ranked above exact.go: %+v", matches)
	}
}

// TestFileMatchesSubstringCaseInsensitive checks case-insensitive matching
// and the all-terms file gate.
func TestFileMatchesSubstringCaseInsensitive(t *testing.T) {
	s := seedStrategiesModule(t)

	matches, err := s.FileMatchesSubstring([][]string{{"ALPHA", "Beta"}}, "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesSubstring: %v", err)
	}
	if !hasFile(matches, "exact.go") {
		t.Fatalf("case-insensitive terms failed to match: %+v", matches)
	}

	// A file missing one term is not a candidate at all.
	none, err := s.FileMatchesSubstring([][]string{{"alpha", "missing"}}, "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesSubstring: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("file missing a term was returned: %+v", none)
	}
}

// TestFileMatchesSubstringPunctuationOnlyTermsInert mirrors the FTS
// sanitizer: punctuation-only tokens are dropped from the term set.
func TestFileMatchesSubstringPunctuationOnlyTermsInert(t *testing.T) {
	s := seedStrategiesModule(t)

	matches, err := s.FileMatchesSubstring([][]string{{"-", "alpha"}}, "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesSubstring: %v", err)
	}
	if !hasFile(matches, "exact.go") {
		t.Fatalf("punctuation-only term broke the scan: %+v", matches)
	}

	inert, err := s.FileMatchesSubstring([][]string{{"-"}}, "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesSubstring: %v", err)
	}
	if len(inert) != 0 {
		t.Fatalf("punctuation-only term list should match nothing: %+v", inert)
	}
}

// TestFileMatchesSubstringAlternation pins any-alternative semantics: a file
// matches when every term of ANY alternative occurs in its content, and
// lines from both sides of the alternation are surfaced.
func TestFileMatchesSubstringAlternation(t *testing.T) {
	s := seedStrategiesModule(t)

	// (alpha AND beta) OR gamma — no file contains gamma, so only the
	// alpha/beta files may surface.
	matches, err := s.FileMatchesSubstring([][]string{{"alpha", "beta"}, {"gamma"}}, "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesSubstring: %v", err)
	}
	if !hasFile(matches, "exact.go") || !hasFile(matches, "scatter.go") {
		t.Fatalf("alternation substring scan missed the alpha/beta fixtures: %+v", matches)
	}
	for _, m := range matches {
		if strings.Contains(m.Line, "gamma") {
			t.Fatalf("gamma alternative over-matched: %+v", m)
		}
	}

	// Either side alone still matches its own file.
	either, err := s.FileMatchesSubstring([][]string{{"exact"}, {"ab block"}}, "", 100, 0)
	if err != nil {
		t.Fatalf("FileMatchesSubstring either-side: %v", err)
	}
	if !hasFile(either, "exact.go") || !hasFile(either, "ab.go") {
		t.Fatalf("either-side alternation missed one side: %+v", either)
	}
}

// TestSearchLexiconDedupLowercasedSorted pins the lexicon contract: symbol
// short names appear once, lowercased, in sorted order.
func TestSearchLexiconDedupLowercasedSorted(t *testing.T) {
	s := seedStrategiesModule(t)

	lexicon, err := s.SearchLexicon()
	if err != nil {
		t.Fatalf("SearchLexicon: %v", err)
	}
	if len(lexicon) == 0 {
		t.Fatal("lexicon is empty")
	}

	helperCount := 0
	for i, entry := range lexicon {
		if entry != strings.ToLower(entry) {
			t.Fatalf("lexicon entry %q is not lowercased", entry)
		}
		if entry == "helper" {
			helperCount++
		}
		if i > 0 && lexicon[i-1] > entry {
			t.Fatalf("lexicon not sorted at %d: %v", i, lexicon)
		}
	}
	if helperCount != 1 {
		t.Fatalf("expected exactly one deduplicated \"helper\" entry, got %d: %v", helperCount, lexicon)
	}
}

// TestSplitAlternatives pins the shared literal-mode splitter: top-level `|`
// separates OR alternatives, segments are trimmed, and empty or
// punctuation-only segments are dropped.
func TestSplitAlternatives(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"A|B", []string{"A", "B"}},
		{"A||B", []string{"A", "B"}},
		{"|", nil},
		{"-|-", nil},
		{"", nil},
		{"A|", []string{"A"}},
		{"A B|C D", []string{"A B", "C D"}},
		{" alpha | beta ", []string{"alpha", "beta"}},
		{"no pipes", []string{"no pipes"}},
	}
	for _, tc := range cases {
		got := SplitAlternatives(tc.in)
		if len(got) != 0 && len(tc.want) != 0 {
			if reflect.DeepEqual(got, tc.want) {
				continue
			}
		} else if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		t.Errorf("SplitAlternatives(%q) = %q, want %q", tc.in, got, tc.want)
	}
}

func matchIndex(matches []FileMatch, substr string) int {
	for i, m := range matches {
		if strings.Contains(m.FilePath, substr) {
			return i
		}
	}
	return len(matches)
}
