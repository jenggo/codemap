package render

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"codemap/opt"
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

const keyPackages = "packages"

type Options struct {
	Format   Format
	FullDocs bool
}

// Option mutates render Options. Alias of opt.Option[Options], so existing
// call sites are unaffected by the shared implementation.
type Option = opt.Option[Options]

// WithFormat selects the output format (TOON, text, JSON, compact).
var WithFormat = opt.New(func(o *Options, f Format) { o.Format = f })

// WithFullDocs shows complete documentation instead of the first sentence.
func WithFullDocs() Option {
	return func(o *Options) {
		o.FullDocs = true
	}
}

func RenderOverview(result *query.OverviewResult, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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

// encodeTOON serializes v to TOON, dropping empty repo fields so single-repo
// output stays byte-for-byte identical to pre-workspace builds while workspace
// output carries a repo tag on every symbol and edge.
func encodeTOON(v any) string {
	out, err := gotoon.Encode(toonValue(v))
	if err != nil {
		return marshalJSON(v)
	}
	return out
}

// toonValue normalizes v the way gotoon does (struct -> map via json tags) but
// omits repo keys whose value is empty.
func toonValue(v any) any {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return toonValue(rv.Elem().Interface())
	case reflect.Struct:
		return toonStruct(rv)
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = toonValue(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		return toonMap(rv)
	case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return v
	default:
		return v
	}
}

func toonStruct(rv reflect.Value) any {
	obj := make(map[string]any)
	t := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := toonFieldName(f)
		val := toonValue(rv.Field(i).Interface())
		if isRepoKey(name) && isEmptyValue(val) {
			continue
		}
		obj[name] = val
	}
	return obj
}

func toonFieldName(f reflect.StructField) string {
	if tag := f.Tag.Get("json"); tag != "" && tag != "-" {
		if idx := strings.Index(tag, ","); idx >= 0 {
			tag = tag[:idx]
		}
		return tag
	}
	return f.Name
}

func toonMap(rv reflect.Value) any {
	if rv.Type().Key().Kind() != reflect.String {
		return nil
	}
	obj := make(map[string]any)
	iter := rv.MapRange()
	for iter.Next() {
		k := iter.Key().String()
		val := toonValue(iter.Value().Interface())
		if isRepoKey(k) && isEmptyValue(val) {
			continue
		}
		obj[k] = val
	}
	return obj
}

func isRepoKey(key string) bool {
	return key == "repo" || key == "Repo" || key == "repos"
}

func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
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
			keyPackages: result.TotalPackages,
			"symbols":   result.TotalSymbols,
			"edges":     result.TotalEdges,
		},
		keyPackages: result.Packages,
	}
	if len(result.Repos) > 0 {
		data["repos"] = result.Repos
	}
	out := encodeTOON(data)
	return out
}

func renderShowTOON(result *query.ShowResult) string {
	data := map[string]any{
		"symbol":         result.Symbol,
		"incoming_edges": result.IncomingEdges,
		"outgoing_edges": result.OutgoingEdges,
	}
	out := encodeTOON(data)
	return out
}

func renderEdgesTOON(edges []query.EdgeDetail) string {
	data := map[string]any{"edges": edges}
	out := encodeTOON(data)
	return out
}

func renderSearchTOON(results []query.SearchResult) string {
	data := map[string]any{"results": results}
	out := encodeTOON(data)
	return out
}

func renderListPackagesTOON(pkgs []store.Package) string {
	data := map[string]any{keyPackages: pkgs}
	out := encodeTOON(data)
	return out
}

func renderMethodsOfTOON(methods []query.SymbolDetail) string {
	data := map[string]any{"methods": methods}
	out := encodeTOON(data)
	return out
}

func renderPackageTOON(pkg *query.PackageResult) string {
	data := map[string]any{
		"path":             pkg.Path,
		"name":             pkg.Name,
		"import_count":     pkg.ImportCount,
		"exported_symbols": pkg.ExportedSymbols,
	}
	out := encodeTOON(data)
	return out
}

func RenderSymbolBody(result *query.SymbolBodyResult, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatTOON:
		return renderSymbolBodyTOON(result)
	case FormatJSON:
		return renderSymbolBodyJSON(result)
	case FormatCompact:
		return renderSymbolBodyCompact(result)
	case FormatText:
		return renderSymbolBodyText(result)
	default:
		return renderSymbolBodyTOON(result)
	}
}

