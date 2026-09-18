package render

import (
	"strings"
	"testing"

	"codemap/query"
)

// TestRenderSearchMarksGenerated verifies that a generated search result is
// distinguishable in every output format.
func TestRenderSearchMarksGenerated(t *testing.T) {
	results := []query.SearchResult{
		{QualifiedName: "pkg.Hand", Kind: "function", Exported: true},
		{QualifiedName: "pkg.Auto", Kind: "function", Exported: true, Generated: true},
	}

	jsonOut := RenderSearch(results, WithFormat(FormatJSON))
	if !strings.Contains(jsonOut, `"Generated": true`) {
		t.Errorf("JSON must carry the generated flag, got %s", jsonOut)
	}

	toonOut := RenderSearch(results, WithFormat(FormatTOON))
	if !strings.Contains(toonOut, "Generated") {
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
			Path:            "pkg",
			Name:            "pkg",
			ExportedSymbols: []query.SymbolDetail{{QualifiedName: "pkg.Auto", Kind: "function", Exported: true, Generated: true}},
		}},
		TotalPackages: 1,
		TotalSymbols:  1,
	}

	out := RenderOverview(result, WithFormat(FormatJSON))
	if !strings.Contains(out, `"generated": true`) {
		t.Errorf("overview JSON must carry the generated flag, got %s", out)
	}
}

func TestRenderPackageMarksGenerated(t *testing.T) {
	pkg := &query.PackageResult{
		Path:            "pkg",
		Name:            "pkg",
		ExportedSymbols: []query.SymbolDetail{{QualifiedName: "pkg.Auto", Kind: "function", Exported: true, Generated: true}},
	}

	out := RenderPackage(pkg, WithFormat(FormatText))
	if !strings.Contains(out, "pkg.Auto (function) [generated]") {
		t.Errorf("package text output must mark the generated symbol, got %s", out)
	}
}
