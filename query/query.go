package query

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"codemap/store"
	"codemap/vcs"
)

type Options struct {
	Exported          *bool
	Kind              string
	Package           string
	File              string
	EdgeTypes         []string
	IncludeTests      bool
	IncludeUnexported bool
	Depth             int
}

type Option func(*Options)

func WithTests() Option {
	return func(o *Options) {
		o.IncludeTests = true
	}
}

func WithKind(kind string) Option {
	return func(o *Options) {
		o.Kind = kind
	}
}

func WithExported(exported bool) Option {
	return func(o *Options) {
		o.Exported = &exported
	}
}

func WithUnexported() Option {
	return func(o *Options) {
		o.IncludeUnexported = true
	}
}

func WithPackage(pkg string) Option {
	return func(o *Options) {
		o.Package = pkg
	}
}

func WithFile(file string) Option {
	return func(o *Options) {
		o.File = file
	}
}

func WithEdgeTypes(types ...string) Option {
	return func(o *Options) {
		o.EdgeTypes = types
	}
}

func WithDepth(depth int) Option {
	return func(o *Options) {
		o.Depth = depth
	}
}

type PackageSummary struct {
	Path            string
	Name            string
	ExportedSymbols []SymbolDetail
	ImportCount     int
}

type SymbolDetail struct {
	QualifiedName string
	Kind          string
	Receiver      string
	Signature     string
	Doc           string
	PosFile       string
	PosLine       int
	Exported      bool
}

type EdgeDetail struct {
	FromRef  string `json:"from_ref"`
	ToRef    string `json:"to_ref"`
	EdgeType string `json:"edge_type"`
	PosFile  string `json:"pos_file"`
	PosLine  int    `json:"pos_line"`
}

type ShowResult struct {
	IncomingEdges []EdgeDetail `json:"incoming_edges"`
	OutgoingEdges []EdgeDetail `json:"outgoing_edges"`
	Symbol        SymbolDetail `json:"symbol"`
}

type OverviewResult struct {
	Packages      []PackageSummary
	TotalPackages int
	TotalSymbols  int
	TotalEdges    int
}

type SearchResult struct {
	QualifiedName string
	Kind          string
	Signature     string
	Doc           string
	Receiver      string
	PosFile       string
	PosLine       int
	Exported      bool
}

func edgeTypeAllowed(edgeType string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	return slices.Contains(allowed, edgeType)
}

func edgeToDetail(e store.Edge) EdgeDetail {
	return EdgeDetail{
		FromRef:  e.FromRef,
		ToRef:    e.ToRef,
		EdgeType: e.EdgeType,
		PosFile:  e.PosFile,
		PosLine:  e.PosLine,
	}
}

var allEdgeTypes = []string{edgeTypeCalls, edgeTypeReferences, edgeTypeSatisfies, edgeTypeEmbeds, edgeTypeImports}

const edgeTypeSatisfies = "satisfies"
const edgeTypeCalls = "calls"
const edgeTypeReferences = "references"
const edgeTypeImports = "imports"
const edgeTypeEmbeds = "embeds"

const kindFunction = "function"
const kindMain = "main"
const kindMethod = "method"
const kindVar = "var"
const kindInterface = "interface"
const kindType = "type"

const changeTypeModified = "modified"
const changeTypeAdded = "added"
const changeTypeRemoved = "removed"

type edgeFetcher func(ref string) ([]store.Edge, error)

type nextNodeFn func(e store.Edge) string

func traverse(start string, fetch edgeFetcher, nextNode nextNodeFn, depth int, allowTypes []string) ([]EdgeDetail, error) {
	if depth <= 0 {
		depth = 1
	}

	visited := make(map[string]bool)
	result := make([]EdgeDetail, 0)
	queue := []string{start}

	for d := 0; d < depth && len(queue) > 0; d++ {
		nextQueue := make([]string, 0)
		for _, qn := range queue {
			if visited[qn] {
				continue
			}
			visited[qn] = true

			edges, err := fetch(qn)
			if err != nil {
				return nil, err
			}

			for _, e := range edges {
				if edgeTypeAllowed(e.EdgeType, allowTypes) {
					result = append(result, EdgeDetail{
						FromRef:  e.FromRef,
						ToRef:    e.ToRef,
						EdgeType: e.EdgeType,
						PosFile:  e.PosFile,
						PosLine:  e.PosLine,
					})
					nextQueue = append(nextQueue, nextNode(e))
				}
			}
		}
		queue = nextQueue
	}

	return result, nil
}

func CallersOf(s *store.Store, qualifiedName string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}
	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = allEdgeTypes
	}
	return traverse(qualifiedName, s.EdgesTo, func(e store.Edge) string { return e.FromRef }, options.Depth, allowTypes)
}

func CalleesOf(s *store.Store, qualifiedName string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}
	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = allEdgeTypes
	}
	return traverse(qualifiedName, s.EdgesFrom, func(e store.Edge) string { return e.ToRef }, options.Depth, allowTypes)
}

func Show(s *store.Store, qualifiedName string, opts ...Option) (*ShowResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	sym, err := s.SymbolByName(qualifiedName)
	if err != nil {
		return nil, err
	}

	incoming, err := s.EdgesTo(qualifiedName)
	if err != nil {
		return nil, err
	}

	outgoing, err := s.EdgesFrom(qualifiedName)
	if err != nil {
		return nil, err
	}

	result := &ShowResult{
		Symbol: SymbolDetail{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Receiver:      sym.Receiver,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		},
		IncomingEdges: []EdgeDetail{},
		OutgoingEdges: []EdgeDetail{},
	}

	for _, e := range incoming {
		result.IncomingEdges = append(result.IncomingEdges, EdgeDetail{
			FromRef:  e.FromRef,
			ToRef:    e.ToRef,
			EdgeType: e.EdgeType,
			PosFile:  e.PosFile,
			PosLine:  e.PosLine,
		})
	}

	for _, e := range outgoing {
		result.OutgoingEdges = append(result.OutgoingEdges, EdgeDetail{
			FromRef:  e.FromRef,
			ToRef:    e.ToRef,
			EdgeType: e.EdgeType,
			PosFile:  e.PosFile,
			PosLine:  e.PosLine,
		})
	}

	return result, nil
}

func Overview(s *store.Store, opts ...Option) (*OverviewResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}

	result := &OverviewResult{}

	for _, p := range pkgs {
		if !options.IncludeTests && p.IsTest {
			continue
		}

		summary := buildPackageSummary(s, p, options.IncludeTests)
		result.Packages = append(result.Packages, summary)
	}

	result.TotalPackages = len(result.Packages)
	for _, pkg := range result.Packages {
		result.TotalSymbols += len(pkg.ExportedSymbols)
	}
	allEdges, err := s.AllEdges()
	if err == nil {
		result.TotalEdges = len(allEdges)
	}

	return result, nil
}

