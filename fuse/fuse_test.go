package fuse

import (
	"reflect"
	"testing"
)

func c(path string, line int) Candidate {
	return Candidate{FilePath: path, LineNumber: line, Score: 1.0}
}

func paths(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.FilePath
	}
	return out
}

// TestFuseSingleStrategyRecall pins the recall guarantee: a candidate found
// by only one strategy still appears in the fused output.
func TestFuseSingleStrategyRecall(t *testing.T) {
	a := []Candidate{c("a.go", 1), c("b.go", 2)}
	b := []Candidate{c("c.go", 3)}

	fused := Fuse(a, b, DefaultK)
	got := paths(fused)
	for _, want := range []string{"a.go", "b.go", "c.go"} {
		found := false
		for _, p := range got {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("candidate %s dropped by fusion, got %v", want, got)
		}
	}
}

// TestFuseAgreementBoost pins the precision guarantee: the same candidate
// ranked by both strategies outranks every single-strategy candidate, even
// ones that ranked above it in an individual list.
func TestFuseAgreementBoost(t *testing.T) {
	a := []Candidate{c("x.go", 1), c("shared.go", 5)}
	b := []Candidate{c("y.go", 1), c("shared.go", 5)}

	fused := Fuse(a, b, DefaultK)
	if len(fused) == 0 || fused[0].FilePath != "shared.go" {
		t.Fatalf("agreement candidate should rank first, got %v", paths(fused))
	}
	// Agreement score must exceed either single-strategy contribution.
	shared := fused[0].Score
	if shared <= 1.0/(DefaultK+1) || shared <= 1.0/(DefaultK+2) {
		t.Fatalf("agreement score %v does not exceed single-strategy scores", shared)
	}
}

// TestFuseTieDeterminism pins the tiebreak: equally scored candidates order
// by file path first (candidates at the same rank in disjoint lists), then
// by line number within one file (same file, same rank), and repeated fusion
// of the same inputs yields the identical order.
func TestFuseTieDeterminism(t *testing.T) {
	a := []Candidate{c("b.go", 7)}
	b := []Candidate{c("a.go", 3)}
	first := Fuse(a, b, DefaultK)
	second := Fuse(a, b, DefaultK)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("fusion is not deterministic:\n%+v\n%+v", first, second)
	}
	if got := paths(first); !reflect.DeepEqual(got, []string{"a.go", "b.go"}) {
		t.Fatalf("path tiebreak violated: %v", got)
	}

	// Same path, same score: line ascending.
	same := Fuse([]Candidate{c("a.go", 9)}, []Candidate{c("a.go", 3)}, DefaultK)
	if same[0].LineNumber != 3 || same[1].LineNumber != 9 {
		t.Fatalf("line tiebreak violated: %+v", same)
	}
}

// TestFuseDuplicateWithinList keeps the first occurrence when one strategy
// reports the same (file, line) twice, so RRF cannot double-count it.
func TestFuseDuplicateWithinList(t *testing.T) {
	a := []Candidate{c("a.go", 1), c("a.go", 1)}
	b := []Candidate{c("b.go", 2)}

	fused := Fuse(a, b, DefaultK)
	if len(fused) != 2 {
		t.Fatalf("expected 2 unique candidates, got %d: %+v", len(fused), fused)
	}
	if fused[0].Score != 1.0/(DefaultK+1) {
		t.Fatalf("duplicate contributed twice: score %v", fused[0].Score)
	}
}

// TestFuseZeroKFallsBack checks the k guard.
func TestFuseZeroKFallsBack(t *testing.T) {
	a := []Candidate{c("a.go", 1)}
	fused := Fuse(a, nil, 0)
	if fused[0].Score != 1.0/(DefaultK+1) {
		t.Fatalf("k<=0 did not fall back to DefaultK: score %v", fused[0].Score)
	}
}

// TestRerankProximityOrdering pins the spec scenario: two files that both
// contain all query terms are ordered by their minimum term window — the
// file with terms within a few lines outranks the one with terms scattered
// across distant sections.
func TestRerankProximityOrdering(t *testing.T) {
	// Same base scores (1.0) and line text carrying the terms.
	tight := []Candidate{
		{FilePath: "tight.go", LineNumber: 3, Line: "// alpha here", Score: 1.0},
		{FilePath: "tight.go", LineNumber: 6, Line: "// beta there", Score: 1.0},
	}
	scattered := []Candidate{
		{FilePath: "far.go", LineNumber: 3, Line: "// alpha here", Score: 1.0},
		{FilePath: "far.go", LineNumber: 900, Line: "// beta there", Score: 1.0},
	}
	both := append(append([]Candidate{}, tight...), scattered...)

	reranked := RerankByProximity(both, []string{"Alpha", "beta"})
	if got := paths(reranked); !reflect.DeepEqual(got, []string{"tight.go", "tight.go", "far.go", "far.go"}) {
		t.Fatalf("tight-window file should rank first, got %v", got)
	}
	if reranked[0].Score <= reranked[2].Score {
		t.Fatalf("tight candidate score %v should exceed scattered score %v", reranked[0].Score, reranked[2].Score)
	}
	if reranked[0].Score > both[0].Score*proximityCap+1e-12 {
		t.Fatalf("multiplier exceeded cap: %v", reranked[0].Score)
	}
}