type symbolBodyJSON struct {
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	PosFile       string `json:"pos_file"`
	Body          string `json:"body"`
	ContextBefore string `json:"context_before,omitempty"`
	ContextAfter  string `json:"context_after,omitempty"`
	PosLine       int    `json:"pos_line"`
	PosEndLine    int    `json:"pos_end_line"`
	GroupMembers  int    `json:"group_members"`
	PartOfGroup   bool   `json:"part_of_group"`
}

func renderSymbolBodyJSON(r *query.SymbolBodyResult) string {
	return marshalJSON(symbolBodyJSON{
		QualifiedName: r.QualifiedName,
		Kind:          r.Kind,
		PosFile:       r.PosFile,
		PosLine:       r.PosLine,
		PosEndLine:    r.PosEndLine,
		Body:          r.Body,
		PartOfGroup:   r.PartOfGroup,
		GroupMembers:  r.GroupMembers,
		ContextBefore: r.ContextBefore,
		ContextAfter:  r.ContextAfter,
	})
}

func renderSymbolBodyTOON(r *query.SymbolBodyResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", r.QualifiedName, r.Kind)
	fmt.Fprintf(&b, "pos: %s:%d-%d\n", r.PosFile, r.PosLine, r.PosEndLine)
	if r.PartOfGroup {
		fmt.Fprintf(&b, "part_of_group: true (group_members=%d)\n", r.GroupMembers)
	}
	if r.ContextBefore != "" {
		b.WriteString("\n--- context before ---\n")
		b.WriteString(r.ContextBefore)
		b.WriteString("\n")
	}
	b.WriteString("\n```go\n")
	b.WriteString(r.Body)
	if !strings.HasSuffix(r.Body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	if r.ContextAfter != "" {
		b.WriteString("\n--- context after ---\n")
		b.WriteString(r.ContextAfter)
		if !strings.HasSuffix(r.ContextAfter, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func renderSymbolBodyCompact(r *query.SymbolBodyResult) string {
	return fmt.Sprintf("%s %s:%d-%d (%d bytes)", r.Kind, r.PosFile, r.PosLine, r.PosEndLine, len(r.Body))
}

func renderSymbolBodyText(r *query.SymbolBodyResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) %s:%d-%d\n", r.QualifiedName, r.Kind, r.PosFile, r.PosLine, r.PosEndLine)
	if r.PartOfGroup {
		fmt.Fprintf(&b, "[part of group, %d members]\n", r.GroupMembers)
	}
	b.WriteString("----\n")
	b.WriteString(r.Body)
	if !strings.HasSuffix(r.Body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("----\n")
	return b.String()
}

func RenderDependencyLayers(result *query.LayersResult, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatTOON:
		return renderDependencyLayersTOON(result)
	case FormatJSON:
		return renderDependencyLayersJSON(result)
	case FormatCompact:
		return renderDependencyLayersCompact(result)
	case FormatText:
		return renderDependencyLayersText(result)
	default:
		return renderDependencyLayersTOON(result)
	}
}

type layerJSON struct {
	Packages []string `json:"packages"`
	Level    int      `json:"level"`
}

type hubJSON struct {
	Package string `json:"package"`
	FanIn   int    `json:"fan_in"`
	FanOut  int    `json:"fan_out"`
}

type layersJSON struct {
	Layers []layerJSON `json:"layers"`
	Hubs   []hubJSON   `json:"hubs"`
}

func renderDependencyLayersJSON(r *query.LayersResult) string {
	out := layersJSON{
		Layers: make([]layerJSON, 0, len(r.Layers)),
		Hubs:   make([]hubJSON, 0, len(r.Hubs)),
	}
	for _, l := range r.Layers {
		out.Layers = append(out.Layers, layerJSON{Level: l.Level, Packages: l.Packages})
	}
	for _, h := range r.Hubs {
		out.Hubs = append(out.Hubs, hubJSON{Package: h.Package, FanIn: h.FanIn, FanOut: h.FanOut})
	}
	return marshalJSON(out)
}

func renderDependencyLayersText(r *query.LayersResult) string {
	var b strings.Builder
	b.WriteString("Layers:\n")
	for _, l := range r.Layers {
		fmt.Fprintf(&b, "  level %d:\n", l.Level)
		for _, p := range l.Packages {
			fmt.Fprintf(&b, "    - %s\n", p)
		}
	}
	b.WriteString("\nHubs (by fan-in):\n")
	for _, h := range r.Hubs {
		fmt.Fprintf(&b, "  %s — fan_in=%d fan_out=%d\n", h.Package, h.FanIn, h.FanOut)
	}
	return b.String()
}

func renderDependencyLayersCompact(r *query.LayersResult) string {
	var b strings.Builder
	for _, l := range r.Layers {
		fmt.Fprintf(&b, "L%d: %s\n", l.Level, strings.Join(l.Packages, ", "))
	}
	return b.String()
}

func renderDependencyLayersTOON(r *query.LayersResult) string {
	data := map[string]any{
		"layers": r.Layers,
		"hubs":   r.Hubs,
	}
	out := encodeTOON(data)
	return out
}

func RenderDependencyFlow(result *query.FlowResult, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatTOON:
		return renderDependencyFlowTOON(result)
	case FormatJSON:
		return marshalJSON(result)
	case FormatCompact:
		return renderDependencyFlowCompact(result)
	case FormatText:
		return renderDependencyFlowText(result)
	default:
		return renderDependencyFlowTOON(result)
	}
}

func renderDependencyFlowTOON(r *query.FlowResult) string {
	data := map[string]any{
		"package":            r.Package,
		"imports":            r.Imports,
		"importers":          r.Importers,
		"transitive_imports": r.TransitiveImports,
	}
	out := encodeTOON(data)
	return out
}

func renderDependencyFlowText(r *query.FlowResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", r.Package)

	b.WriteString("imports (immediate):\n")
	if len(r.Imports) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, e := range r.Imports {
		fmt.Fprintf(&b, "  -> %s [%s:%d]\n", e.ToRef, e.PosFile, e.PosLine)
	}

	b.WriteString("\nimporters (immediate):\n")
	if len(r.Importers) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, e := range r.Importers {
		fmt.Fprintf(&b, "  <- %s [%s:%d]\n", e.FromRef, e.PosFile, e.PosLine)
	}

	b.WriteString("\ntransitive imports:\n")
	if len(r.TransitiveImports) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, e := range r.TransitiveImports {
		fmt.Fprintf(&b, "  -> %s [%s:%d]\n", e.ToRef, e.PosFile, e.PosLine)
	}
	return b.String()
}

func renderDependencyFlowCompact(r *query.FlowResult) string {
	return fmt.Sprintf("%s: %d imports, %d importers, %d transitive",
		r.Package, len(r.Imports), len(r.Importers), len(r.TransitiveImports))
}

func RenderEntryPoints(entries []query.EntryPoint, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatTOON:
		return renderEntryPointsTOON(entries)
	case FormatJSON:
		return marshalJSON(entries)
	case FormatCompact:
		return renderEntryPointsCompact(entries)
	case FormatText:
		return renderEntryPointsText(entries)
	default:
		return renderEntryPointsTOON(entries)
	}
}

func renderEntryPointsTOON(entries []query.EntryPoint) string {
	byReason := make(map[string][]query.EntryPoint)
	for _, e := range entries {
		byReason[e.Reason] = append(byReason[e.Reason], e)
	}
	data := map[string]any{
		"entry_points": entries,
		"by_reason":    byReason,
	}
	out := encodeTOON(data)
	return out
}

func renderEntryPointsText(entries []query.EntryPoint) string {
	var b strings.Builder
	currentReason := ""
	for _, e := range entries {
		if e.Reason != currentReason {
			currentReason = e.Reason
			fmt.Fprintf(&b, "\n[%s]\n", e.Reason)
		}
		fmt.Fprintf(&b, "  %s (%s) %s:%d\n", e.QualifiedName, e.Kind, e.PosFile, e.PosLine)
	}
	return b.String()
}

func renderEntryPointsCompact(entries []query.EntryPoint) string {
	byReason := make(map[string]int)
	for _, e := range entries {
		byReason[e.Reason]++
	}
	parts := make([]string, 0, len(byReason))
	for k, v := range byReason {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(parts)
	return fmt.Sprintf("entry_points: %d (%s)", len(entries), strings.Join(parts, ", "))
}

func RenderChangedSymbols(result *query.ChangedSymbolsResult, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatTOON:
		return renderChangedSymbolsTOON(result)
	case FormatJSON:
		return marshalJSON(result)
	case FormatCompact:
		return renderChangedSymbolsCompact(result)
	case FormatText:
		return renderChangedSymbolsText(result)
	default:
		return renderChangedSymbolsTOON(result)
	}
}

func renderChangedSymbolsTOON(r *query.ChangedSymbolsResult) string {
	data := map[string]any{
		"summary": r.Summary,
		"symbols": r.Symbols,
	}
	out := encodeTOON(data)
	return out
}

func renderChangedSymbolsText(r *query.ChangedSymbolsResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Files changed: %d, modified: %d, added: %d, removed: %d\n\n",
		r.Summary.FilesChanged, r.Summary.Modified, r.Summary.Added, r.Summary.Removed)
	for _, sym := range r.Symbols {
		fmt.Fprintf(&b, "[%s] %s (%s) %s:%d\n", sym.ChangeType, sym.QualifiedName, sym.Kind, sym.PosFile, sym.PosLine)
		if sym.BlastRadius != nil {
			fmt.Fprintf(&b, "  blast_radius: direct=%d transitive=%d\n",
				sym.BlastRadius.DirectCallers, sym.BlastRadius.TransitiveCallers)
		}
		if sym.Body != "" {
			b.WriteString("  ```go\n")
			b.WriteString(indentEach(sym.Body, "  "))
			b.WriteString("  ```\n")
		}
	}
	return b.String()
}

