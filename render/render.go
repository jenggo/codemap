package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"codemap/query"
	"codemap/store"

	"github.com/alpkeskin/gotoon"
)

type Format int

const (
	FormatTOON Format = iota
	FormatText
	FormatJSON
	FormatCompact
)

type Options struct {
	Format   Format
	FullDocs bool
}

type Option func(*Options)

func WithFormat(f Format) Option {
	return func(o *Options) {
		o.Format = f
	}
}

func WithFullDocs() Option {
	return func(o *Options) {
		o.FullDocs = true
	}
}

func RenderOverview(result *query.OverviewResult, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderOverviewTOON(result)
	case FormatJSON:
		return renderOverviewJSON(result, options.FullDocs)
	case FormatCompact:
		return renderOverviewCompact(result)
	case FormatText:
		return renderOverviewText(result)
	default:
		return renderOverviewTOON(result)
	}
}

type symJSON struct {
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Signature     string `json:"signature"`
	Receiver      string `json:"receiver"`
	PosFile       string `json:"pos_file"`
	Doc           string `json:"doc"`
	PosLine       int    `json:"pos_line"`
	Exported      bool   `json:"exported"`
}

func renderOverviewJSON(result *query.OverviewResult, fullDocs bool) string {
	type pkgJSON struct {
		Path            string    `json:"path"`
		Name            string    `json:"name"`
		ExportedSymbols []symJSON `json:"exported_symbols"`
		ImportCount     int       `json:"import_count"`
	}

	type overviewJSON struct {
		Packages []pkgJSON `json:"packages"`
		Summary  struct {
			TotalPackages int `json:"total_packages"`
			TotalSymbols  int `json:"total_symbols"`
			TotalEdges    int `json:"total_edges"`
		} `json:"summary"`
	}

	pkgs := make([]pkgJSON, len(result.Packages))
	for i, s := range result.Packages {
		syms := make([]symJSON, len(s.ExportedSymbols))
		for j, sym := range s.ExportedSymbols {
			syms[j] = symToJSON(sym, fullDocs)
		}
		pkgs[i] = pkgJSON{
			Path:            s.Path,
			Name:            s.Name,
			ExportedSymbols: syms,
			ImportCount:     s.ImportCount,
		}
	}

	out := overviewJSON{}
	out.Summary.TotalPackages = result.TotalPackages
	out.Summary.TotalSymbols = result.TotalSymbols
	out.Summary.TotalEdges = result.TotalEdges
	out.Packages = pkgs

	return marshalJSON(out)
}