func buildPackageSummary(s *store.Store, p store.Package, includeTests bool) PackageSummary {
	syms, _ := s.SymbolsByPackage(p.Path, includeTests)

	summary := PackageSummary{
		Path: p.Path,
		Name: p.Name,
	}

	importEdges, _ := s.EdgesFrom(p.Path)
	for _, e := range importEdges {
		if e.EdgeType == edgeTypeImports {
			summary.ImportCount++
		}
	}

	for _, sym := range syms {
		if sym.Exported {
			summary.ExportedSymbols = append(summary.ExportedSymbols, SymbolDetail{
				QualifiedName: sym.QualifiedName,
				Kind:          sym.Kind,
				Receiver:      sym.Receiver,
				Signature:     sym.Signature,
				Doc:           sym.Doc,
				PosFile:       sym.PosFile,
				PosLine:       sym.PosLine,
				Exported:      sym.Exported,
			})
		}
	}

	return summary
}

func Search(s *store.Store, pattern string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	var syms []store.Symbol
	var err error
	if options.File != "" {
		syms, err = s.SearchSymbolsByFile(options.File, options.Kind, options.Exported, options.IncludeTests)
	} else {
		syms, err = s.SearchSymbols(pattern, options.Kind, options.Exported, options.Package, options.IncludeTests)
	}
	if err != nil {
		return nil, err
	}

	patternLower := strings.ToLower(pattern)
	result := make([]SearchResult, 0, len(syms))
	for _, sym := range syms {
		result = append(result, SearchResult{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			Receiver:      sym.Receiver,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		})
	}

	if options.File == "" {
		sortSearchResults(result, patternLower)
	}
	return result, nil
}

func matchTier(nameLower, patternLower string) int {
	if nameLower == patternLower {
		return 0
	}
	if strings.HasPrefix(nameLower, patternLower) {
		return 1
	}
	return 2
}

func kindPriority(kind string) int {
	switch kind {
	case kindFunction, kindMethod:
		return 0
	case kindInterface, kindType:
		return 1
	default:
		return 2
	}
}

func sortSearchResults(results []SearchResult, patternLower string) {
	sort.SliceStable(results, func(i, j int) bool {
		a := &results[i]
		b := &results[j]

		aTier := matchTier(strings.ToLower(a.QualifiedName), patternLower)
		bTier := matchTier(strings.ToLower(b.QualifiedName), patternLower)
		if aTier != bTier {
			return aTier < bTier
		}

		if a.Exported != b.Exported {
			return a.Exported
		}

		return kindPriority(a.Kind) < kindPriority(b.Kind)
	})
}

func MethodsOf(s *store.Store, typeName string, opts ...Option) ([]SymbolDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	syms, err := s.MethodsByReceiver(typeName, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	if len(syms) == 0 {
		return nil, fmt.Errorf("type not found: %s", typeName)
	}

	results := make([]SymbolDetail, 0, len(syms))
	for _, sym := range syms {
		results = append(results, SymbolDetail{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Receiver:      sym.Receiver,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		})
	}

	return results, nil
}

type PackageResult struct {
	Path            string
	Name            string
	ExportedSymbols []SymbolDetail
	ImportCount     int
}

func Package(s *store.Store, pkgPath string, opts ...Option) (*PackageResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}

	var found *store.Package
	for _, p := range pkgs {
		if p.Path == pkgPath && (!p.IsTest || options.IncludeTests) {
			found = &p
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("package not found: %s", pkgPath)
	}

	syms, err := s.SymbolsByPackage(pkgPath, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	importEdges, _ := s.EdgesFrom(pkgPath)
	importCount := 0
	for _, e := range importEdges {
		if e.EdgeType == edgeTypeImports {
			importCount++
		}
	}

	var symbols []SymbolDetail
	for _, sym := range syms {
		if !options.IncludeUnexported && !sym.Exported {
			continue
		}
		symbols = append(symbols, SymbolDetail{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Receiver:      sym.Receiver,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		})
	}

	return &PackageResult{
		Path:            found.Path,
		Name:            found.Name,
		ImportCount:     importCount,
		ExportedSymbols: symbols,
	}, nil
}

func EdgesByType(s *store.Store, edgeType string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	edges, err := s.EdgesByType(edgeType)
	if err != nil {
		return nil, err
	}

	result := make([]EdgeDetail, len(edges))
	for i, e := range edges {
		result[i] = EdgeDetail{
			FromRef:  e.FromRef,
			ToRef:    e.ToRef,
			EdgeType: e.EdgeType,
			PosFile:  e.PosFile,
			PosLine:  e.PosLine,
		}
	}
	return result, nil
}

func AllEdges(s *store.Store, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	edges, err := s.AllEdges()
	if err != nil {
		return nil, err
	}

	result := make([]EdgeDetail, len(edges))
	for i, e := range edges {
		result[i] = EdgeDetail{
			FromRef:  e.FromRef,
			ToRef:    e.ToRef,
			EdgeType: e.EdgeType,
			PosFile:  e.PosFile,
			PosLine:  e.PosLine,
		}
	}
	return result, nil
}

func ImportersOf(s *store.Store, pkgPath string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = []string{edgeTypeImports}
	}

	edges, err := s.EdgesTo(pkgPath)
	if err != nil {
		return nil, err
	}

	result := filterEdgesByType(edges, allowTypes)
	if len(result) > 0 {
		return result, nil
	}

	fallback := importersViaShortPath(s, pkgPath, allowTypes)
	if fallback != nil {
		return fallback, nil
	}
	return result, nil
}

func filterEdgesByType(edges []store.Edge, allowTypes []string) []EdgeDetail {
	result := make([]EdgeDetail, 0, len(edges))
	for _, e := range edges {
		if edgeTypeAllowed(e.EdgeType, allowTypes) {
			result = append(result, edgeToDetail(e))
		}
	}
	return result
}

func importersViaShortPath(s *store.Store, pkgPath string, allowTypes []string) []EdgeDetail {
	allImports, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil
	}
	pkgs, err := s.ListPackages()
	if err != nil {
		return nil
	}
	projectSet := make(map[string]bool)
	for _, p := range pkgs {
		projectSet[p.Path] = true
	}
	reverseMap := buildImportPathMap(allImports, projectSet)
	importPath, ok := reverseMap[pkgPath]
	if !ok || importPath == pkgPath {
		return nil
	}
	edges, err := s.EdgesTo(importPath)
	if err != nil {
		return nil
	}
	return filterEdgesByType(edges, allowTypes)
}

func ImportsOf(s *store.Store, pkgPath string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	edges, err := s.EdgesFrom(pkgPath)
	if err != nil {
		return nil, err
	}

	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = []string{edgeTypeImports}
	}

	result := make([]EdgeDetail, 0)
	for _, e := range edges {
		if edgeTypeAllowed(e.EdgeType, allowTypes) {
			result = append(result, EdgeDetail{
				FromRef:  e.FromRef,
				ToRef:    e.ToRef,
				EdgeType: e.EdgeType,
				PosFile:  e.PosFile,
				PosLine:  e.PosLine,
			})
		}
	}
	return result, nil
}

func SearchByPrefix(s *store.Store, prefix string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	syms, err := s.SearchByQualifiedNamePrefix(prefix, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	result := make([]SearchResult, 0, len(syms))
	for _, sym := range syms {
		result = append(result, SearchResult{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			Receiver:      sym.Receiver,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		})
	}

	return result, nil
}

func TransitiveImports(s *store.Store, pkgPath string, opts ...Option) ([]EdgeDetail, error) {
	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	projectSet, found := buildProjectSet(pkgs, pkgPath)
	if !found {
		return transitiveImportsLegacy(s, pkgPath)
	}

	allImports, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, err
	}

	return transitiveImportBFS(pkgPath, allImports, projectSet), nil
}

