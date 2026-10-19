package render

import (
	"strings"
	"testing"

	"codemap/query"
)

func summaryFixture() *query.SummaryResult {
	return &query.SummaryResult{
		Module:       "example.com/app",
		IndexedVia:   "go-list",
		GitHead:      "abcdef1234567890",
		TotalSymbols: 42,
		TotalEdges:   99,
		Packages: []query.SummaryPackage{
			{Path: "example.com/app/store", Doc: "SQLite persistence.", SymCount: 30},
			{Path: "example.com/app/api", SymCount: 12},
		},
		EntryPoints: []query.EntryPoint{
			{QualifiedName: "example.com/app/main.main", PosFile: "main.go", PosLine: 10},
		},
		ThirdParty: []string{"modernc.org/sqlite"},
		Stdlib:     []string{"fmt", "os"},
		Hubs:       []query.Hub{{Package: "example.com/app/store", FanIn: 4}},
	}
}

func TestRenderSummaryFormats(t *testing.T) {
	result := summaryFixture()

	for _, tc := range []struct {
		format Format
		want   []string
	}{
		{FormatTOON, []string{"example.com/app", "SQLite persistence.", "modernc.org/sqlite"}},
		{FormatJSON, []string{`"Module": "example.com/app"`, `"doc": "SQLite persistence."`}},
		{FormatCompact, []string{"example.com/app: 2 packages", "store: 30 sym", "third_party: modernc.org/sqlite", "hubs: example.com/app/store(4)"}},
		{FormatText, []string{"example.com/app | 2 packages", "SQLite persistence.", "entry points:"}},
	} {
		out := RenderSummary(result, WithFormat(tc.format))
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("format %v missing %q in:\n%s", tc.format, want, out)
			}
		}
	}
}

func TestRenderSummaryStaysSmall(t *testing.T) {
	out := RenderSummary(summaryFixture(), WithFormat(FormatTOON))
	if len(out) > 2048 {
		t.Errorf("summary must stay small, got %d bytes", len(out))
	}
}
