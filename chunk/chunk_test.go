package chunk

import (
	"strconv"
	"strings"
	"testing"
)

func concat(sections []Section) string {
	var b strings.Builder
	for _, sec := range sections {
		b.WriteString(sec.Content)
	}
	return b.String()
}

// overviewTOON mirrors render.RenderOverview's TOON shape: column-0 top-level
// keys, indented members, keys sorted alphabetically. The packages section is
// padded above MinChunk so it survives the small-section merge; the trailing
// summary block stays below MinChunk and keeps its own section because a last
// section has no neighbor to merge into.
const overviewTOON = `packages[2]:
  - ExportedSymbols[8]:
    - QualifiedName: a.NewParser
      Kind: function
    - QualifiedName: a.Parser
      Kind: type
    - QualifiedName: a.Parser.Parse
      Kind: method
    - QualifiedName: a.LoadConfig
      Kind: function
    - QualifiedName: a.SaveState
      Kind: function
    - QualifiedName: a.ResetCache
      Kind: function
    - QualifiedName: a.MarshalOutput
      Kind: function
    - QualifiedName: a.UnmarshalInput
      Kind: function
    ImportCount: 0
    Name: a
    Path: codemap/a
  - ExportedSymbols[1]:
    - QualifiedName: b.Run
      Kind: function
    ImportCount: 1
    Name: b
    Path: codemap/b
summary:
  edges: 4
  packages: 2
  symbols: 9
`

func TestRenderedSectionsTOONOverview(t *testing.T) {
	sections := RenderedSections(overviewTOON)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections (packages, summary), got %d: %+v", len(sections), sections)
	}
	if sections[0].Title != "packages[2]:" {
		t.Errorf("section 0 title = %q, want %q", sections[0].Title, "packages[2]:")
	}
	if sections[1].Title != "summary:" {
		t.Errorf("section 1 title = %q, want %q", sections[1].Title, "summary:")
	}
	if got := concat(sections); got != overviewTOON {
		t.Errorf("concatenation is not lossless:\n got: %q\nwant: %q", got, overviewTOON)
	}
}

func TestRenderedSectionsJSONObject(t *testing.T) {
	pkgs := make([]string, 0, 40)
	for i := range 40 {
		pkgs = append(pkgs, `    {"path": "codemap/pkg`+strings.Repeat("x", 10)+strconv.Itoa(i)+`", "name": "pkg`+strconv.Itoa(i)+`", "exported_symbols": [{"qualified_name": "codemap/pkg`+strconv.Itoa(i)+`.Thing`+strconv.Itoa(i)+`", "kind": "function"}]}`)
	}
	payload := `{
  "packages": [
` +
		strings.Join(pkgs, ",\n") + `
  ],
  "summary": {
    "total_packages": 40,
    "brace_in_string": "not } a boundary",
    "quote_in_string": "say \"hi\""
  }
}
`
	sections := RenderedSections(payload)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections (packages, summary), got %d: %+v", len(sections), sections)
	}
	if sections[0].Title != "packages" || sections[1].Title != "summary" {
		t.Errorf("titles = %q, %q; want packages, summary", sections[0].Title, sections[1].Title)
	}
	if got := concat(sections); got != payload {
		t.Errorf("concatenation is not lossless:\n got: %q\nwant: %q", got, payload)
	}
}

func TestRenderedSectionsSingleBlock(t *testing.T) {
	payload := "Bundle: a.Parser (est. 42 tokens)\n\nbody of the bundle\n"
	sections := RenderedSections(payload)
	if len(sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(sections))
	}
	if sections[0].Content != payload {
		t.Errorf("section content = %q, want the whole payload", sections[0].Content)
	}
}

func TestRenderedSectionsNoNewlines(t *testing.T) {
	payload := strings.Repeat("x", MaxChunk+100)
	sections := RenderedSections(payload)
	if len(sections) != 1 {
		t.Fatalf("expected 1 oversized fallback section for an unbreakable payload, got %d", len(sections))
	}
	if len(sections[0].Content) != len(payload) {
		t.Errorf("section length = %d, want %d", len(sections[0].Content), len(payload))
	}
}

func TestRenderedSectionsFixedFallbackLossless(t *testing.T) {
	var b strings.Builder
	for range 4000 {
		b.WriteString("line of output with an identifier like grepTarget1234567\n")
	}
	payload := b.String()
	if len(payload) <= MaxChunk {
		t.Fatalf("test payload must exceed MaxChunk, is %d", len(payload))
	}
	sections := RenderedSections(payload)
	if len(sections) < 2 {
		t.Fatalf("expected multiple fixed chunks, got %d", len(sections))
	}
	for i, sec := range sections {
		if len(sec.Content) > MaxChunk {
			t.Errorf("section %d is %d bytes, exceeds MaxChunk %d", i, len(sec.Content), MaxChunk)
		}
	}
	if got := concat(sections); got != payload {
		t.Errorf("concatenation is not lossless")
	}
}

func TestRenderedSectionsStructuralSplitWinsWhenWithinMax(t *testing.T) {
	var b strings.Builder
	b.WriteString("packages[3]:\n")
	for range 3 {
		b.WriteString(strings.Repeat("  - symbol entry data line\n", 80))
	}
	b.WriteString("summary:\n  packages: 3\n  symbols: 240\n")
	payload := b.String()
	sections := RenderedSections(payload)
	if len(sections) != 2 {
		t.Fatalf("expected structural split into 2 sections, got %d", len(sections))
	}
	for i, sec := range sections {
		if len(sec.Content) > MaxChunk {
			t.Fatalf("section %d is %d bytes, exceeds MaxChunk", i, len(sec.Content))
		}
	}
	if got := concat(sections); got != payload {
		t.Errorf("concatenation is not lossless")
	}
}

func TestRenderedSectionsEmptyInput(t *testing.T) {
	if got := RenderedSections(""); got != nil {
		t.Errorf("RenderedSections(\"\") = %+v, want nil", got)
	}
}

func TestRenderedSectionsMergesSmallSections(t *testing.T) {
	var b strings.Builder
	b.WriteString("summary:\n  packages: 1\n")
	for range 300 {
		b.WriteString("packages[1]:\n  - data line to give the section some bulk for the merge check\n")
	}
	payload := b.String()
	sections := RenderedSections(payload)
	for i, sec := range sections {
		if len(sec.Content) < MinChunk && i < len(sections)-1 {
			t.Errorf("section %d (%d bytes) below MinChunk %d but not merged", i, len(sec.Content), MinChunk)
		}
	}
	if got := concat(sections); got != payload {
		t.Errorf("concatenation is not lossless")
	}
}