// TestRerankSingleTermUnchanged ensures single-term queries bypass proximity.
func TestRerankSingleTermUnchanged(t *testing.T) {
	in := []Candidate{c("b.go", 2), c("a.go", 1)}
	out := RerankByProximity(in, []string{"alpha"})
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("single-term rerank changed results: %+v", out)
	}
}

// TestRerankPartialCoverageNoBoost: a file that never covers every term gets
// no boost, while a file whose candidates cover all terms does.
func TestRerankPartialCoverageNoBoost(t *testing.T) {
	in := []Candidate{
		{FilePath: "only-alpha.go", LineNumber: 1, Line: "// alpha only", Score: 1.0},
		{FilePath: "mixed.go", LineNumber: 1, Line: "// alpha here", Score: 1.0},
		{FilePath: "mixed.go", LineNumber: 2, Line: "// beta there", Score: 1.0},
	}
	out := RerankByProximity(in, []string{"alpha", "beta"})
	if out[len(out)-1].FilePath != "only-alpha.go" {
		t.Fatalf("partial-coverage file should rank last, got %v", paths(out))
	}
}

// TestCorrectTermsUnambiguous pins the happy path: a misspelled term with
// exactly one lexicon neighbor within budget is corrected.
func TestCorrectTermsUnambiguous(t *testing.T) {
	lexicon := []string{"SearchLexicon", "Store", "FileMatch"}
	corrected, applied := CorrectTerms(lexicon, []string{"SrchLexicon"}, 2)
	if corrected[0] != "searchlexicon" {
		t.Fatalf("expected correction to searchlexicon, got %q", corrected[0])
	}
	if len(applied) != 1 || applied[0].Original != "SrchLexicon" || applied[0].Corrected != "searchlexicon" {
		t.Fatalf("unexpected applied corrections: %+v", applied)
	}
}

// TestCorrectTermsAmbiguousNotCorrected pins the unambiguous-only policy:
// two equidistant candidates leave the term untouched.
func TestCorrectTermsAmbiguousNotCorrected(t *testing.T) {
	lexicon := []string{"alphaa", "alphab"}
	corrected, applied := CorrectTerms(lexicon, []string{"alpha"}, 1)
	if corrected[0] != "alpha" {
		t.Fatalf("ambiguous term must stay as-is, got %q", corrected[0])
	}
	if len(applied) != 0 {
		t.Fatalf("ambiguous term must not record a correction: %+v", applied)
	}
}

// TestCorrectTermsPolicies covers the remaining guard rails: known terms,
// short terms, over-budget distances, and punctuation-only terms.
func TestCorrectTermsPolicies(t *testing.T) {
	lexicon := []string{"searchlexicon", "store"}
	cases := []struct {
		name string
		term string
		want string
	}{
		{"already correct", "SearchLexicon", "SearchLexicon"},
		{"short term never corrected", "ab", "ab"},
		{"distance over budget", "searchlexiconxyz", "searchlexiconxyz"},
		{"punctuation-only", "--", "--"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			corrected, applied := CorrectTerms(lexicon, []string{tc.term}, 2)
			if corrected[0] != tc.want {
				t.Fatalf("CorrectTerms(%q) = %q, want %q", tc.term, corrected[0], tc.want)
			}
			if len(applied) != 0 {
				t.Fatalf("unexpected corrections: %+v", applied)
			}
		})
	}
}

// TestCorrectTermsOrderPreserved: uncorrected terms keep their positions.
func TestCorrectTermsOrderPreserved(t *testing.T) {
	lexicon := []string{"searchlexicon"}
	corrected, _ := CorrectTerms(lexicon, []string{"keep", "SrchLexicon", "Store"}, 2)
	want := []string{"keep", "searchlexicon", "Store"}
	if !reflect.DeepEqual(corrected, want) {
		t.Fatalf("corrected = %v, want %v", corrected, want)
	}
}

// TestMinTermWindowUnit covers the window computation directly, including
// the partial-coverage zero.
func TestMinTermWindowUnit(t *testing.T) {
	terms := []string{"a", "b"}
	cases := []struct {
		name  string
		byLoc map[string]map[int]bool
		want  int
	}{
		{"same line", map[string]map[int]bool{"a": {5: true}, "b": {5: true}}, 1},
		{"adjacent lines", map[string]map[int]bool{"a": {3: true}, "b": {4: true}}, 2},
		{"nested best", map[string]map[int]bool{"a": {2: true, 10: true}, "b": {4: true, 12: true}}, 3},
		{"missing term", map[string]map[int]bool{"a": {2: true}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := minTermWindow(tc.byLoc, terms); got != tc.want {
				t.Fatalf("minTermWindow = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestWordTerms pins term normalization shared by proximity and correction.
func TestWordTerms(t *testing.T) {
	got := WordTerms([]string{"Alpha", "  ", "-", "beta x"})
	want := []string{"alpha", "beta x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WordTerms = %v, want %v", got, want)
	}
}