func buildProjectSet(pkgs []store.Package, pkgPath string) (map[string]bool, bool) {
	projectSet := make(map[string]bool)
	found := false
	for _, p := range pkgs {
		projectSet[p.Path] = true
		if p.Path == pkgPath {
			found = true
		}
	}
	return projectSet, found
}

func transitiveImportsLegacy(s *store.Store, pkgPath string) ([]EdgeDetail, error) {
	edges, err := s.TransitiveImports(pkgPath)
	if err != nil {
		return nil, err
	}
	result := make([]EdgeDetail, len(edges))
	for i, e := range edges {
		result[i] = edgeToDetail(e)
	}
	return result, nil
}

func transitiveImportBFS(pkgPath string, allImports []store.Edge, projectSet map[string]bool) []EdgeDetail {
	var out []EdgeDetail
	visited := map[string]bool{pkgPath: true}
	queue := []string{pkgPath}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, e := range allImports {
			if e.FromRef != current {
				continue
			}
			target := resolveProjectTarget(e.ToRef, projectSet)
			if target == "" || visited[target] {
				continue
			}
			out = append(out, edgeToDetail(e))
			visited[target] = true
			queue = append(queue, target)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromRef != out[j].FromRef {
			return out[i].FromRef < out[j].FromRef
		}
		return out[i].ToRef < out[j].ToRef
	})
	return out
}

func TypeUsage(s *store.Store, typeName string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	syms, err := s.SearchByType(typeName, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	result := make([]SearchResult, 0, len(syms))
	for _, sym := range syms {
		result = append(result, SearchResult{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			Receiver:      sym.Receiver,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		})
	}

	return result, nil
}

type PathStep struct {
	From     string
	To       string
	EdgeType string
	PosFile  string
	PosLine  int
}

type pathNode struct {
	name string
	path []PathStep
}

func enqueueIfUnvisited(name string, e store.Edge, path []PathStep, visited map[string]bool, queue []pathNode) []pathNode {
	if visited[name] {
		return queue
	}
	step := PathStep{From: e.FromRef, To: e.ToRef, EdgeType: e.EdgeType, PosFile: e.PosFile, PosLine: e.PosLine}
	nextPath := append(append([]PathStep{}, path...), step)
	return append(queue, pathNode{name: name, path: nextPath})
}

func FindPath(s *store.Store, from, to string, maxDepth int, opts ...Option) ([]PathStep, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	visited := make(map[string]bool)
	queue := []pathNode{{name: from}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current.name] {
			continue
		}
		visited[current.name] = true

		if current.name == to && len(current.path) > 0 {
			return current.path, nil
		}

		if len(current.path) >= maxDepth {
			continue
		}

		edges, err := s.EdgesFrom(current.name)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			queue = enqueueIfUnvisited(e.ToRef, e, current.path, visited, queue)
		}

		edges, err = s.EdgesTo(current.name)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			queue = enqueueIfUnvisited(e.FromRef, e, current.path, visited, queue)
		}
	}

	return nil, fmt.Errorf("no path found from %s to %s", from, to)
}

func MethodSearch(s *store.Store, methodName string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	syms, err := s.MethodsByName(methodName, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	result := make([]SearchResult, 0, len(syms))
	for _, sym := range syms {
		result = append(result, SearchResult{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			Receiver:      sym.Receiver,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		})
	}
	return result, nil
}

type InterfaceImpl struct {
	Interface  string
	ImplType   string
	ImplMethod string
	PosFile    string
	PosLine    int
}

func InterfaceImplementations(s *store.Store, interfaceName string, opts ...Option) ([]InterfaceImpl, error) {
	satisfiesEdges, err := s.EdgesTo(interfaceName)
	if err != nil {
		return nil, err
	}

	var implTypes []string
	seen := make(map[string]bool)
	for _, e := range satisfiesEdges {
		if e.EdgeType == edgeTypeSatisfies && !seen[e.FromRef] {
			seen[e.FromRef] = true
			implTypes = append(implTypes, e.FromRef)
		}
	}

	results := make([]InterfaceImpl, 0)
	for _, implType := range implTypes {
		methods, err := s.MethodsByReceiver(extractShortName(implType), false)
		if err != nil {
			continue
		}
		for _, m := range methods {
			results = append(results, InterfaceImpl{
				Interface:  interfaceName,
				ImplType:   implType,
				ImplMethod: m.QualifiedName,
				PosFile:    m.PosFile,
				PosLine:    m.PosLine,
			})
		}
	}
	return results, nil
}

func extractShortName(qualifiedName string) string {
	parts := strings.Split(qualifiedName, ".")
	if len(parts) == 0 {
		return qualifiedName
	}
	return parts[len(parts)-1]
}

type UnusedSymbol struct {
	QualifiedName string
	Kind          string
	Signature     string
	PosFile       string
	PosLine       int
}

func UnusedSymbols(s *store.Store, opts ...Option) ([]UnusedSymbol, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	allSyms, err := s.AllSymbols(options.IncludeTests)
	if err != nil {
		return nil, err
	}

	edgesFrom, err := s.AllEdges()
	if err != nil {
		return nil, err
	}

	called := make(map[string]bool)
	for _, e := range edgesFrom {
		if e.EdgeType == edgeTypeCalls || e.EdgeType == edgeTypeReferences || e.EdgeType == edgeTypeSatisfies || e.EdgeType == edgeTypeEmbeds {
			called[e.ToRef] = true
		}
	}

	unused := make([]UnusedSymbol, 0)
	for _, sym := range allSyms {
		if sym.Exported {
			continue
		}
		if !called[sym.QualifiedName] {
			unused = append(unused, UnusedSymbol{
				QualifiedName: sym.QualifiedName,
				Kind:          sym.Kind,
				Signature:     sym.Signature,
				PosFile:       sym.PosFile,
				PosLine:       sym.PosLine,
			})
		}
	}
	return unused, nil
}

type Cycle struct {
	Path []string
}

func DetectCycles(s *store.Store, edgeType string) ([]Cycle, error) {
	edges, err := s.EdgesByType(edgeType)
	if err != nil {
		return nil, err
	}

	graph := make(map[string][]string)
	for _, e := range edges {
		graph[e.FromRef] = append(graph[e.FromRef], e.ToRef)
	}

	cycles := make([]Cycle, 0)
	visited := make(map[string]bool)
	inStack := make(map[string]bool)
	var path []string

	var dfs func(node string)
	dfs = func(node string) {
		if inStack[node] {
			cycleStart := -1
			for i, p := range path {
				if p == node {
					cycleStart = i
					break
				}
			}
			if cycleStart >= 0 {
				cycle := append([]string{}, path[cycleStart:]...)
				cycle = append(cycle, node)
				cycles = append(cycles, Cycle{Path: cycle})
			}
			return
		}
		if visited[node] {
			return
		}
		visited[node] = true
		inStack[node] = true
		path = append(path, node)

		for _, next := range graph[node] {
			dfs(next)
		}

		path = path[:len(path)-1]
		inStack[node] = false
	}

	nodes := make(map[string]bool)
	for _, e := range edges {
		nodes[e.FromRef] = true
		nodes[e.ToRef] = true
	}
	for node := range nodes {
		dfs(node)
	}

	return cycles, nil
}

func SymbolsInFile(s *store.Store, filePath string, opts ...Option) ([]SearchResult, error) {
	syms, err := s.SearchSymbolsByFile(filePath, "", nil, false)
	if err != nil {
		return nil, err
	}

	result := make([]SearchResult, 0, len(syms))
	for _, sym := range syms {
		result = append(result, SearchResult{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			Receiver:      sym.Receiver,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
		})
	}
	return result, nil
}

type BlastRadius struct {
	Symbol            string
	DirectCallers     int
	TransitiveCallers int
	Implementations   int
	Embedders         int
	TypeUsers         int
}

func GetBlastRadius(s *store.Store, qualifiedName string, depth int, opts ...Option) (*BlastRadius, error) {
	if depth <= 0 {
		depth = 3
	}

	directCallers, err := s.EdgesTo(qualifiedName)
	if err != nil {
		return nil, err
	}
	directCount := 0
	for _, e := range directCallers {
		if e.EdgeType == edgeTypeCalls || e.EdgeType == edgeTypeReferences {
			directCount++
		}
	}

	transitiveOpts := make([]Option, 0, len(opts)+1)
	transitiveOpts = append(transitiveOpts, opts...)
	transitiveOpts = append(transitiveOpts, WithDepth(depth))
	transitiveCallers, err := CallersOf(s, qualifiedName, transitiveOpts...)
	if err != nil {
		return nil, err
	}

	implCount := 0
	embedCount := 0
	for _, e := range directCallers {
		if e.EdgeType == edgeTypeSatisfies {
			implCount++
		}
		if e.EdgeType == edgeTypeEmbeds {
			embedCount++
		}
	}

	shortName := extractShortName(qualifiedName)
	typeUsers, err := TypeUsage(s, shortName, opts...)
	if err != nil {
		return nil, err
	}

	return &BlastRadius{
		Symbol:            qualifiedName,
		DirectCallers:     directCount,
		TransitiveCallers: len(transitiveCallers),
		Implementations:   implCount,
		Embedders:         embedCount,
		TypeUsers:         len(typeUsers),
	}, nil
}

func ListPackages(s *store.Store, opts ...Option) ([]store.Package, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}

	if !options.IncludeTests {
		var filtered []store.Package
		for _, p := range pkgs {
			if !p.IsTest {
				filtered = append(filtered, p)
			}
		}
		return filtered, nil
	}

	return pkgs, nil
}