func renderOverviewText(result *query.OverviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summary: %d packages, %d symbols, %d edges\n\n", result.TotalPackages, result.TotalSymbols, result.TotalEdges)
	for _, s := range result.Packages {
		fmt.Fprintf(&b, "%s (%s)\n", s.Path, s.Name)
		fmt.Fprintf(&b, "  exports: %d symbols\n", len(s.ExportedSymbols))
		fmt.Fprintf(&b, "  imports: %d packages\n", s.ImportCount)
		for _, sym := range s.ExportedSymbols {
			fmt.Fprintf(&b, "    - %s (%s)\n", sym.QualifiedName, sym.Kind)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderOverviewCompact(result *query.OverviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summary: %d packages, %d symbols, %d edges\n", result.TotalPackages, result.TotalSymbols, result.TotalEdges)
	for _, s := range result.Packages {
		fmt.Fprintf(&b, "%s: %d exports, %d imports\n", s.Path, len(s.ExportedSymbols), s.ImportCount)
	}
	return b.String()
}

func RenderShow(result *query.ShowResult, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderShowTOON(result)
	case FormatJSON:
		return renderShowJSON(result, options.FullDocs)
	case FormatCompact:
		return renderShowCompact(result)
	case FormatText:
		return renderShowText(result, options.FullDocs)
	default:
		return renderShowTOON(result)
	}
}

func renderShowJSON(r *query.ShowResult, fullDocs bool) string {
	type showJSON struct {
		QualifiedName string             `json:"qualified_name"`
		Kind          string             `json:"kind"`
		Receiver      string             `json:"receiver"`
		Signature     string             `json:"signature"`
		Doc           string             `json:"doc"`
		PosFile       string             `json:"pos_file"`
		IncomingEdges []query.EdgeDetail `json:"incoming_edges"`
		OutgoingEdges []query.EdgeDetail `json:"outgoing_edges"`
		PosLine       int                `json:"pos_line"`
		Exported      bool               `json:"exported"`
	}

	doc := r.Symbol.Doc
	if !fullDocs {
		doc = firstSentence(doc)
	}

	j := showJSON{
		QualifiedName: r.Symbol.QualifiedName,
		Kind:          r.Symbol.Kind,
		Receiver:      r.Symbol.Receiver,
		Signature:     r.Symbol.Signature,
		Doc:           doc,
		PosFile:       r.Symbol.PosFile,
		PosLine:       r.Symbol.PosLine,
		Exported:      r.Symbol.Exported,
		IncomingEdges: r.IncomingEdges,
		OutgoingEdges: r.OutgoingEdges,
	}

	return marshalJSON(j)
}

func renderShowText(r *query.ShowResult, fullDocs bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", r.Symbol.QualifiedName, r.Symbol.Kind)
	if r.Symbol.Receiver != "" {
		fmt.Fprintf(&b, "  receiver: %s\n", r.Symbol.Receiver)
	}
	fmt.Fprintf(&b, "  signature: %s\n", r.Symbol.Signature)
	doc := r.Symbol.Doc
	if !fullDocs {
		doc = firstSentence(doc)
	}
	if doc != "" {
		fmt.Fprintf(&b, "  doc: %s\n", doc)
	}
	fmt.Fprintf(&b, "  pos: %s:%d\n", r.Symbol.PosFile, r.Symbol.PosLine)

	if len(r.IncomingEdges) > 0 {
		b.WriteString("\n  incoming edges:\n")
		for _, e := range r.IncomingEdges {
			fmt.Fprintf(&b, "    %s <- %s (%s) [%s:%d]\n", e.ToRef, e.FromRef, e.EdgeType, e.PosFile, e.PosLine)
		}
	}

	if len(r.OutgoingEdges) > 0 {
		b.WriteString("\n  outgoing edges:\n")
		for _, e := range r.OutgoingEdges {
			fmt.Fprintf(&b, "    %s -> %s (%s) [%s:%d]\n", e.FromRef, e.ToRef, e.EdgeType, e.PosFile, e.PosLine)
		}
	}

	return b.String()
}

func renderShowCompact(r *query.ShowResult) string {
	return fmt.Sprintf("%s %s %s", r.Symbol.Kind, r.Symbol.QualifiedName, r.Symbol.Signature)
}

func RenderCallers(edges []query.EdgeDetail, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderEdgesTOON(edges)
	case FormatJSON:
		return marshalJSON(edges)
	case FormatCompact:
		var b strings.Builder
		for _, e := range edges {
			fmt.Fprintf(&b, "%s (%s) [%s:%d]\n", e.FromRef, e.EdgeType, e.PosFile, e.PosLine)
		}
		return b.String()
	case FormatText:
		var b strings.Builder
		for _, e := range edges {
			fmt.Fprintf(&b, "%s (%s) [%s:%d]\n", e.FromRef, e.EdgeType, e.PosFile, e.PosLine)
		}
		return b.String()
	default:
		return renderEdgesTOON(edges)
	}
}

func RenderCallees(edges []query.EdgeDetail, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderEdgesTOON(edges)
	case FormatJSON:
		return marshalJSON(edges)
	case FormatCompact:
		var b strings.Builder
		for _, e := range edges {
			fmt.Fprintf(&b, "%s (%s) [%s:%d]\n", e.ToRef, e.EdgeType, e.PosFile, e.PosLine)
		}
		return b.String()
	case FormatText:
		var b strings.Builder
		for _, e := range edges {
			fmt.Fprintf(&b, "%s (%s) [%s:%d]\n", e.ToRef, e.EdgeType, e.PosFile, e.PosLine)
		}
		return b.String()
	default:
		return renderEdgesTOON(edges)
	}
}

