package query_test

import (
	"path/filepath"
	"slices"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

const (
	handSymbol     = "example.com/generated.Zulu"
	handHelper     = "example.com/generated.helperHand"
	genSymbol      = "example.com/generated.AlphaAuto"
	genHelper      = "example.com/generated.helperAuto"
	testSymbol     = "example.com/generated_test.AardvarkTest"
	generatedFile  = "zz_generated.go"
	generatedChurn = 50
)

// setupStore indexes the named fixture under testdata/fixtures. Its hand-written
// and generated symbols share match tier, exported state, and kind, and the
// generated file carries the fixture's highest churn, so ranking and analysis
// can only be explained by provenance.
func setupProvenanceStore(t *testing.T, fixture string) *store.Store {
	t.Helper()
	absPath, err := filepath.Abs(filepath.Join("../testdata/fixtures", fixture))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	churn := make(map[string]int, len(pr.Files))
	for path := range pr.Files {
		churn[path] = 1
		if filepath.Base(path) == generatedFile {
			churn[path] = generatedChurn
		}
	}

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, churn); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

// assertNoGenerated fails when any generated fixture symbol appears in names.
func assertNoGenerated(t *testing.T, label string, names []string) {
	t.Helper()
	for _, name := range names {
		if name == genSymbol || name == genHelper {
			t.Errorf("%s must exclude generated symbol %s", label, name)
		}
	}
}

func hotspotNames(hotspots []query.Hotspot) []string {
	names := make([]string, len(hotspots))
	for i, h := range hotspots {
		names[i] = h.QualifiedName
	}
	return names
}

func unusedNames(unused []query.UnusedSymbol) []string {
	names := make([]string, len(unused))
	for i, u := range unused {
		names[i] = u.QualifiedName
	}
	return names
}

func importanceNames(entries []query.ImportanceEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.QualifiedName
	}
	return names
}

// generatedPattern matches every fixture symbol at the same (prefix) tier.
const generatedPattern = "example.com/generated"

func TestSearchRanksHandWrittenBeforeGenerated(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	results, err := query.Search(s, generatedPattern)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.QualifiedName
	}
	// AlphaAuto sorts first by name; ranking must push it, and every other
	// generated or test symbol, behind the hand-written one without dropping it.
	want := []string{handSymbol, genSymbol, handHelper, genHelper}
	if !slices.Equal(names, want) {
		t.Fatalf("ranking = %v, want %v", names, want)
	}
	if results[0].Generated {
		t.Error("Zulu must not be marked generated")
	}
	if !results[1].Generated {
		t.Error("AlphaAuto must carry Generated=true")
	}
}

// TestSearchTestRankingDoesNotChangeInclusion verifies the spec's test-vs-non-test
// ordering rule: test symbols stay excluded by default, and when they are
// included they are present and rank after a non-test symbol of the same tier,
// exported state, and kind.
func TestSearchTestRankingDoesNotChangeInclusion(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	excluded, err := query.Search(s, generatedPattern)
	if err != nil {
		t.Fatal(err)
	}
	excludedNames := make([]string, len(excluded))
	for i, r := range excluded {
		excludedNames[i] = r.QualifiedName
	}
	if slices.Contains(excludedNames, testSymbol) {
		t.Fatalf("test symbol must stay excluded by default, got %v", excludedNames)
	}

	included, err := query.Search(s, generatedPattern, query.WithTests())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(included))
	for i, r := range included {
		names[i] = r.QualifiedName
	}
	// The test symbol is present (never dropped) and ranked after every
	// non-test symbol: exported symbols first, then the test symbol, then the
	// unexported helpers.
	want := []string{handSymbol, genSymbol, testSymbol, handHelper, genHelper}
	if !slices.Equal(names, want) {
		t.Fatalf("ranking with tests included = %v, want %v", names, want)
	}
}

func TestSearchGeneratedOnlyMatchIsReturned(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	results, err := query.Search(s, "AlphaAuto")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].QualifiedName != genSymbol || !results[0].Generated {
		t.Fatalf("a generated-only match must be returned with Generated=true, got %+v", results)
	}
}