type spanMatch struct {
	startOff     int
	endOff       int
	docStartOff  int
	partOfGroup  bool
	groupMembers int
}

func matchFuncDecl(d *ast.FuncDecl, fset *token.FileSet, posLine int, name, kind string) *spanMatch {
	if kind != kindFunction && kind != kindMethod && kind != "" {
		return nil
	}
	if d.Name == nil || d.Name.Name != name || fset.Position(d.Pos()).Line != posLine {
		return nil
	}
	isMethod := d.Recv != nil
	if kind == kindFunction && isMethod {
		return nil
	}
	if kind == kindMethod && !isMethod {
		return nil
	}
	docStart := -1
	if d.Doc != nil {
		docStart = fset.Position(d.Doc.Pos()).Offset
	}
	return &spanMatch{
		startOff:    fset.Position(d.Pos()).Offset,
		endOff:      fset.Position(d.End()).Offset,
		docStartOff: docStart,
	}
}

func matchTypeSpec(s *ast.TypeSpec, d *ast.GenDecl, fset *token.FileSet, posLine int, name string, grouped bool) *spanMatch {
	if s.Name == nil || s.Name.Name != name || fset.Position(s.Pos()).Line != posLine {
		return nil
	}
	startTok, endTok := declRange(fset, d, s, grouped)
	return &spanMatch{
		startOff:     fset.Position(startTok).Offset,
		endOff:       fset.Position(endTok).Offset,
		docStartOff:  typeDocOffset(fset, d, s),
		partOfGroup:  grouped,
		groupMembers: len(d.Specs),
	}
}

func matchValueSpec(s *ast.ValueSpec, d *ast.GenDecl, fset *token.FileSet, posLine int, name string, grouped bool) *spanMatch {
	if len(s.Names) == 0 || fset.Position(s.Pos()).Line != posLine {
		return nil
	}
	found := false
	for _, n := range s.Names {
		if n != nil && n.Name == name {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	startTok, endTok := declRange(fset, d, s, grouped)
	docStart := -1
	if d.Doc != nil {
		docStart = fset.Position(d.Doc.Pos()).Offset
	}
	return &spanMatch{
		startOff:     fset.Position(startTok).Offset,
		endOff:       fset.Position(endTok).Offset,
		docStartOff:  docStart,
		partOfGroup:  grouped,
		groupMembers: len(d.Specs),
	}
}

func matchGenDecl(d *ast.GenDecl, fset *token.FileSet, posLine int, name, kind string) *spanMatch {
	tok, ok := genDeclTokForKind(kind)
	if !ok || d.Tok != tok {
		return nil
	}
	grouped := d.Lparen != token.NoPos
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if m := matchTypeSpec(s, d, fset, posLine, name, grouped); m != nil {
				return m
			}
		case *ast.ValueSpec:
			if m := matchValueSpec(s, d, fset, posLine, name, grouped); m != nil {
				return m
			}
		}
	}
	return nil
}

func symbolSpan(filePath string, posLine int, name, kind string) (startOff, endOff, docStartOff int, partOfGroup bool, groupMembers int, err error) {
	data, readErr := os.ReadFile(filePath)
	if readErr != nil {
		return 0, 0, 0, false, 0, fmt.Errorf("read source file %s: %w", filePath, readErr)
	}

	fset := token.NewFileSet()
	f, parseErr := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if parseErr != nil {
		return 0, 0, 0, false, 0, fmt.Errorf("parse source file %s: %w", filePath, parseErr)
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if m := matchFuncDecl(d, fset, posLine, name, kind); m != nil {
				return m.startOff, m.endOff, m.docStartOff, m.partOfGroup, m.groupMembers, nil
			}
		case *ast.GenDecl:
			if m := matchGenDecl(d, fset, posLine, name, kind); m != nil {
				return m.startOff, m.endOff, m.docStartOff, m.partOfGroup, m.groupMembers, nil
			}
		}
	}

	return 0, 0, 0, false, 0, fmt.Errorf("declaration not found: %s (kind=%s) at line %d in %s", name, kind, posLine, filePath)
}

func symbolEndLine(filePath string, posLine int, name, kind string) (int, error) {
	startOff, endOff, _, _, _, err := symbolSpan(filePath, posLine, name, kind)
	if err != nil {
		return 0, err
	}
	_ = startOff
	data, readErr := os.ReadFile(filePath)
	if readErr != nil {
		return 0, readErr
	}
	lineOffsets := computeLineOffsets(data)
	return offsetToLine(lineOffsets, endOff), nil
}

func genDeclTokForKind(kind string) (token.Token, bool) {
	switch kind {
	case "type", "interface", "struct":
		return token.TYPE, true
	case kindVar:
		return token.VAR, true
	case "const":
		return token.CONST, true
	}
	return token.ILLEGAL, false
}

func declRange(_ *token.FileSet, gd *ast.GenDecl, _ ast.Spec, _ bool) (token.Pos, token.Pos) {
	return gd.Pos(), gd.End()
}