func RenderSearch(results []query.SearchResult, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderSearchTOON(results)
	case FormatJSON:
		return marshalJSON(results)
	case FormatCompact:
		var b strings.Builder
		for _, r := range results {
			fmt.Fprintf(&b, "%s %s\n", r.Kind, r.QualifiedName)
		}
		return b.String()
	case FormatText:
		var b strings.Builder
		for _, r := range results {
			doc := firstSentence(r.Doc)
			fmt.Fprintf(&b, "%s %s\n", r.QualifiedName, r.Kind)
			if doc != "" {
				fmt.Fprintf(&b, "  %s\n", doc)
			}
		}
		return b.String()
	default:
		return renderSearchTOON(results)
	}
}

func RenderEdges(edges []query.EdgeDetail, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderEdgesTOON(edges)
	case FormatJSON:
		return marshalJSON(edges)
	case FormatCompact:
		var b strings.Builder
		for _, e := range edges {
			fmt.Fprintf(&b, "%s --%s--> %s [%s:%d]\n", e.FromRef, e.EdgeType, e.ToRef, e.PosFile, e.PosLine)
		}
		return b.String()
	case FormatText:
		var b strings.Builder
		for _, e := range edges {
			fmt.Fprintf(&b, "%s --%s--> %s\n", e.FromRef, e.EdgeType, e.ToRef)
			if e.PosFile != "" {
				fmt.Fprintf(&b, "    at %s:%d\n", e.PosFile, e.PosLine)
			}
		}
		return b.String()
	default:
		return renderEdgesTOON(edges)
	}
}

func RenderListPackages(pkgs []store.Package, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderListPackagesTOON(pkgs)
	case FormatJSON:
		return marshalJSON(pkgs)
	case FormatCompact:
		var b strings.Builder
		for _, p := range pkgs {
			fmt.Fprintf(&b, "%s (%s) - %d symbols\n", p.Path, p.Name, p.SymCount)
		}
		return b.String()
	case FormatText:
		var b strings.Builder
		for _, p := range pkgs {
			fmt.Fprintf(&b, "%s (%s) - %d symbols\n", p.Path, p.Name, p.SymCount)
		}
		return b.String()
	default:
		return renderListPackagesTOON(pkgs)
	}
}

func RenderMethodsOf(methods []query.SymbolDetail, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderMethodsOfTOON(methods)
	case FormatJSON:
		return marshalJSON(methods)
	case FormatCompact:
		var b strings.Builder
		for _, m := range methods {
			fmt.Fprintf(&b, "%s %s\n", m.Kind, m.QualifiedName)
		}
		return b.String()
	case FormatText:
		var b strings.Builder
		for _, m := range methods {
			fmt.Fprintf(&b, "%s (%s)\n", m.QualifiedName, m.Kind)
			fmt.Fprintf(&b, "  receiver: %s\n", m.Receiver)
			fmt.Fprintf(&b, "  signature: %s\n", m.Signature)
			fmt.Fprintf(&b, "  pos: %s:%d\n", m.PosFile, m.PosLine)
			if m.Doc != "" {
				fmt.Fprintf(&b, "  doc: %s\n", firstSentence(m.Doc))
			}
		}
		return b.String()
	default:
		return renderMethodsOfTOON(methods)
	}
}

func RenderPackage(pkg *query.PackageResult, opts ...Option) string {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	switch options.Format {
	case FormatTOON:
		return renderPackageTOON(pkg)
	case FormatJSON:
		return renderPackageJSON(pkg)
	case FormatCompact:
		return renderPackageCompact(pkg)
	case FormatText:
		return renderPackageText(pkg)
	default:
		return renderPackageTOON(pkg)
	}
}

func renderPackageText(pkg *query.PackageResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", pkg.Path, pkg.Name)
	fmt.Fprintf(&b, "  imports: %d packages\n", pkg.ImportCount)
	exportedCount := 0
	unexportedCount := 0
	for _, sym := range pkg.ExportedSymbols {
		if sym.Exported {
			exportedCount++
		} else {
			unexportedCount++
		}
	}
	fmt.Fprintf(&b, "  exports: %d symbols\n", exportedCount)
	if unexportedCount > 0 {
		fmt.Fprintf(&b, "  unexported: %d symbols\n", unexportedCount)
	}
	for _, sym := range pkg.ExportedSymbols {
		label := sym.QualifiedName
		if !sym.Exported {
			label = "unexported " + label
		}
		fmt.Fprintf(&b, "    - %s (%s)\n", label, sym.Kind)
		if sym.Receiver != "" {
			fmt.Fprintf(&b, "      receiver: %s\n", sym.Receiver)
		}
		fmt.Fprintf(&b, "      signature: %s\n", sym.Signature)
		fmt.Fprintf(&b, "      pos: %s:%d\n", sym.PosFile, sym.PosLine)
		if sym.Doc != "" {
			fmt.Fprintf(&b, "      doc: %s\n", firstSentence(sym.Doc))
		}
	}
	return b.String()
}