func renderChangedSymbolsCompact(r *query.ChangedSymbolsResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "files=%d modified=%d added=%d removed=%d\n",
		r.Summary.FilesChanged, r.Summary.Modified, r.Summary.Added, r.Summary.Removed)
	for _, sym := range r.Symbols {
		repo := ""
		if sym.Repo != "" {
			repo = " [" + sym.Repo + "]"
		}
		fmt.Fprintf(&b, "%s %s (%s)%s %s:%d", sym.ChangeType, sym.QualifiedName, sym.Kind, repo, sym.PosFile, sym.PosLine)
		if sym.BlastRadius != nil {
			fmt.Fprintf(&b, " blast(d=%d,t=%d)", sym.BlastRadius.DirectCallers, sym.BlastRadius.TransitiveCallers)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func indentEach(s, prefix string) string {
	lines := strings.Split(s, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		if line != "" {
			b.WriteString(prefix)
		}
		b.WriteString(line)
	}
	return b.String()
}

func RenderTextMatches(matches []store.FileMatch, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatJSON:
		return marshalJSON(matches)
	case FormatCompact:
		var b strings.Builder
		for _, m := range matches {
			fmt.Fprintf(&b, "%s:%d\n", m.FilePath, m.LineNumber)
		}
		return b.String()
	case FormatTOON, FormatText:
		var b strings.Builder
		for _, m := range matches {
			fmt.Fprintf(&b, "%s:%d\n", m.FilePath, m.LineNumber)
			if m.Line != "" {
				fmt.Fprintf(&b, "  %s\n", m.Line)
			}
			if m.ContextBefore != "" {
				b.WriteString(indentEach(m.ContextBefore, "  "))
				b.WriteString("\n")
			}
			if m.ContextAfter != "" {
				b.WriteString(indentEach(m.ContextAfter, "  "))
				b.WriteString("\n")
			}
		}
		return b.String()
	default:
		var b strings.Builder
		for _, m := range matches {
			fmt.Fprintf(&b, "%s:%d\n", m.FilePath, m.LineNumber)
			if m.Line != "" {
				fmt.Fprintf(&b, "  %s\n", m.Line)
			}
		}
		return b.String()
	}
}

func RenderBundle(bundle *query.Bundle, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatJSON:
		return marshalJSON(bundle)
	case FormatCompact:
		return fmt.Sprintf("%s (tokens=%d)", bundle.QualifiedName, bundle.TokenEstimate)
	case FormatTOON, FormatText:
		return renderBundleText(bundle)
	default:
		return renderBundleTOON(bundle)
	}
}

