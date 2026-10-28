package render

import (
	"strings"
	"testing"

	"codemap/query"
	"codemap/store"
)

// TestRenderSearchMarksGenerated verifies that a generated search result is
// distinguishable in every output format.
func TestRenderSearchMarksGenerated(t *testing.T) {
	results := []query.SearchResult{
		{QualifiedName: "pkg.Hand", Kind: "function", Exported: true},
		{QualifiedName: "pkg.Auto", Kind: "function", Exported: true, Generated: true},
	}

	jsonOut := RenderSearch(results, WithFormat(FormatJSON))
	if !strings.Contains(jsonOut, `"generated": true`) {
		t.Errorf("JSON must carry the generated flag, got %s", jsonOut)
	}

	toonOut := RenderSearch(results, WithFormat(FormatTOON))
	if !strings.Contains(toonOut, "generated") {
		t.Errorf("TOON must carry the generated column, got %s", toonOut)
	}

	for _, tc := range []struct {
		format Format
		want   string
	}{
		{FormatText, "pkg.Auto function [generated]"},
		{FormatCompact, "function pkg.Auto [generated]"},
	} {
		out := RenderSearch(results, WithFormat(tc.format))
		if !strings.Contains(out, tc.want) {
			t.Errorf("format %v must mark the generated symbol, got %s", tc.format, out)
		}
		if got := strings.Count(out, "[generated]"); got != 1 {
			t.Errorf("format %v must mark only the generated symbol, got %d markers in %s", tc.format, got, out)
		}
	}
}

func TestRenderOverviewMarksGenerated(t *testing.T) {
	result := &query.OverviewResult{
		Packages: []query.PackageSummary{{
			Path:    "pkg",
			Name:    "pkg",
			Symbols: []query.SymbolDetail{{QualifiedName: "pkg.Auto", Kind: "function", Exported: true, Generated: true}},
		}},
		TotalPackages: 1,
		TotalSymbols:  1,
	}

	out := RenderOverview(result, WithFormat(FormatJSON))
	if !strings.Contains(out, `"generated": true`) {
		t.Errorf("overview JSON must carry the generated flag, got %s", out)
	}
}

// TestRenderOverviewJSONCarriesPackageDoc pins that the JSON renderer keeps the
// package doc the query layer now fills in; it builds its own row type, so the
// field has to be copied explicitly.
func TestRenderOverviewJSONCarriesPackageDoc(t *testing.T) {
	result := &query.OverviewResult{
		Packages: []query.PackageSummary{{
			Path: "example.com/doc",
			Name: "doc",
			Doc:  "Package doc explains the package.",
		}},
		TotalPackages: 1,
	}

	out := RenderOverview(result, WithFormat(FormatJSON))
	if !strings.Contains(out, `"doc": "Package doc explains the package."`) {
		t.Errorf("overview JSON must carry the package doc, got %s", out)
	}
}

// TestRenderChangedSymbolsStatesCleanBuild pins that a clean build reads as a
// positive result: the key is absent from the structured output and the text
// renderer says so, rather than leaving silence.
func TestRenderChangedSymbolsStatesCleanBuild(t *testing.T) {
	result := &query.ChangedSymbolsResult{}

	jsonOut := RenderChangedSymbols(result, WithFormat(FormatJSON))
	if strings.Contains(jsonOut, "compile_errors") {
		t.Errorf("clean build must omit compile_errors from JSON, got %s", jsonOut)
	}
	toonOut := RenderChangedSymbols(result, WithFormat(FormatTOON))
	if strings.Contains(toonOut, "compile_errors") {
		t.Errorf("clean build must omit compile_errors from TOON, got %s", toonOut)
	}
	textOut := RenderChangedSymbols(result, WithFormat(FormatText))
	if !strings.Contains(textOut, "compile errors: none") {
		t.Errorf("text must state the clean build, got %s", textOut)
	}
}

func TestRenderPackageMarksGenerated(t *testing.T) {
	pkg := &query.PackageResult{
		Path:    "pkg",
		Name:    "pkg",
		Symbols: []query.SymbolDetail{{QualifiedName: "pkg.Auto", Kind: "function", Exported: true, Generated: true}},
	}

	out := RenderPackage(pkg, WithFormat(FormatText))
	if !strings.Contains(out, "pkg.Auto (function) [generated]") {
		t.Errorf("package text output must mark the generated symbol, got %s", out)
	}
}

// TestRenderTextMatchesSignals pins that compact output stays exactly
// path:line while the text header names the matching strategies.
func TestRenderTextMatchesSignals(t *testing.T) {
	matches := []store.FileMatch{
		{FilePath: "a.go", LineNumber: 7, Line: "hit", Signals: []string{"fts", "substring"}},
		{FilePath: "b.go", LineNumber: 9, Line: "plain"},
	}

	if got := RenderTextMatches(matches, WithFormat(FormatCompact)); got != "a.go:7\nb.go:9\n" {
		t.Fatalf("compact output changed: %q", got)
	}

	text := RenderTextMatches(matches, WithFormat(FormatText))
	if !strings.Contains(text, "a.go:7 [fts,substring]\n") {
		t.Fatalf("text header missing signals: %q", text)
	}
	if !strings.Contains(text, "b.go:9\n") {
		t.Fatalf("text header for signal-free row changed: %q", text)
	}
}
