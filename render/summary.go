package render

import (
	"fmt"
	"strings"

	"codemap/opt"
	"codemap/query"
)

// RenderSummary renders the repository tl;dr. Every format stays small: the
// summary exists to orient an agent, not to enumerate symbols.
func RenderSummary(result *query.SummaryResult, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatTOON:
		return renderSummaryTOON(result)
	case FormatJSON:
		return marshalJSON(result)
	case FormatCompact:
		return renderSummaryCompact(result)
	case FormatText:
		return renderSummaryText(result)
	default:
		return renderSummaryTOON(result)
	}
}

func renderSummaryTOON(result *query.SummaryResult) string {
	data := map[string]any{
		"module":      result.Module,
		"description": result.Description,
		"indexed_via": result.IndexedVia,
		"git_head":    result.GitHead,
		"totals": map[string]any{
			"packages": len(result.Packages),
			keySymbols: result.TotalSymbols,
			keyEdges:   result.TotalEdges,
		},
		"packages":     result.Packages,
		"entry_points": result.EntryPoints,
		"third_party":  result.ThirdParty,
		"stdlib":       result.Stdlib,
		"hubs":         result.Hubs,
	}
	return encodeTOON(data)
}

func renderSummaryText(result *query.SummaryResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s | %d packages, %d symbols, %d edges | %s @ %s\n",
		result.Module, len(result.Packages), result.TotalSymbols, result.TotalEdges, result.IndexedVia, shortHead(result.GitHead))
	if result.Description != "" {
		fmt.Fprintf(&b, "%s\n", result.Description)
	}

	b.WriteString("\npackages (largest first):\n")
	for _, p := range result.Packages {
		doc := p.Doc
		if doc == "" {
			doc = "-"
		}
		fmt.Fprintf(&b, "  %-24s %4d sym  %s\n", p.Path, p.SymCount, doc)
	}

	if len(result.EntryPoints) > 0 {
		b.WriteString("\nentry points:\n")
		for _, e := range result.EntryPoints {
			fmt.Fprintf(&b, "  %s (%s:%d)\n", e.QualifiedName, e.PosFile, e.PosLine)
		}
	}
	if len(result.ThirdParty) > 0 {
		fmt.Fprintf(&b, "\nthird-party: %s\n", strings.Join(result.ThirdParty, ", "))
	}
	if len(result.Stdlib) > 0 {
		fmt.Fprintf(&b, "stdlib: %s\n", strings.Join(result.Stdlib, ", "))
	}
	if len(result.Hubs) > 0 {
		parts := make([]string, 0, len(result.Hubs))
		for _, h := range result.Hubs {
			parts = append(parts, fmt.Sprintf("%s (fan-in %d)", h.Package, h.FanIn))
		}
		fmt.Fprintf(&b, "hubs: %s\n", strings.Join(parts, ", "))
	}
	return b.String()
}

func renderSummaryCompact(result *query.SummaryResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d packages, %d symbols, %d edges\n",
		result.Module, len(result.Packages), result.TotalSymbols, result.TotalEdges)
	for _, p := range result.Packages {
		fmt.Fprintf(&b, "%s: %d sym", p.Path, p.SymCount)
		if p.Doc != "" {
			fmt.Fprintf(&b, " - %s", p.Doc)
		}
		b.WriteString("\n")
	}
	if len(result.EntryPoints) > 0 {
		names := make([]string, 0, len(result.EntryPoints))
		for _, e := range result.EntryPoints {
			names = append(names, e.QualifiedName)
		}
		fmt.Fprintf(&b, "entry: %s\n", strings.Join(names, ", "))
	}
	if len(result.ThirdParty) > 0 {
		fmt.Fprintf(&b, "third_party: %s\n", strings.Join(result.ThirdParty, ", "))
	}
	if len(result.Stdlib) > 0 {
		fmt.Fprintf(&b, "stdlib: %s\n", strings.Join(result.Stdlib, ", "))
	}
	if len(result.Hubs) > 0 {
		parts := make([]string, 0, len(result.Hubs))
		for _, h := range result.Hubs {
			parts = append(parts, fmt.Sprintf("%s(%d)", h.Package, h.FanIn))
		}
		fmt.Fprintf(&b, "hubs: %s\n", strings.Join(parts, ", "))
	}
	return b.String()
}

func shortHead(head string) string {
	if len(head) > 8 {
		return head[:8]
	}
	return head
}