func typeDocOffset(fset *token.FileSet, gd *ast.GenDecl, spec *ast.TypeSpec) int {
	if spec.Doc != nil {
		return fset.Position(spec.Doc.Pos()).Offset
	}
	if gd.Doc != nil {
		return fset.Position(gd.Doc.Pos()).Offset
	}
	return -1
}

type SymbolBodyResult struct {
	QualifiedName string
	Kind          string
	PosFile       string
	Body          string
	ContextBefore string
	ContextAfter  string
	PosLine       int
	PosEndLine    int
	GroupMembers  int
	PartOfGroup   bool
}

func GetSymbolBody(s *store.Store, qualifiedName string, contextLines int, includeDoc bool) (*SymbolBodyResult, error) {
	sym, err := s.SymbolByName(qualifiedName)
	if err != nil {
		return nil, fmt.Errorf("symbol not found: %s", qualifiedName)
	}

	startOff, endOff, docStartOff, partOfGroup, groupMembers, err := symbolSpan(sym.PosFile, sym.PosLine, sym.Name, sym.Kind)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(sym.PosFile)
	if err != nil {
		return nil, fmt.Errorf("read source file %s: %w", sym.PosFile, err)
	}

	bodyStart := startOff
	if includeDoc && docStartOff >= 0 {
		bodyStart = docStartOff
	}
	body := string(data[bodyStart:endOff])

	lineOffsets := computeLineOffsets(data)
	posEndLine := offsetToLine(lineOffsets, endOff)
	posStartLine := offsetToLine(lineOffsets, bodyStart)

	var before, after string
	if contextLines > 0 {
		startLineIdx := max(posStartLine-1-contextLines, 0)
		endLineIdx := posStartLine - 1
		if endLineIdx >= 0 && startLineIdx <= endLineIdx {
			before = sliceLines(data, lineOffsets, startLineIdx, endLineIdx)
		}

		afterStart := posEndLine
		afterEnd := posEndLine - 1 + contextLines
		if afterStart < len(lineOffsets) && afterEnd < len(lineOffsets) {
			after = sliceLines(data, lineOffsets, afterStart, afterEnd)
		}
	}

	return &SymbolBodyResult{
		QualifiedName: sym.QualifiedName,
		Kind:          sym.Kind,
		PosFile:       sym.PosFile,
		PosLine:       sym.PosLine,
		PosEndLine:    posEndLine,
		Body:          body,
		PartOfGroup:   partOfGroup,
		GroupMembers:  groupMembers,
		ContextBefore: before,
		ContextAfter:  after,
	}, nil
}

func computeLineOffsets(data []byte) []int {
	offsets := []int{0}
	for i, b := range data {
		if b == '\n' {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

func offsetToLine(offsets []int, off int) int {
	lo, hi := 0, len(offsets)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if offsets[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}

func sliceLines(data []byte, offsets []int, startLine, endLine int) string {
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		return ""
	}
	if endLine > len(offsets) {
		endLine = len(offsets)
	}
	start := offsets[startLine-1]
	if endLine >= len(offsets) {
		return string(data[start:])
	}
	end := offsets[endLine]
	if end > 0 && data[end-1] == '\n' {
		end--
	}
	return string(data[start:end])
}

type Layer struct {
	Packages []string
	Level    int
}

type Hub struct {
	Package string
	FanIn   int
	FanOut  int
}

type LayersResult struct {
	Layers []Layer
	Hubs   []Hub
}

type hubEntry struct {
	pkg    string
	fanIn  int
	fanOut int
}

func buildProjectGraph(s *store.Store, projectSet map[string]bool) (graph map[string][]string, fanIn, fanOut map[string]int, err error) {
	imports, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, nil, nil, err
	}
	fanIn = make(map[string]int)
	fanOut = make(map[string]int)
	graph = make(map[string][]string)
	for _, e := range imports {
		if !projectSet[e.FromRef] {
			continue
		}
		target := resolveProjectTarget(e.ToRef, projectSet)
		if target == "" {
			continue
		}
		graph[e.FromRef] = append(graph[e.FromRef], target)
		fanOut[e.FromRef]++
		fanIn[target]++
	}
	return graph, fanIn, fanOut, nil
}

func kahnLayers(projectSet map[string]bool, graph map[string][]string) map[string]int {
	level := make(map[string]int)
	reverse := buildReverseGraph(graph)
	var queue []string
	for p := range projectSet {
		if len(graph[p]) == 0 {
			level[p] = 0
			queue = append(queue, p)
		}
	}
	sort.Strings(queue)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		candidate := level[current] + 1
		for _, d := range reverse[current] {
			if existing, ok := level[d]; !ok || candidate > existing {
				level[d] = candidate
			}
		}
		for _, d := range reverse[current] {
			if depsReady(d, graph, level) {
				queue = append(queue, d)
			}
		}
	}
	for p := range projectSet {
		if _, ok := level[p]; !ok {
			level[p] = 0
		}
	}
	return level
}

func buildReverseGraph(graph map[string][]string) map[string][]string {
	reverse := make(map[string][]string)
	for from, tos := range graph {
		for _, to := range tos {
			reverse[to] = append(reverse[to], from)
		}
	}
	return reverse
}

func depsReady(pkg string, graph map[string][]string, level map[string]int) bool {
	for _, dep := range graph[pkg] {
		if _, ok := level[dep]; !ok {
			return false
		}
	}
	return true
}

func levelsToLayers(level map[string]int) []Layer {
	byLevel := make(map[int][]string)
	for p, lv := range level {
		byLevel[lv] = append(byLevel[lv], p)
	}
	for lv := range byLevel {
		sort.Strings(byLevel[lv])
	}
	levels := make([]int, 0, len(byLevel))
	for lv := range byLevel {
		levels = append(levels, lv)
	}
	sort.Ints(levels)
	layers := make([]Layer, 0, len(levels))
	for _, lv := range levels {
		layers = append(layers, Layer{Level: lv, Packages: byLevel[lv]})
	}
	return layers
}

func topHubEntries(projectSet map[string]bool, fanIn, fanOut map[string]int, topHubs int) []Hub {
	allHubs := make([]hubEntry, 0, len(projectSet))
	for p := range projectSet {
		allHubs = append(allHubs, hubEntry{pkg: p, fanIn: fanIn[p], fanOut: fanOut[p]})
	}
	sort.Slice(allHubs, func(i, j int) bool {
		if allHubs[i].fanIn != allHubs[j].fanIn {
			return allHubs[i].fanIn > allHubs[j].fanIn
		}
		return allHubs[i].pkg < allHubs[j].pkg
	})
	limit := min(topHubs, len(allHubs))
	hubs := make([]Hub, 0, limit)
	for i := range limit {
		hubs = append(hubs, Hub{Package: allHubs[i].pkg, FanIn: allHubs[i].fanIn, FanOut: allHubs[i].fanOut})
	}
	return hubs
}

func DependencyLayers(s *store.Store, includeTests bool, topHubs int) (*LayersResult, error) {
	if topHubs <= 0 {
		topHubs = 5
	}
	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	projectSet := make(map[string]bool)
	for _, p := range pkgs {
		if !includeTests && p.IsTest {
			continue
		}
		projectSet[p.Path] = true
	}
	graph, fanIn, fanOut, err := buildProjectGraph(s, projectSet)
	if err != nil {
		return nil, err
	}
	level := kahnLayers(projectSet, graph)
	return &LayersResult{
		Layers: levelsToLayers(level),
		Hubs:   topHubEntries(projectSet, fanIn, fanOut, topHubs),
	}, nil
}

func resolveProjectTarget(toRef string, projectSet map[string]bool) string {
	if projectSet[toRef] {
		return toRef
	}
	suffix := "/" + toRef
	for p := range projectSet {
		if strings.HasSuffix(p, suffix) || p == toRef {
			return p
		}
	}
	if idx := strings.LastIndex(toRef, "/"); idx >= 0 {
		bare := toRef[idx+1:]
		if projectSet[bare] {
			return bare
		}
	}
	return ""
}

type FlowResult struct {
	Package           string
	Imports           []EdgeDetail
	Importers         []EdgeDetail
	TransitiveImports []EdgeDetail
}

func directImports(pkgPath string, allImports []store.Edge, projectSet map[string]bool) []EdgeDetail {
	var out []EdgeDetail
	for _, e := range allImports {
		if e.FromRef == pkgPath && (projectSet[e.ToRef] || resolveProjectTarget(e.ToRef, projectSet) != "") {
			out = append(out, edgeToDetail(e))
		}
	}
	return out
}

func directImporters(pkgPath string, allImports []store.Edge, reverseMap map[string]string) []EdgeDetail {
	var out []EdgeDetail
	for _, e := range allImports {
		if e.ToRef == pkgPath {
			out = append(out, edgeToDetail(e))
		}
	}
	if len(out) > 0 || reverseMap == nil {
		return out
	}
	importPath := reverseMap[pkgPath]
	if importPath == "" || importPath == pkgPath {
		return out
	}
	for _, e := range allImports {
		if e.ToRef == importPath {
			out = append(out, edgeToDetail(e))
		}
	}
	return out
}

func buildImportPathMap(allImports []store.Edge, projectSet map[string]bool) map[string]string {
	m := make(map[string]string)
	for _, e := range allImports {
		if e.EdgeType != edgeTypeImports {
			continue
		}
		target := resolveProjectTarget(e.ToRef, projectSet)
		if target != "" && target != e.ToRef {
			m[target] = e.ToRef
		}
	}
	return m
}

func transitiveImports(pkgPath string, allImports []store.Edge, projectSet map[string]bool) []EdgeDetail {
	var out []EdgeDetail
	visited := map[string]bool{pkgPath: true}
	queue := []string{pkgPath}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, e := range allImports {
			if e.FromRef != current {
				continue
			}
			targetBare := resolveProjectTarget(e.ToRef, projectSet)
			if targetBare == "" || visited[targetBare] {
				continue
			}
			out = append(out, edgeToDetail(e))
			visited[targetBare] = true
			queue = append(queue, targetBare)
		}
	}
	return out
}