func renderBundleText(bundle *query.Bundle) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Bundle: %s (est. %d tokens)\n\n", bundle.QualifiedName, bundle.TokenEstimate)
	if bundle.Symbol != nil {
		writeBundleSymbol(&b, bundle.Symbol)
	}
	if bundle.Body != "" {
		writeBundleBody(&b, bundle.Body)
	}
	writeBundleCallees(&b, bundle.Callees)
	writeBundleCallers(&b, bundle.Callers)
	writeBundleSameFile(&b, bundle.SameFile)
	return b.String()
}

func writeBundleSymbol(b *strings.Builder, sym *query.SymbolDetail) {
	fmt.Fprintf(b, "Symbol: %s (%s)\n", sym.QualifiedName, sym.Kind)
	fmt.Fprintf(b, "  signature: %s\n", sym.Signature)
	if sym.Doc != "" {
		fmt.Fprintf(b, "  doc: %s\n", firstSentence(sym.Doc))
	}
	fmt.Fprintf(b, "  pos: %s:%d\n\n", sym.PosFile, sym.PosLine)
}

func writeBundleBody(b *strings.Builder, body string) {
	b.WriteString("```go\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n\n")
}

func writeBundleCallees(b *strings.Builder, callees []query.EdgeDetail) {
	if len(callees) == 0 {
		return
	}
	fmt.Fprintf(b, "Callees (%d):\n", len(callees))
	for _, e := range callees {
		fmt.Fprintf(b, "  -> %s (%s)\n", e.ToRef, e.EdgeType)
	}
	b.WriteString("\n")
}

