package render

import (
	"strings"
	"testing"

	"codemap/query"
)

func TestRenderPatternMatchesFormats(t *testing.T) {
	matches := []query.PatternMatch{
		{File: "a.go", Line: 8, Text: "defer cleanup(ctx)", Symbol: "pkg.Handle"},
		{File: "a.go", Line: 12, Text: "import \"fmt\"", Symbol: ""},
	}

	jsonOut := RenderPatternMatches(matches, WithFormat(FormatJSON))
	for _, want := range []string{`"file": "a.go"`, `"line": 8`, `"text": "defer cleanup(ctx)"`, `"symbol": "pkg.Handle"`} {
		if !strings.Contains(jsonOut, want) {
			t.Errorf("JSON output missing %s, got:\n%s", want, jsonOut)
		}
	}

	toonOut := RenderPatternMatches(matches, WithFormat(FormatTOON))
	if !strings.Contains(toonOut, "a.go:8 [pkg.Handle]") {
		t.Errorf("TOON output must label the match with its enclosing symbol, got:\n%s", toonOut)
	}
	if !strings.Contains(toonOut, "defer cleanup(ctx)") {
		t.Errorf("TOON output must include the matched source, got:\n%s", toonOut)
	}
	if strings.Contains(toonOut, "a.go:12 [") {
		t.Errorf("a match outside any symbol must not be tagged, got:\n%s", toonOut)
	}

	compact := RenderPatternMatches(matches, WithFormat(FormatCompact))
	want := "a.go:8 [pkg.Handle] defer cleanup(ctx)\na.go:12 import \"fmt\"\n"
	if compact != want {
		t.Errorf("compact output = %q, want %q", compact, want)
	}
}

func TestRenderPatternMatchesCollapsesMultilineCompact(t *testing.T) {
	matches := []query.PatternMatch{
		{File: "a.go", Line: 3, Text: "if err != nil {\n\treturn err\n}", Symbol: "pkg.Handle"},
	}
	compact := RenderPatternMatches(matches, WithFormat(FormatCompact))
	want := "a.go:3 [pkg.Handle] if err != nil { return err }\n"
	if compact != want {
		t.Errorf("compact output = %q, want %q", compact, want)
	}
}