func DependencyFlow(s *store.Store, pkgPath string) (*FlowResult, error) {
	if pkgPath == "" {
		return nil, fmt.Errorf("package_path is required")
	}
	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	projectSet := make(map[string]bool)
	found := false
	for _, p := range pkgs {
		projectSet[p.Path] = true
		if p.Path == pkgPath {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("package not found: %s", pkgPath)
	}
	allImports, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, err
	}
	reverseMap := buildImportPathMap(allImports, projectSet)

	return &FlowResult{
		Package:           pkgPath,
		Imports:           directImports(pkgPath, allImports, projectSet),
		Importers:         directImporters(pkgPath, allImports, reverseMap),
		TransitiveImports: transitiveImports(pkgPath, allImports, projectSet),
	}, nil
}

type EntryPoint struct {
	QualifiedName string
	Kind          string
	Signature     string
	PosFile       string
	Reason        string
	PosLine       int
}

var defaultEntryHeuristics = []string{kindMain, "test", "uncalled_exported"}

var entryPointHeuristics = map[string]bool{
	kindMain:            true,
	"test":              true,
	"uncalled_exported": true,
	"handler_sig":       true,
	"handler_name":      true,
}

var handlerNames = map[string]bool{
	"Serve":  true,
	"Handle": true,
	"Run":    true,
	"Start":  true,
	"Listen": true,
}

func addEntryPoint(out []EntryPoint, seen map[string]bool, sym store.Symbol, reason string) []EntryPoint {
	qn := sym.QualifiedName
	if seen[qn] {
		return out
	}
	seen[qn] = true
	return append(out, EntryPoint{
		QualifiedName: qn,
		Kind:          sym.Kind,
		Signature:     sym.Signature,
		PosFile:       sym.PosFile,
		PosLine:       sym.PosLine,
		Reason:        reason,
	})
}

func checkEntryHeuristics(out []EntryPoint, seen map[string]bool, sym store.Symbol, allowed, pkgMain, pkgIsTest, incoming map[string]bool) []EntryPoint {
	pkgPath := sym.PackagePath
	if pkgPath == "" {
		pkgPath = extractPkgPath(sym.QualifiedName)
	}
	isTest := sym.IsTest || pkgIsTest[pkgPath]

	if allowed["test"] && isTest && isTestEntryName(sym.Name) {
		out = addEntryPoint(out, seen, sym, "test")
	}
	if allowed[kindMain] && !isTest && sym.Name == kindMain && sym.Kind == kindFunction && pkgMain[pkgPath] {
		out = addEntryPoint(out, seen, sym, kindMain)
	}
	if allowed["uncalled_exported"] && !isTest && sym.Exported && (sym.Kind == kindFunction || sym.Kind == kindMethod) && !incoming[sym.QualifiedName] && !isTestEntryName(sym.Name) {
		out = addEntryPoint(out, seen, sym, "uncalled_exported")
	}
	if allowed["handler_sig"] && !isTest && sym.Kind == kindMethod && containsHTTPHandler(sym.Signature) {
		out = addEntryPoint(out, seen, sym, "handler_sig")
	}
	if allowed["handler_name"] && !isTest && sym.Kind == kindMethod && handlerNames[sym.Name] {
		out = addEntryPoint(out, seen, sym, "handler_name")
	}
	return out
}

func EntryPoints(s *store.Store, heuristics []string, includeTests bool) ([]EntryPoint, error) {
	if len(heuristics) == 0 {
		heuristics = defaultEntryHeuristics
	}
	allowed := make(map[string]bool, len(heuristics))
	for _, h := range heuristics {
		if entryPointHeuristics[h] {
			allowed[h] = true
		}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("no recognized heuristics: %v", heuristics)
	}

	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	pkgMain := make(map[string]bool)
	pkgIsTest := make(map[string]bool)
	for _, p := range pkgs {
		pkgMain[p.Path] = p.Name == kindMain && !p.IsTest
		pkgIsTest[p.Path] = p.IsTest
	}

	allSyms, err := s.AllSymbols(includeTests)
	if err != nil {
		return nil, err
	}
	allEdges, err := s.AllEdges()
	if err != nil {
		return nil, err
	}
	incoming := make(map[string]bool)
	for _, e := range allEdges {
		if e.EdgeType == edgeTypeCalls || e.EdgeType == edgeTypeReferences {
			incoming[e.ToRef] = true
		}
	}

	var out []EntryPoint
	seen := make(map[string]bool)

	for _, sym := range allSyms {
		out = checkEntryHeuristics(out, seen, sym, allowed, pkgMain, pkgIsTest, incoming)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].QualifiedName < out[j].QualifiedName
	})
	return out, nil
}