func writeBundleCallers(b *strings.Builder, callers []query.EdgeDetail) {
	if len(callers) == 0 {
		return
	}
	fmt.Fprintf(b, "Callers (%d):\n", len(callers))
	for _, e := range callers {
		fmt.Fprintf(b, "  <- %s (%s)\n", e.FromRef, e.EdgeType)
	}
	b.WriteString("\n")
}

func writeBundleSameFile(b *strings.Builder, sameFile []query.SearchResult) {
	if len(sameFile) == 0 {
		return
	}
	fmt.Fprintf(b, "Same-file symbols (%d):\n", len(sameFile))
	for _, s := range sameFile {
		fmt.Fprintf(b, "  - %s (%s)\n", s.QualifiedName, s.Kind)
	}
}

func renderBundleTOON(b *query.Bundle) string {
	data := map[string]any{
		"qualified_name": b.QualifiedName,
		"symbol":         b.Symbol,
		"body":           b.Body,
		"callees":        b.Callees,
		"callers":        b.Callers,
		"same_file":      b.SameFile,
		"token_estimate": b.TokenEstimate,
	}
	out := encodeTOON(data)
	return out
}

func RenderHotspots(hotspots []query.Hotspot, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatJSON:
		return marshalJSON(hotspots)
	case FormatCompact:
		var b strings.Builder
		for _, h := range hotspots {
			fmt.Fprintf(&b, "%.2f %s C=%d Ch=%d\n", h.RiskScore, h.QualifiedName, h.Complexity, h.ChurnCount)
		}
		return b.String()
	case FormatTOON, FormatText:
		var b strings.Builder
		fmt.Fprintf(&b, "Hotspots (%d):\n\n", len(hotspots))
		for i, h := range hotspots {
			fmt.Fprintf(&b, "%d. %s (%s)\n", i+1, h.QualifiedName, h.Kind)
			fmt.Fprintf(&b, "   risk: %.2f  complexity: %d  churn: %d\n", h.RiskScore, h.Complexity, h.ChurnCount)
			fmt.Fprintf(&b, "   pos: %s:%d\n\n", h.PosFile, h.PosLine)
		}
		for _, h := range hotspots {
			if h.ChurnError != "" {
				fmt.Fprintf(&b, "note: churn data degraded — %s\n", h.ChurnError)
				break
			}
		}
		return b.String()
	default:
		return renderHotspotsTOON(hotspots)
	}
}

func renderHotspotsTOON(hotspots []query.Hotspot) string {
	data := map[string]any{"hotspots": hotspots}
	out := encodeTOON(data)
	return out
}

func RenderImportance(entries []query.ImportanceEntry, opts ...Option) string {
	options := &Options{}
	opt.Apply(options, opts)

	switch options.Format {
	case FormatJSON:
		return marshalJSON(entries)
	case FormatCompact:
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%.4f %s (%s)\n", e.Importance, e.QualifiedName, e.Kind)
		}
		return b.String()
	case FormatTOON, FormatText:
		var b strings.Builder
		fmt.Fprintf(&b, "Symbol Importance (%d):\n\n", len(entries))
		for i, e := range entries {
			fmt.Fprintf(&b, "%d. %s (%s)\n", i+1, e.QualifiedName, e.Kind)
			fmt.Fprintf(&b, "   importance: %.4f\n", e.Importance)
			fmt.Fprintf(&b, "   pos: %s:%d\n\n", e.PosFile, e.PosLine)
		}
		return b.String()
	default:
		return renderImportanceTOON(entries)
	}
}

func renderImportanceTOON(entries []query.ImportanceEntry) string {
	data := map[string]any{"importance": entries}
	out := encodeTOON(data)
	return out
}