func TestSearchGeneratedFilter(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	excluded, err := query.Search(s, generatedPattern, query.WithGenerated(store.GeneratedExclude))
	if err != nil {
		t.Fatal(err)
	}
	excludedNames := make([]string, len(excluded))
	for i, r := range excluded {
		excludedNames[i] = r.QualifiedName
	}
	if !slices.Equal(excludedNames, []string{handSymbol, handHelper}) {
		t.Fatalf("exclude filter = %v, want the hand-written symbols", excludedNames)
	}

	only, err := query.Search(s, generatedPattern, query.WithGenerated(store.GeneratedOnly))
	if err != nil {
		t.Fatal(err)
	}
	onlyNames := make([]string, len(only))
	for i, r := range only {
		onlyNames[i] = r.QualifiedName
	}
	if !slices.Equal(onlyNames, []string{genSymbol, genHelper}) {
		t.Fatalf("only filter = %v, want the generated symbols", onlyNames)
	}

	unfiltered, err := query.Search(s, generatedPattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != 4 {
		t.Fatalf("default search must not filter, got %+v", unfiltered)
	}
}

// TestSearchGeneratedFilterAppliesToPrefixMode verifies the prefix mode, which
// does not push the filter into SQL, still honors it.
func TestSearchGeneratedFilterAppliesToPrefixMode(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	only, err := query.SearchByPrefix(s, "example.com/generated.A", query.WithGenerated(store.GeneratedOnly))
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].QualifiedName != genSymbol {
		t.Fatalf("prefix mode must honor the generated filter, got %+v", only)
	}

	excluded, err := query.SearchByPrefix(s, "example.com/generated", query.WithGenerated(store.GeneratedExclude))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range excluded {
		if r.Generated {
			t.Errorf("prefix mode must honor the exclude filter, got generated %s", r.QualifiedName)
		}
	}
}

// TestSearchByPrefixRanksGeneratedLast verifies prefix mode applies the same
// provenance ranking as substring mode: the exported hand-written symbol ranks
// first and generated symbols rank last, without being dropped. The store's own
// order is by qualified name, so this can only pass if ranking is applied.
func TestSearchByPrefixRanksGeneratedLast(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	results, err := query.SearchByPrefix(s, generatedPattern)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.QualifiedName
	}
	want := []string{handSymbol, genSymbol, handHelper, genHelper}
	if !slices.Equal(names, want) {
		t.Fatalf("prefix ranking = %v, want %v", names, want)
	}
}

// TestHotspotsExcludeGenerated verifies that the highest-scoring generated file
// cannot top hotspots and that hand-written symbols still rank.
func TestHotspotsExcludeGenerated(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	hotspots, err := query.Hotspots(s, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := hotspotNames(hotspots)
	assertNoGenerated(t, "hotspots", names)
	if len(names) == 0 || names[0] != handSymbol {
		t.Fatalf("expected the hand-written symbol to top hotspots, got %v", names)
	}
}

func TestSymbolImportanceExcludesGenerated(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	entries, err := query.SymbolImportance(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := importanceNames(entries)
	assertNoGenerated(t, "importance", names)
	if len(names) == 0 {
		t.Fatal("expected hand-written symbols to still rank")
	}
}

func TestUnusedExcludesGenerated(t *testing.T) {
	s := setupProvenanceStore(t, "generated")

	unused, err := query.UnusedSymbols(s)
	if err != nil {
		t.Fatal(err)
	}
	names := unusedNames(unused)
	assertNoGenerated(t, "unused", names)
	if !slices.Contains(names, handHelper) {
		t.Fatalf("expected the hand-written helper in unused, got %v", names)
	}
}

// TestMethodSearchRanksGeneratedLast verifies method mode applies the same
// provenance ranking as substring and prefix modes: the hand-written method
// ranks before the generated one. The store returns methods in qualified-name
// order (AutoHandler before Server), so this only passes if ranking is applied.
func TestMethodSearchRanksGeneratedLast(t *testing.T) {
	s := setupProvenanceStore(t, "generated-methods")

	results, err := query.MethodSearch(s, "Handle")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.QualifiedName
	}
	want := []string{
		"example.com/genmethods.Server.Handle",
		"example.com/genmethods.AutoHandler.Handle",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("method ranking = %v, want %v", names, want)
	}
}