func isTestEntryName(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func containsHTTPHandler(sig string) bool {
	return strings.Contains(sig, "http.ResponseWriter") && strings.Contains(sig, "*http.Request")
}

func extractPkgPath(qn string) string {
	idx := strings.LastIndex(qn, ".")
	if idx < 0 {
		return ""
	}
	return qn[:idx]
}

type ChangedSymbol struct {
	BlastRadius   *BlastRadius
	QualifiedName string
	Kind          string
	ChangeType    string
	PosFile       string
	Body          string
	PosLine       int
}

type ChangedSymbolsSummary struct {
	Modified     int
	Added        int
	Removed      int
	FilesChanged int
}

type ChangedSymbolsResult struct {
	Symbols []ChangedSymbol
	Summary ChangedSymbolsSummary
}

func ChangedSymbols(s *store.Store, repoDir, ref string, withBlast, includeBodies, includeTests bool) (*ChangedSymbolsResult, error) {
	repoDir, err := resolveRepoDir(repoDir)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = vcs.DefaultBranch(repoDir)
	}

	statuses, err := vcs.GitFileStatuses(repoDir, ref)
	if err != nil {
		return nil, err
	}

	result := &ChangedSymbolsResult{
		Summary: ChangedSymbolsSummary{FilesChanged: len(statuses)},
	}
	seen := make(map[string]bool)

	for _, st := range statuses {
		if st.Status == "D" {
			processDeletedFile(s, repoDir, ref, result, st.Path, withBlast, includeTests, seen)
			continue
		}
		hunks, herr := vcs.GitHunks(repoDir, ref, st.Path)
		if herr != nil {
			continue
		}
		ct := changeTypeModified
		if strings.HasPrefix(st.Status, "A") {
			ct = changeTypeAdded
		}
		processChangedFile(s, result, st.Path, ct, withBlast, includeBodies, includeTests, seen, hunks)
	}

	return result, nil
}

func processDeletedFile(s *store.Store, repoDir, ref string, result *ChangedSymbolsResult, file string, withBlast, _ bool, seen map[string]bool) {
	syms, err := symbolsAtRef(repoDir, ref, file)
	if err != nil {
		return
	}
	for _, sym := range syms {
		qn := file + "." + sym.Name
		if seen[qn] {
			continue
		}
		seen[qn] = true
		cs := ChangedSymbol{
			QualifiedName: qn,
			Kind:          sym.Kind,
			ChangeType:    changeTypeRemoved,
			PosFile:       file,
			PosLine:       sym.PosLine,
		}
		if withBlast {
			br, _ := GetBlastRadius(s, qn, 3)
			cs.BlastRadius = br
		}
		result.Symbols = append(result.Symbols, cs)
		result.Summary.Removed++
	}
}

func readChangedBody(filePath, name, kind string, posLine int, include bool) string {
	if !include {
		return ""
	}
	startOff, endOff, _, _, _, spanErr := symbolSpan(filePath, posLine, name, kind)
	if spanErr != nil {
		return ""
	}
	data, rerr := os.ReadFile(filePath)
	if rerr != nil {
		return ""
	}
	return string(data[startOff:endOff])
}

func incSummary(ct string, result *ChangedSymbolsResult) {
	switch ct {
	case changeTypeAdded:
		result.Summary.Added++
	case changeTypeModified:
		result.Summary.Modified++
	case changeTypeRemoved:
		result.Summary.Removed++
	}
}

func processChangedFile(s *store.Store, result *ChangedSymbolsResult, file, changeType string, withBlast, includeBodies, includeTests bool, seen map[string]bool, hunks []vcs.Hunk) {
	syms, err := s.SearchSymbolsByFile(file, "", nil, includeTests)
	if err != nil {
		return
	}
	for _, sym := range syms {
		if seen[sym.QualifiedName] {
			continue
		}
		seen[sym.QualifiedName] = true

		ct := changeType
		if ct == changeTypeModified && hunks != nil {
			endLine := sym.PosLine
			if el, elErr := symbolEndLine(sym.PosFile, sym.PosLine, sym.Name, sym.Kind); elErr == nil && el > 0 {
				endLine = el
			}
			ct = refineChangeType(sym.PosLine, endLine, hunks)
			if ct == "" {
				continue
			}
		}

		cs := ChangedSymbol{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			ChangeType:    ct,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
		}

		if ct != changeTypeRemoved {
			cs.Body = readChangedBody(sym.PosFile, sym.Name, sym.Kind, sym.PosLine, includeBodies)
		}

		if withBlast {
			br, _ := GetBlastRadius(s, sym.QualifiedName, 3)
			cs.BlastRadius = br
		}
		result.Symbols = append(result.Symbols, cs)
		incSummary(ct, result)
	}
}

func funcDeclToSymbol(d *ast.FuncDecl, fset *token.FileSet, out []removedSymbol) []removedSymbol {
	if d.Name == nil {
		return out
	}
	pos := fset.Position(d.Pos())
	kind := kindFunction
	if d.Recv != nil {
		kind = kindMethod
	}
	return append(out, removedSymbol{Name: d.Name.Name, Kind: kind, PosLine: pos.Line})
}

func typeSpecToSymbol(s *ast.TypeSpec, fset *token.FileSet, out []removedSymbol) []removedSymbol {
	if s.Name == nil {
		return out
	}
	return append(out, removedSymbol{Name: s.Name.Name, Kind: "type", PosLine: fset.Position(s.Pos()).Line})
}

func valueSpecToSymbols(s *ast.ValueSpec, tok token.Token, fset *token.FileSet, out []removedSymbol) []removedSymbol {
	for _, n := range s.Names {
		if n == nil {
			continue
		}
		out = append(out, removedSymbol{Name: n.Name, Kind: varOrConstKind(tok), PosLine: fset.Position(s.Pos()).Line})
	}
	return out
}

func symbolsAtRef(refRepoDir, ref, file string) ([]removedSymbol, error) {
	data, err := vcs.GitShowFile(refRepoDir, ref, file)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []removedSymbol
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			out = funcDeclToSymbol(d, fset, out)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					out = typeSpecToSymbol(s, fset, out)
				case *ast.ValueSpec:
					out = valueSpecToSymbols(s, d.Tok, fset, out)
				}
			}
		}
	}
	return out, nil
}

type removedSymbol struct {
	Name    string
	Kind    string
	PosLine int
}

func varOrConstKind(tok token.Token) string {
	if tok == token.VAR {
		return kindVar
	}
	if tok == token.CONST {
		return "const"
	}
	return ""
}

func refineChangeType(startLine, endLine int, hunks []vcs.Hunk) string {
	for _, h := range hunks {
		hunkStart := h.NewStart
		hunkEnd := h.NewStart + h.NewCount - 1
		if h.NewCount == 0 {
			hunkStart = h.NewStart
			hunkEnd = h.NewStart
		}
		if startLine <= hunkEnd && endLine >= hunkStart {
			if h.OldCount == 0 {
				return changeTypeAdded
			}
			return changeTypeModified
		}
	}
	return ""
}

func resolveRepoDir(repoDir string) (string, error) {
	if repoDir == "" {
		repoDir = "."
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}
	return abs, nil
}

