package query

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"codemap/store"
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

var allEdgeTypes = []string{"calls", "references", "satisfies", "embeds", "imports"}

const edgeTypeSatisfies = "satisfies"

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
		if e.EdgeType == "imports" {
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
	case "function", "method":
		return 0
	case "interface", "type":
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
		if e.EdgeType == "imports" {
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

	edges, err := s.EdgesTo(pkgPath)
	if err != nil {
		return nil, err
	}

	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = []string{"imports"}
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
		allowTypes = []string{"imports"}
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
	edges, err := s.TransitiveImports(pkgPath)
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

func FindPath(s *store.Store, from, to string, maxDepth int, opts ...Option) ([]PathStep, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	type node struct {
		name string
		path []PathStep
	}
	visited := make(map[string]bool)
	queue := []node{{name: from}}

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
			if !visited[e.ToRef] {
				step := PathStep{From: e.FromRef, To: e.ToRef, EdgeType: e.EdgeType, PosFile: e.PosFile, PosLine: e.PosLine}
				queue = append(queue, node{name: e.ToRef, path: append(append([]PathStep{}, current.path...), step)})
			}
		}

		edges, err = s.EdgesTo(current.name)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			if !visited[e.FromRef] {
				step := PathStep{From: e.FromRef, To: e.ToRef, EdgeType: e.EdgeType, PosFile: e.PosFile, PosLine: e.PosLine}
				queue = append(queue, node{name: e.FromRef, path: append(append([]PathStep{}, current.path...), step)})
			}
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

	var results []InterfaceImpl
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
		if e.EdgeType == "calls" || e.EdgeType == "references" || e.EdgeType == edgeTypeSatisfies || e.EdgeType == "embeds" {
			called[e.ToRef] = true
		}
	}

	var unused []UnusedSymbol
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

	var cycles []Cycle
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
	Symbol           string
	DirectCallers    int
	TransitiveCallers int
	Implementations  int
	Embedders        int
	TypeUsers        int
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
		if e.EdgeType == "calls" || e.EdgeType == "references" {
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
		if e.EdgeType == "embeds" {
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