func renderPackageJSON(pkg *query.PackageResult) string {
	return marshalJSON(pkg)
}

func renderPackageCompact(pkg *query.PackageResult) string {
	exportedCount := 0
	for _, sym := range pkg.ExportedSymbols {
		if sym.Exported {
			exportedCount++
		}
	}
	unexportedCount := len(pkg.ExportedSymbols) - exportedCount
	if unexportedCount > 0 {
		return fmt.Sprintf("%s: %d exports, %d unexported, %d imports", pkg.Path, exportedCount, unexportedCount, pkg.ImportCount)
	}
	return fmt.Sprintf("%s: %d exports, %d imports", pkg.Path, exportedCount, pkg.ImportCount)
}

func marshalJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": "marshal failed: %s"}`, err.Error())
	}
	return string(data)
}

func symToJSON(sym query.SymbolDetail, fullDocs bool) symJSON {
	doc := sym.Doc
	if !fullDocs {
		doc = firstSentence(doc)
	}
	return symJSON{
		QualifiedName: sym.QualifiedName,
		Kind:          sym.Kind,
		Signature:     sym.Signature,
		Receiver:      sym.Receiver,
		PosFile:       sym.PosFile,
		PosLine:       sym.PosLine,
		Exported:      sym.Exported,
		Doc:           doc,
	}
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	idx := strings.Index(s, ".")
	if idx == -1 {
		if len(s) > 100 {
			return s[:100] + "..."
		}
		return s
	}
	return s[:idx+1]
}

func renderOverviewTOON(result *query.OverviewResult) string {
	data := map[string]any{
		"summary": map[string]any{
			"packages": result.TotalPackages,
			"symbols":  result.TotalSymbols,
			"edges":    result.TotalEdges,
		},
		"packages": result.Packages,
	}
	out, err := gotoon.Encode(data)
	if err != nil {
		return marshalJSON(result)
	}
	return out
}

func renderShowTOON(result *query.ShowResult) string {
	data := map[string]any{
		"symbol":         result.Symbol,
		"incoming_edges": result.IncomingEdges,
		"outgoing_edges": result.OutgoingEdges,
	}
	out, err := gotoon.Encode(data)
	if err != nil {
		return marshalJSON(result)
	}
	return out
}

func renderEdgesTOON(edges []query.EdgeDetail) string {
	data := map[string]any{"edges": edges}
	out, err := gotoon.Encode(data)
	if err != nil {
		return marshalJSON(edges)
	}
	return out
}

func renderSearchTOON(results []query.SearchResult) string {
	data := map[string]any{"results": results}
	out, err := gotoon.Encode(data)
	if err != nil {
		return marshalJSON(results)
	}
	return out
}

func renderListPackagesTOON(pkgs []store.Package) string {
	data := map[string]any{"packages": pkgs}
	out, err := gotoon.Encode(data)
	if err != nil {
		return marshalJSON(pkgs)
	}
	return out
}

func renderMethodsOfTOON(methods []query.SymbolDetail) string {
	data := map[string]any{"methods": methods}
	out, err := gotoon.Encode(data)
	if err != nil {
		return marshalJSON(methods)
	}
	return out
}

func renderPackageTOON(pkg *query.PackageResult) string {
	data := map[string]any{
		"path":            pkg.Path,
		"name":            pkg.Name,
		"import_count":    pkg.ImportCount,
		"exported_symbols": pkg.ExportedSymbols,
	}
	out, err := gotoon.Encode(data)
	if err != nil {
		return marshalJSON(pkg)
	}
	return out
}