func SearchText(s *store.Store, pattern, filePattern string, isRegex bool, contextLines int) ([]store.FileMatch, error) {
	return s.SearchFileContent(pattern, filePattern, isRegex, contextLines)
}

type Bundle struct {
	QualifiedName string         `json:"qualified_name"`
	Symbol        *SymbolDetail  `json:"symbol,omitempty"`
	Body          string         `json:"body"`
	Callees       []EdgeDetail   `json:"callees"`
	Callers       []EdgeDetail   `json:"callers"`
	SameFile      []SearchResult `json:"same_file"`
	TokenEstimate int            `json:"token_estimate"`
}

func ContextBundle(s *store.Store, qualifiedName string, tokenBudget int) (*Bundle, error) {
	if tokenBudget <= 0 {
		tokenBudget = 8000
	}

	showResult, err := Show(s, qualifiedName)
	if err != nil {
		return nil, err
	}

	bodyResult, err := GetSymbolBody(s, qualifiedName, 0, true)
	if err != nil {
		return nil, err
	}

	callees, err := CalleesOf(s, qualifiedName, WithDepth(1))
	if err != nil {
		callees = nil
	}

	callers, err := CallersOf(s, qualifiedName, WithDepth(1))
	if err != nil {
		callers = nil
	}

	var sameFile []SearchResult
	if bodyResult.PosFile != "" {
		sameFile, _ = SymbolsInFile(s, bodyResult.PosFile)
	}

	bundle := &Bundle{
		QualifiedName: qualifiedName,
		Symbol:        &showResult.Symbol,
		Body:          bodyResult.Body,
		Callees:       callees,
		Callers:       callers,
		SameFile:      sameFile,
	}

	bundle.TokenEstimate = estimateTokens(bundle)
	if bundle.TokenEstimate > tokenBudget {
		trimBundle(bundle, tokenBudget)
	}

	return bundle, nil
}

func estimateTokens(b *Bundle) int {
	tokens := len(b.Body) / 4
	for _, c := range b.Callees {
		tokens += len(c.FromRef) + len(c.ToRef) + 10
	}
	for _, c := range b.Callers {
		tokens += len(c.FromRef) + len(c.ToRef) + 10
	}
	for _, s := range b.SameFile {
		tokens += len(s.QualifiedName) + len(s.Signature) + 10
	}
	return tokens
}

func trimBundle(b *Bundle, budget int) {
	for len(b.SameFile) > 0 && estimateTokens(b) > budget {
		b.SameFile = b.SameFile[:len(b.SameFile)-1]
	}
	for len(b.Callers) > 0 && estimateTokens(b) > budget {
		b.Callers = b.Callers[:len(b.Callers)-1]
	}
	for len(b.Callees) > 0 && estimateTokens(b) > budget {
		b.Callees = b.Callees[:len(b.Callees)-1]
	}
}

type Hotspot struct {
	QualifiedName string  `json:"qualified_name"`
	Kind          string  `json:"kind"`
	PosFile       string  `json:"pos_file"`
	PosLine       int     `json:"pos_line"`
	Complexity    int     `json:"complexity"`
	ChurnCount    int     `json:"churn_count"`
	RiskScore     float64 `json:"risk_score"`
}

func Hotspots(s *store.Store, topN, minComplexity, minChurn int) ([]Hotspot, error) {
	if topN <= 0 {
		topN = 10
	}

	allSyms, err := s.AllSymbols(false)
	if err != nil {
		return nil, err
	}

	var hotspots []Hotspot
	for _, sym := range allSyms {
		if sym.Complexity < minComplexity || sym.ChurnCount < minChurn {
			continue
		}
		risk := float64(sym.Complexity) * math.Log(float64(sym.ChurnCount)+1)
		hotspots = append(hotspots, Hotspot{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Complexity:    sym.Complexity,
			ChurnCount:    sym.ChurnCount,
			RiskScore:     risk,
		})
	}

	sort.Slice(hotspots, func(i, j int) bool {
		return hotspots[i].RiskScore > hotspots[j].RiskScore
	})

	if len(hotspots) > topN {
		hotspots = hotspots[:topN]
	}

	return hotspots, nil
}

type ImportanceEntry struct {
	QualifiedName string  `json:"qualified_name"`
	Kind          string  `json:"kind"`
	PosFile       string  `json:"pos_file"`
	PosLine       int     `json:"pos_line"`
	Importance    float64 `json:"importance"`
}

func SymbolImportance(s *store.Store, topN, scope int) ([]ImportanceEntry, error) {
	if topN <= 0 {
		topN = 10
	}

	allSyms, err := s.AllSymbols(false)
	if err != nil {
		return nil, err
	}

	allEdges, err := s.AllEdges()
	if err != nil {
		return nil, err
	}

	nodeIdx, nodes := buildNodeIndex(allSyms)
	n := len(nodes)
	if n == 0 {
		return nil, nil
	}

	adj := buildImportAdjacency(allEdges, nodeIdx)
	rank := pagerank(n, adj)

	symMap := make(map[string]store.Symbol)
	for _, sym := range allSyms {
		symMap[sym.QualifiedName] = sym
	}

	entries := buildImportanceEntries(nodes, symMap, rank)
	if len(entries) > topN {
		entries = entries[:topN]
	}

	return entries, nil
}

func buildNodeIndex(allSyms []store.Symbol) (map[string]int, []string) {
	nodeIdx := make(map[string]int)
	var nodes []string
	for _, sym := range allSyms {
		if _, ok := nodeIdx[sym.QualifiedName]; !ok {
			nodeIdx[sym.QualifiedName] = len(nodes)
			nodes = append(nodes, sym.QualifiedName)
		}
	}
	return nodeIdx, nodes
}

func buildImportAdjacency(allEdges []store.Edge, nodeIdx map[string]int) map[int][]int {
	adj := make(map[int][]int)
	for _, e := range allEdges {
		if e.EdgeType != edgeTypeImports {
			continue
		}
		fromIdx, ok1 := nodeIdx[e.FromRef]
		toIdx, ok2 := nodeIdx[e.ToRef]
		if ok1 && ok2 {
			adj[toIdx] = append(adj[toIdx], fromIdx)
		}
	}
	return adj
}

func pagerank(n int, adj map[int][]int) []float64 {
	damping := 0.85
	iterations := 20
	initVal := 1.0 / float64(n)
	rank := make([]float64, n)
	for i := range rank {
		rank[i] = initVal
	}
	for range iterations {
		newRank := make([]float64, n)
		for i := range n {
			sum := 0.0
			for _, from := range adj[i] {
				if outDeg := len(adj[from]); outDeg > 0 {
					sum += rank[from] / float64(outDeg)
				}
			}
			newRank[i] = (1-damping)/float64(n) + damping*sum
		}
		rank = newRank
	}
	return rank
}

func buildImportanceEntries(nodes []string, symMap map[string]store.Symbol, rank []float64) []ImportanceEntry {
	entries := make([]ImportanceEntry, 0, len(nodes))
	for i, name := range nodes {
		sym := symMap[name]
		entries = append(entries, ImportanceEntry{
			QualifiedName: name,
			Kind:          sym.Kind,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
			Importance:    rank[i],
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Importance > entries[j].Importance
	})
	return entries
}
