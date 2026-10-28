package render

import (
	"strings"
	"testing"

	"codemap/query"
)

// TestRenderSearchFileScoped pins the symbols_in_file projection: no pos_file,
// first-sentence docs by default, and a smaller payload than the unprojected
// render.
func TestRenderSearchFileScoped(t *testing.T) {
	results := []query.SearchResult{
		{
			QualifiedName: "pkg.One",
			Kind:          "function",
			Signature:     "func One()",
			Doc:           "One does things.\n\nIt also has a long tail that must not survive the default render.",
			PosFile:       "/repo/pkg/file.go",
			PosLine:       3,
			Exported:      true,
		},
		{
			QualifiedName: "pkg.Two",
			Kind:          "function",
			Signature:     "func Two()",
			Doc:           "Two does things.\n\nAnother long tail.",
			PosFile:       "/repo/pkg/file.go",
			PosLine:       7,
			Exported:      true,
		},
	}
	projected := RenderSearch(results, WithFormat(FormatTOON), WithFileScoped())
	plain := RenderSearch(results, WithFormat(FormatTOON))

	if strings.Contains(projected, "pos_file") {
		t.Errorf("projected output carries pos_file:\n%s", projected)
	}
	if !strings.Contains(projected, "One does things.") {
		t.Errorf("projected output lost the first sentence:\n%s", projected)
	}
	if strings.Contains(projected, "long tail") {
		t.Errorf("projected output kept the full doc:\n%s", projected)
	}
	if len(projected) >= len(plain) {
		t.Errorf("projected payload %d bytes not smaller than unprojected %d", len(projected), len(plain))
	}
}

// TestRenderSearchTOONFullDocs pins that the TOON renderer honours full_docs,
// which it previously ignored while JSON and text truncated.
func TestRenderSearchTOONFullDocs(t *testing.T) {
	results := []query.SearchResult{{
		QualifiedName: "pkg.One",
		Kind:          "function",
		Signature:     "func One()",
		Doc:           "One does things.\n\nIt also has a long tail.",
		PosFile:       "/repo/pkg/file.go",
		PosLine:       3,
	}}
	full := RenderSearch(results, WithFormat(FormatTOON), WithFullDocs())
	if !strings.Contains(full, "It also has a long tail.") {
		t.Errorf("full_docs lost the complete doc:\n%s", full)
	}
}

// TestRenderSearchFileScopedJSON pins that the projection holds for the JSON
// renderer too, which otherwise returns the full SearchResult unchanged.
func TestRenderSearchFileScopedJSON(t *testing.T) {
	results := []query.SearchResult{{
		QualifiedName: "pkg.One",
		Kind:          "function",
		Signature:     "func One()",
		Doc:           "One does things.\n\nA tail that must not survive.",
		PosFile:       "/repo/pkg/file.go",
		PosLine:       3,
		Exported:      true,
	}}
	out := RenderSearch(results, WithFormat(FormatJSON), WithFileScoped())
	if strings.Contains(out, "pos_file") {
		t.Errorf("projected JSON carries pos_file:\n%s", out)
	}
	if strings.Contains(out, "A tail") {
		t.Errorf("projected JSON kept the full doc:\n%s", out)
	}
	if !strings.Contains(out, "One does things.") {
		t.Errorf("projected JSON lost the first sentence:\n%s", out)
	}
}
