package query

import (
	"database/sql"
	"errors"
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
	Repo              string
	EdgeTypes         []string
	Depth             int
	IncludeTests      bool
	IncludeUnexported bool
}

type Option func(*Options)

// WithRepo scopes a query to one workspace member (module path).
func WithRepo(repo string) Option {
	return func(o *Options) {
		o.Repo = repo
	}
}

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
	Repo            string `json:"repo,omitempty"`
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
	Repo          string `json:"repo,omitempty"`
	PosLine       int
	Exported      bool
}

type EdgeDetail struct {
	FromRef   string       `json:"from_ref"`
	ToRef     string       `json:"to_ref"`
	EdgeType  string       `json:"edge_type"`
	PosFile   string       `json:"pos_file"`
	Repo      string       `json:"repo,omitempty"`
	State     string       `json:"state,omitempty"`
	Sites     []store.Site `json:"sites"`
	PosLine   int          `json:"pos_line"`
	SiteCount int          `json:"site_count"`
}

type ShowResult struct {
	IncomingEdges []EdgeDetail `json:"incoming_edges"`
	OutgoingEdges []EdgeDetail `json:"outgoing_edges"`
	Symbol        SymbolDetail `json:"symbol"`
}

type OverviewResult struct {
	Packages      []PackageSummary
	Repos         []RepoSummary
	TotalPackages int
	TotalSymbols  int
	TotalEdges    int
}

// RepoSummary reports one workspace member's lifecycle state and index time.
type RepoSummary struct {
	ModulePath string `json:"module_path"`
	Dir        string `json:"dir"`
	State      string `json:"state"`
	IndexedAt  string `json:"indexed_at"`
}

type SearchResult struct {
	QualifiedName string
	Kind          string
	Signature     string
	Doc           string
	Receiver      string
	PosFile       string
	Repo          string `json:"repo,omitempty"`
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
		FromRef:   e.FromRef,
		ToRef:     e.ToRef,
		EdgeType:  e.EdgeType,
		PosFile:   e.PosFile,
		PosLine:   e.PosLine,
		Repo:      e.Repo,
		Sites:     e.Sites,
		SiteCount: e.SiteCount,
	}
}

// filterSearchResults narrows results to one workspace member (module path).
// An empty repo means "no filter" — single-repo mode is unaffected.
func filterSearchResults(results []SearchResult, repo string) []SearchResult {
	if repo == "" {
		return results
	}
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		if r.Repo == repo {
			out = append(out, r)
		}
	}
	return out
}

func filterSymbolDetails(items []SymbolDetail, repo string) []SymbolDetail {
	if repo == "" {
		return items
	}
	out := make([]SymbolDetail, 0, len(items))
	for _, it := range items {
		if it.Repo == repo {
			out = append(out, it)
		}
	}
	return out
}

func filterEdges(edges []EdgeDetail, repo string) []EdgeDetail {
	if repo == "" {
		return edges
	}
	out := make([]EdgeDetail, 0, len(edges))
	for _, e := range edges {
		if e.Repo == repo {
			out = append(out, e)
		}
	}
	return out
}

func filterPathSteps(path []PathStep, repo string) []PathStep {
	if repo == "" {
		return path
	}
	out := make([]PathStep, 0, len(path))
	for _, p := range path {
		if p.Repo == repo {
			out = append(out, p)
		}
	}
	return out
}

// repoMismatch reports whether a symbol lookup violates a repo scope.
func repoMismatch(actualRepo, wanted string) bool {
	return wanted != "" && actualRepo != wanted
}

var allEdgeTypes = []string{edgeTypeCalls, edgeTypeReferences, edgeTypeSatisfies, edgeTypeEmbeds, edgeTypeImports, edgeTypeContract}

const edgeTypeSatisfies = "satisfies"
const edgeTypeCalls = "calls"
const edgeTypeReferences = "references"
const edgeTypeImports = "imports"
const edgeTypeEmbeds = "embeds"
const edgeTypeContract = "contract"

const kindFunction = "function"
const kindMain = "main"
const kindMethod = "method"
const kindVar = "var"
const kindInterface = "interface"
const kindType = "type"

const changeTypeModified = "modified"
const changeTypeAdded = "added"
const changeTypeRemoved = "removed"

// traverse walks the edge graph level by level, fetching every edge for the
// whole frontier in one batched query per depth instead of one query per
// visited node. Depth semantics and the visited set match the previous
// node-at-a-time implementation exactly.
func traverse(s *store.Store, start string, outgoing bool, depth int, allowTypes []string) ([]EdgeDetail, error) {
	if depth <= 0 {
		depth = 1
	}

	visited := make(map[string]bool)
	result := make([]EdgeDetail, 0)
	queue := []string{start}

	for d := 0; d < depth && len(queue) > 0; d++ {
		frontier := frontierNodes(queue, visited)
		if len(frontier) == 0 {
			break
		}

		edges, err := s.EdgesForNodes(frontier, allowTypes, outgoing)
		if err != nil {
			return nil, err
		}

		levelResult, nextQueue := expandLevel(edges, allowTypes, outgoing, visited)
		result = append(result, levelResult...)
		queue = nextQueue
	}

	return result, nil
}

// frontierNodes returns the unvisited nodes of queue, marking them visited.
func frontierNodes(queue []string, visited map[string]bool) []string {
	frontier := make([]string, 0, len(queue))
	for _, qn := range queue {
		if visited[qn] {
			continue
		}
		visited[qn] = true
		frontier = append(frontier, qn)
	}
	return frontier
}

// expandLevel converts one depth level's edges to details and enqueues the
// unvisited next-hop nodes, returning both.
func expandLevel(edges []store.Edge, allowTypes []string, outgoing bool, visited map[string]bool) ([]EdgeDetail, []string) {
	result := make([]EdgeDetail, 0, len(edges))
	nextQueue := make([]string, 0)
	for _, e := range edges {
		if !edgeTypeAllowed(e.EdgeType, allowTypes) {
			continue
		}
		result = append(result, edgeToDetail(e))
		next := e.ToRef
		if !outgoing {
			next = e.FromRef
		}
		if !visited[next] {
			nextQueue = append(nextQueue, next)
		}
	}
	return result, nextQueue
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
	edges, err := traverse(s, qualifiedName, false, options.Depth, allowTypes)
	if err != nil {
		return nil, err
	}
	return filterEdges(edges, options.Repo), nil
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
	edges, err := traverse(s, qualifiedName, true, options.Depth, allowTypes)
	if err != nil {
		return nil, err
	}
	return filterEdges(edges, options.Repo), nil
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
	if repoMismatch(sym.Repo, options.Repo) {
		return nil, fmt.Errorf("symbol %s not found in repo %s", qualifiedName, options.Repo)
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
			Repo:          sym.Repo,
		},
		IncomingEdges: []EdgeDetail{},
		OutgoingEdges: []EdgeDetail{},
	}

	for _, e := range incoming {
		result.IncomingEdges = append(result.IncomingEdges, edgeToDetail(e))
	}

	for _, e := range outgoing {
		result.OutgoingEdges = append(result.OutgoingEdges, edgeToDetail(e))
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

	importCounts, err := s.ImportCounts()
	if err != nil {
		return nil, fmt.Errorf("overview: import counts: %w", err)
	}

	for _, p := range pkgs {
		if !options.IncludeTests && p.IsTest {
			continue
		}
		if repoMismatch(p.Repo, options.Repo) {
			continue
		}

		summary, err := buildPackageSummary(s, p, options.IncludeTests, importCounts)
		if err != nil {
			return nil, err
		}
		result.Packages = append(result.Packages, summary)
	}

	result.TotalPackages = len(result.Packages)
	for _, pkg := range result.Packages {
		result.TotalSymbols += len(pkg.ExportedSymbols)
	}
	totalEdges, err := s.CountEdges()
	if err != nil {
		return nil, fmt.Errorf("overview: counting edges: %w", err)
	}
	result.TotalEdges = totalEdges

	repos, err := s.WorkspaceHealth()
	if err != nil {
		return nil, fmt.Errorf("overview: workspace health: %w", err)
	}
	for _, r := range repos {
		if repoMismatch(r.ModulePath, options.Repo) {
			continue
		}
		result.Repos = append(result.Repos, RepoSummary{
			ModulePath: r.ModulePath,
			Dir:        r.Dir,
			State:      s.RepoState(r),
			IndexedAt:  r.IndexedAt,
		})
	}

	return result, nil
}

func buildPackageSummary(s *store.Store, p store.Package, includeTests bool, importCounts map[string]int) (PackageSummary, error) {
	syms, err := s.SymbolsByPackage(p.Path, includeTests)
	if err != nil {
		return PackageSummary{}, fmt.Errorf("overview: symbols for %s: %w", p.Path, err)
	}

	summary := PackageSummary{
		Path:        p.Path,
		Name:        p.Name,
		Repo:        p.Repo,
		ImportCount: importCounts[p.Path],
	}

	summary.ExportedSymbols = symbolDetails(syms, false)

	return summary, nil
}

// symbolDetails converts store symbols to query details, keeping only exported
// symbols unless includeUnexported is set.
func symbolDetails(syms []store.Symbol, includeUnexported bool) []SymbolDetail {
	var symbols []SymbolDetail
	for _, sym := range syms {
		if !includeUnexported && !sym.Exported {
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
			Repo:          sym.Repo,
		})
	}
	return symbols
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
			Repo:          sym.Repo,
		})
	}

	if options.File == "" {
		sortSearchResults(result, patternLower)
	}
	return filterSearchResults(result, options.Repo), nil
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
	// Lowercase the qualified names once; the comparator fires O(n log n) times
	// and would otherwise re-lower the same string per comparison.
	keys := make([]string, len(results))
	for i := range results {
		keys[i] = strings.ToLower(results[i].QualifiedName)
	}
	sort.SliceStable(results, func(i, j int) bool {
		a := &results[i]
		b := &results[j]

		aTier := matchTier(keys[i], patternLower)
		bTier := matchTier(keys[j], patternLower)
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
			Repo:          sym.Repo,
		})
	}

	results = filterSymbolDetails(results, options.Repo)
	if options.Repo != "" && len(results) == 0 {
		return nil, fmt.Errorf("type not found: %s in repo %s", typeName, options.Repo)
	}
	return results, nil
}

type PackageResult struct {
	Path            string
	Name            string
	Repo            string `json:"repo,omitempty"`
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
		if p.Path == pkgPath && (!p.IsTest || options.IncludeTests) && !repoMismatch(p.Repo, options.Repo) {
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

	importEdges, err := s.EdgesFrom(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("package %s: import edges: %w", pkgPath, err)
	}
	importCount := 0
	for _, e := range importEdges {
		if e.EdgeType == edgeTypeImports {
			importCount++
		}
	}

	return &PackageResult{
		Path:            found.Path,
		Name:            found.Name,
		ImportCount:     importCount,
		ExportedSymbols: symbolDetails(syms, options.IncludeUnexported),
		Repo:            found.Repo,
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
		result[i] = edgeToDetail(e)
	}
	return filterEdges(result, options.Repo), nil
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
		result[i] = edgeToDetail(e)
	}
	return filterEdges(result, options.Repo), nil
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
	result = filterEdges(result, options.Repo)
	if len(result) > 0 {
		return result, nil
	}

	fallback, ferr := importersViaShortPath(s, pkgPath, allowTypes)
	if ferr != nil {
		return nil, ferr
	}
	if fallback != nil {
		return filterEdges(fallback, options.Repo), nil
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

func importersViaShortPath(s *store.Store, pkgPath string, allowTypes []string) ([]EdgeDetail, error) {
	allImports, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, fmt.Errorf("importers of %s: import edges: %w", pkgPath, err)
	}
	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, fmt.Errorf("importers of %s: packages: %w", pkgPath, err)
	}
	projectSet := make(map[string]bool)
	for _, p := range pkgs {
		projectSet[p.Path] = true
	}
	reverseMap := buildImportPathMap(allImports, projectSet)
	importPath, ok := reverseMap[pkgPath]
	if !ok || importPath == pkgPath {
		return nil, nil
	}
	edges, err := s.EdgesTo(importPath)
	if err != nil {
		return nil, fmt.Errorf("importers of %s: edges to %s: %w", pkgPath, importPath, err)
	}
	return filterEdgesByType(edges, allowTypes), nil
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

	knownModules, err := s.KnownModulePaths()
	if err != nil {
		return nil, fmt.Errorf("imports of %s: known modules: %w", pkgPath, err)
	}
	pkgSet, err := s.ListPackages()
	if err != nil {
		return nil, fmt.Errorf("imports of %s: packages: %w", pkgPath, err)
	}
	indexed := make(map[string]bool)
	for _, p := range pkgSet {
		indexed[p.Path] = true
	}

	result := make([]EdgeDetail, 0)
	for _, e := range edges {
		if edgeTypeAllowed(e.EdgeType, allowTypes) {
			d := edgeToDetail(e)
			if e.EdgeType == edgeTypeImports && !indexed[e.ToRef] {
				if targetModule := modulePathForRef(e.ToRef, knownModules); targetModule != "" {
					d.State = "not indexed"
				}
			}
			result = append(result, d)
		}
	}
	return filterEdges(result, options.Repo), nil
}

// modulePathForRef returns the configured module path that owns ref (ref is a
// package import path or symbol name built from one), or "" when ref is not
// under any known module. Prefix-collision-safe: the longest matching module
// wins and ref must cross a path boundary.
func modulePathForRef(ref string, knownModules []string) string {
	best := ""
	for _, m := range knownModules {
		if ref == m || strings.HasPrefix(ref, m+"/") || strings.HasPrefix(ref, m+".") {
			if len(m) > len(best) {
				best = m
			}
		}
	}
	return best
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
			Repo:          sym.Repo,
		})
	}

	return filterSearchResults(result, options.Repo), nil
}

func TransitiveImports(s *store.Store, pkgPath string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	projectSet, found := buildProjectSet(pkgs, pkgPath)
	if !found {
		legacy, err := transitiveImportsLegacy(s, pkgPath)
		if err != nil {
			return nil, err
		}
		return filterEdges(legacy, options.Repo), nil
	}

	allImports, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, err
	}

	return filterEdges(transitiveImportBFS(pkgPath, store.BuildImportAdjacency(allImports), projectSet), options.Repo), nil
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

func transitiveImportBFS(pkgPath string, adj map[string][]store.Edge, projectSet map[string]bool) []EdgeDetail {
	edges := store.ImportFrontierBFS(adj, pkgPath, func(e store.Edge, visited map[string]bool) string {
		target := resolveProjectTarget(e.ToRef, projectSet)
		if target == "" || visited[target] {
			return ""
		}
		return target
	})
	out := make([]EdgeDetail, 0, len(edges))
	for _, e := range edges {
		out = append(out, edgeToDetail(e))
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
			Repo:          sym.Repo,
		})
	}

	return filterSearchResults(result, options.Repo), nil
}

type PathStep struct {
	From     string
	To       string
	EdgeType string
	PosFile  string
	Repo     string
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
	step := PathStep{From: e.FromRef, To: e.ToRef, EdgeType: e.EdgeType, PosFile: e.PosFile, PosLine: e.PosLine, Repo: e.Repo}
	nextPath := append(append([]PathStep{}, path...), step)
	return append(queue, pathNode{name: name, path: nextPath})
}

// ErrNoPath reports that a traversal found no connection between two symbols.
// It is distinct from a query failure, which is returned as a wrapped error so
// callers can tell "no path exists" apart from "the query failed".
var ErrNoPath = errors.New("no path found")

func FindPath(s *store.Store, from, to string, maxDepth int, opts ...Option) ([]PathStep, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}
	if maxDepth <= 0 {
		maxDepth = 10
	}

	// Contract edges (non-suggested, non-suppressed) join the traversal so
	// cross-repo wire relationships show up in paths.
	contracts, err := s.QueryContracts(store.ContractFilter{})
	if err != nil {
		return nil, fmt.Errorf("find_path: contracts: %w", err)
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
			return filterPathSteps(current.path, options.Repo), nil
		}

		if len(current.path) >= maxDepth {
			continue
		}

		queue, err = expandPathNode(s, current, options.Repo, queue, visited, contracts)
		if err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("%w from %s to %s", ErrNoPath, from, to)
}

// expandPathNode enqueues every out- and in-edge of current (respecting a repo
// scope) plus cross-repo contract edges as the next BFS frontier.
func expandPathNode(s *store.Store, current pathNode, repo string, queue []pathNode, visited map[string]bool, contracts []store.Contract) ([]pathNode, error) {
	outEdges, err := s.EdgesFrom(current.name)
	if err != nil {
		return nil, fmt.Errorf("find_path: edges from %s: %w", current.name, err)
	}
	for _, e := range outEdges {
		if !repoMismatch(e.Repo, repo) {
			queue = enqueueIfUnvisited(e.ToRef, e, current.path, visited, queue)
		}
	}

	inEdges, err := s.EdgesTo(current.name)
	if err != nil {
		return nil, fmt.Errorf("find_path: edges to %s: %w", current.name, err)
	}
	for _, e := range inEdges {
		if !repoMismatch(e.Repo, repo) {
			queue = enqueueIfUnvisited(e.FromRef, e, current.path, visited, queue)
		}
	}

	for _, c := range contracts {
		if c.Suggested {
			continue
		}
		switch {
		case c.FromRef == current.name:
			if !repoMismatch(c.Repo, repo) {
				queue = enqueueIfUnvisited(c.ToRef, contractToEdge(c), current.path, visited, queue)
			}
		case c.ToRef == current.name:
			if !repoMismatch(c.Repo, repo) {
				queue = enqueueIfUnvisited(c.FromRef, contractToEdge(c), current.path, visited, queue)
			}
		}
	}
	return queue, nil
}

// contractToEdge adapts a contract row to an edge step for path rendering.
func contractToEdge(c store.Contract) store.Edge {
	return store.Edge{
		FromRef:  c.FromRef,
		ToRef:    c.ToRef,
		EdgeType: edgeTypeContract,
		Repo:     c.Repo,
	}
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
			Repo:          sym.Repo,
		})
	}
	return filterSearchResults(result, options.Repo), nil
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
	// Truncated is set on the last element of a truncated result: the graph
	// contains more distinct cycles than the returned set (limited by maxCycles).
	Truncated bool
}

// maxCycles bounds cycle enumeration on adversarial graphs. Pathological
// strongly-connected components can contain exponentially many simple cycles,
// so results are capped and the truncation is surfaced on the last element.
const maxCycles = 1000

func DetectCycles(s *store.Store, edgeType string) ([]Cycle, error) {
	edges, err := s.EdgesByType(edgeType)
	if err != nil {
		return nil, err
	}

	finder := newCycleFinder(edges)
	for _, root := range finder.nodes {
		finder.dfs(root, root)
		if finder.capped() {
			finder.truncated = true
			break
		}
	}

	if finder.truncated && len(finder.cycles) > 0 {
		finder.cycles[len(finder.cycles)-1].Truncated = true
	}

	return finder.cycles, nil
}

// cycleFinder runs a per-root DFS over a directed graph to enumerate every
// distinct simple cycle: each cycle is discovered with its smallest node as
// root (edges from nodes below the root are never expanded), then rotated to
// canonical form and deduplicated, so the shared-visited bug that missed
// most SCC cycles is gone and output is deterministic.
type cycleFinder struct {
	graph     map[string][]string
	nodes     []string
	cycles    []Cycle
	seen      map[string]bool
	inStack   map[string]bool
	path      []string
	truncated bool
}

func newCycleFinder(edges []store.Edge) *cycleFinder {
	graph := make(map[string][]string)
	nodeSet := make(map[string]bool)
	for _, e := range edges {
		graph[e.FromRef] = append(graph[e.FromRef], e.ToRef)
		nodeSet[e.FromRef] = true
		nodeSet[e.ToRef] = true
	}
	for k, tos := range graph {
		slices.Sort(tos)
		graph[k] = slices.Compact(tos)
	}

	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	slices.Sort(nodes)

	return &cycleFinder{
		graph:   graph,
		nodes:   nodes,
		cycles:  make([]Cycle, 0),
		seen:    make(map[string]bool),
		inStack: make(map[string]bool),
	}
}

// capped reports whether the cycle cap has been reached.
func (f *cycleFinder) capped() bool {
	return len(f.cycles) >= maxCycles
}

// emit records the cycle formed by the current path back to node.
func (f *cycleFinder) emit(node string) {
	start := -1
	for i, p := range f.path {
		if p == node {
			start = i
			break
		}
	}
	if start < 0 {
		return
	}
	cycle := append([]string{}, f.path[start:]...)
	cycle = append(cycle, node)
	key := canonicalCycle(cycle)
	if key != "" && !f.seen[key] {
		f.seen[key] = true
		f.cycles = append(f.cycles, Cycle{Path: cycle})
	}
}

func (f *cycleFinder) dfs(node, root string) {
	if f.capped() {
		f.truncated = true
		return
	}
	if f.inStack[node] {
		f.emit(node)
		return
	}

	f.inStack[node] = true
	f.path = append(f.path, node)

	if node >= root {
		for _, next := range f.graph[node] {
			f.dfs(next, root)
			if f.capped() {
				f.truncated = true
				break
			}
		}
	}

	f.path = f.path[:len(f.path)-1]
	f.inStack[node] = false
}

// canonicalCycle rotates a cycle (first node repeated at the end) so its
// smallest node comes first, and returns a key suitable for deduplication.
// Returns "" for degenerate inputs.
func canonicalCycle(cycle []string) string {
	n := len(cycle) - 1
	if n <= 0 || cycle[0] != cycle[len(cycle)-1] {
		return ""
	}
	minIdx := 0
	for i := 1; i < n; i++ {
		if cycle[i] < cycle[minIdx] {
			minIdx = i
		}
	}
	rot := make([]string, 0, n+1)
	for i := range n {
		rot = append(rot, cycle[(minIdx+i)%n])
	}
	rot = append(rot, rot[0])
	return strings.Join(rot, "\x00")
}

func SymbolsInFile(s *store.Store, filePath string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}
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
			Repo:          sym.Repo,
		})
	}
	return filterSearchResults(result, options.Repo), nil
}

type BlastRadius struct {
	Symbol            string
	DirectCallers     int
	TransitiveCallers int
	Implementations   int
	Embedders         int
	TypeUsers         int
}

// errSymbolMissing distinguishes a non-existent symbol from a query failure in
// blast-radius lookups; internal consumers (changed_symbols) tolerate it for
// symbols that were removed from the index.
var errSymbolMissing = errors.New("symbol not found")

func GetBlastRadius(s *store.Store, qualifiedName string, depth int, opts ...Option) (*BlastRadius, error) {
	options := &Options{}
	for _, o := range opts {
		o(options)
	}
	if depth <= 0 {
		depth = 3
	}

	// Verify the symbol exists so an empty blast radius for a typo'd or removed
	// symbol is never mistaken for a genuine (empty) result.
	if _, err := s.SymbolByName(qualifiedName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", errSymbolMissing, qualifiedName)
		}
		return nil, fmt.Errorf("blast_radius: %s: %w", qualifiedName, err)
	}

	directCallers, err := s.EdgesTo(qualifiedName)
	if err != nil {
		return nil, err
	}

	transitiveOpts := make([]Option, 0, len(opts)+1)
	transitiveOpts = append(transitiveOpts, opts...)
	transitiveOpts = append(transitiveOpts, WithDepth(depth))
	transitiveCallers, err := CallersOf(s, qualifiedName, transitiveOpts...)
	if err != nil {
		return nil, err
	}

	implCount, embedCount := countSatisfiesAndEmbeds(directCallers, options.Repo)

	shortName := extractShortName(qualifiedName)
	typeUsers, err := TypeUsage(s, shortName, opts...)
	if err != nil {
		return nil, err
	}

	return &BlastRadius{
		Symbol:            qualifiedName,
		DirectCallers:     countDirectCallers(directCallers, options.Repo),
		TransitiveCallers: len(transitiveCallers),
		Implementations:   implCount,
		Embedders:         embedCount,
		TypeUsers:         len(typeUsers),
	}, nil
}

func countDirectCallers(edges []store.Edge, repo string) int {
	n := 0
	for _, e := range edges {
		if !repoMismatch(e.Repo, repo) && (e.EdgeType == edgeTypeCalls || e.EdgeType == edgeTypeReferences) {
			n++
		}
	}
	return n
}

func countSatisfiesAndEmbeds(edges []store.Edge, repo string) (impl, embed int) {
	for _, e := range edges {
		if repoMismatch(e.Repo, repo) {
			continue
		}
		if e.EdgeType == edgeTypeSatisfies {
			impl++
		}
		if e.EdgeType == edgeTypeEmbeds {
			embed++
		}
	}
	return impl, embed
}

// BlastRadiusWithContracts extends GetBlastRadius to include contract-aware
// cross-repo consumers. Non-suggested contract edges are counted as direct
// consumers; suggested edges are excluded unless explicitly opted in.
func BlastRadiusWithContracts(s *store.Store, qualifiedName string, depth int, opts ...Option) (*BlastRadius, error) {
	br, err := GetBlastRadius(s, qualifiedName, depth, opts...)
	if err != nil {
		return nil, err
	}

	// Query contract edges where this symbol is the producer.
	contracts, err := s.QueryContracts(store.ContractFilter{})
	if err != nil {
		return br, nil // contract query failure is non-fatal
	}

	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	contractConsumers := 0
	for _, c := range contracts {
		if c.FromRef == qualifiedName && c.Direction == store.ContractDirectionProducer && !c.Suggested {
			if !repoMismatch(c.Repo, options.Repo) {
				contractConsumers++
			}
		}
		if c.ToRef == qualifiedName && c.Direction == store.ContractDirectionConsumer && !c.Suggested {
			if !repoMismatch(c.Repo, options.Repo) {
				contractConsumers++
			}
		}
	}

	br.DirectCallers += contractConsumers
	return br, nil
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
			if !p.IsTest && !repoMismatch(p.Repo, options.Repo) {
				filtered = append(filtered, p)
			}
		}
		return filtered, nil
	}

	var filtered []store.Package
	for _, p := range pkgs {
		if !repoMismatch(p.Repo, options.Repo) {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

type spanMatch struct {
	startOff     int
	endOff       int
	docStartOff  int
	partOfGroup  bool
	groupMembers int
}

func matchFuncDecl(d *ast.FuncDecl, fset *token.FileSet, posLine int, receiver, name, kind string) *spanMatch {
	if kind != kindFunction && kind != kindMethod && kind != "" {
		return nil
	}
	if d.Name == nil || d.Name.Name != name {
		return nil
	}
	if posLine != 0 && fset.Position(d.Pos()).Line != posLine {
		return nil
	}
	isMethod := d.Recv != nil
	if kind == kindFunction && isMethod {
		return nil
	}
	if kind == kindMethod && !isMethod {
		return nil
	}
	if kind == kindMethod && receiver != "" {
		astRecv := ""
		if d.Recv != nil && len(d.Recv.List) > 0 {
			astRecv = astReceiverName(d.Recv.List[0].Type)
		}
		if astRecv != receiverBaseName(receiver) {
			return nil
		}
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

// astReceiverName extracts a method receiver's base type name from its AST
// expression, stripping pointer and type-parameter wrappers ("*Server[T]" →
// "Server").
func astReceiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return astReceiverName(t.X)
	case *ast.IndexExpr:
		return astReceiverName(t.X)
	case *ast.IndexListExpr:
		return astReceiverName(t.X)
	default:
		return ""
	}
}

// receiverBaseName normalizes a stored receiver string ("Store", "*Store", or
// "Store[T]") to its base type name for comparison against the AST.
func receiverBaseName(r string) string {
	r = strings.TrimPrefix(r, "*")
	if i := strings.IndexByte(r, '['); i >= 0 {
		r = r[:i]
	}
	return r
}

func matchTypeSpec(s *ast.TypeSpec, d *ast.GenDecl, fset *token.FileSet, posLine int, name string, grouped bool) *spanMatch {
	if s.Name == nil || s.Name.Name != name || (posLine != 0 && fset.Position(s.Pos()).Line != posLine) {
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
	if len(s.Names) == 0 || (posLine != 0 && fset.Position(s.Pos()).Line != posLine) {
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

func symbolSpan(filePath string, posLine int, receiver, name, kind string) (startOff, endOff, docStartOff int, partOfGroup bool, groupMembers int, err error) {
	data, readErr := os.ReadFile(filePath)
	if readErr != nil {
		return 0, 0, 0, false, 0, fmt.Errorf("read source file %s: %w", filePath, readErr)
	}

	fset := token.NewFileSet()
	f, parseErr := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if parseErr != nil {
		return 0, 0, 0, false, 0, fmt.Errorf("parse source file %s: %w", filePath, parseErr)
	}

	matchAtLine := func(line int) *spanMatch {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if m := matchFuncDecl(d, fset, line, receiver, name, kind); m != nil {
					return m
				}
			case *ast.GenDecl:
				if m := matchGenDecl(d, fset, line, name, kind); m != nil {
					return m
				}
			}
		}
		return nil
	}

	// Exact-line match first: the fast path when the index is fresh.
	if m := matchAtLine(posLine); m != nil {
		return m.startOff, m.endOff, m.docStartOff, m.partOfGroup, m.groupMembers, nil
	}

	// Name-only fallback: the index may be stale (file edited since indexing),
	// so the recorded line no longer matches. Top-level names are unique per
	// file and methods are disambiguated by receiver, so this stays safe.
	if posLine != 0 {
		if m := matchAtLine(0); m != nil {
			return m.startOff, m.endOff, m.docStartOff, m.partOfGroup, m.groupMembers, nil
		}
	}

	return 0, 0, 0, false, 0, fmt.Errorf("declaration not found: %s (kind=%s) at line %d in %s", name, kind, posLine, filePath)
}

func symbolEndLine(filePath string, posLine int, receiver, name, kind string) (int, error) {
	startOff, endOff, _, _, _, err := symbolSpan(filePath, posLine, receiver, name, kind)
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

	startOff, endOff, docStartOff, partOfGroup, groupMembers, err := symbolSpan(sym.PosFile, sym.PosLine, sym.Receiver, sym.Name, sym.Kind)
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

func transitiveImports(pkgPath string, adj map[string][]store.Edge, projectSet map[string]bool) []EdgeDetail {
	edges := store.ImportFrontierBFS(adj, pkgPath, func(e store.Edge, visited map[string]bool) string {
		target := resolveProjectTarget(e.ToRef, projectSet)
		if target == "" || visited[target] {
			return ""
		}
		return target
	})
	out := make([]EdgeDetail, 0, len(edges))
	for _, e := range edges {
		out = append(out, edgeToDetail(e))
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
		TransitiveImports: transitiveImports(pkgPath, store.BuildImportAdjacency(allImports), projectSet),
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
	Repo          string `json:"repo,omitempty"`
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
			if err := processDeletedFile(s, repoDir, ref, result, st.Path, withBlast, includeTests, seen); err != nil {
				return nil, err
			}
			continue
		}
		hunks, herr := vcs.GitHunks(repoDir, ref, st.Path)
		if herr != nil {
			return nil, fmt.Errorf("changed_symbols: git hunks for %s: %w", st.Path, herr)
		}
		ct := changeTypeModified
		if strings.HasPrefix(st.Status, "A") {
			ct = changeTypeAdded
		}
		if err := processChangedFile(s, result, st.Path, ct, withBlast, includeBodies, includeTests, seen, hunks); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// WorkspaceChangedSymbols computes a workspace-wide diff: changed symbols from
// every indexed member, each computed against that repo's own default ref and
// tagged with its repo (module path). Repos with no checkout are skipped.
func WorkspaceChangedSymbols(s *store.Store, withBlast, includeBodies, includeTests bool) (*ChangedSymbolsResult, error) {
	repos, err := s.ListRepos()
	if err != nil {
		return nil, err
	}

	result := &ChangedSymbolsResult{}
	for _, r := range repos {
		if r.Missing {
			continue
		}
		if _, err := os.Stat(r.Dir); os.IsNotExist(err) {
			continue
		}
		perRepo, err := ChangedSymbols(s, r.Dir, "", withBlast, includeBodies, includeTests)
		if err != nil {
			continue
		}
		for i := range perRepo.Symbols {
			perRepo.Symbols[i].Repo = r.ModulePath
		}
		result.Symbols = append(result.Symbols, perRepo.Symbols...)
		result.Summary.Modified += perRepo.Summary.Modified
		result.Summary.Added += perRepo.Summary.Added
		result.Summary.Removed += perRepo.Summary.Removed
		result.Summary.FilesChanged += perRepo.Summary.FilesChanged
	}
	return result, nil
}

func processDeletedFile(s *store.Store, repoDir, ref string, result *ChangedSymbolsResult, file string, withBlast, _ bool, seen map[string]bool) error {
	syms, err := symbolsAtRef(repoDir, ref, file)
	if err != nil {
		return fmt.Errorf("changed_symbols: symbols at %s@%s: %w", file, ref, err)
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
			br, berr := blastRadiusFor(s, qn)
			if berr != nil {
				return berr
			}
			cs.BlastRadius = br
		}
		result.Symbols = append(result.Symbols, cs)
		result.Summary.Removed++
	}
	return nil
}

func readChangedBody(filePath, name, kind, receiver string, posLine int, include bool) (string, error) {
	if !include {
		return "", nil
	}
	startOff, endOff, _, _, _, spanErr := symbolSpan(filePath, posLine, receiver, name, kind)
	if spanErr != nil {
		return "", fmt.Errorf("changed_symbols: body for %s in %s: %w", name, filePath, spanErr)
	}
	data, rerr := os.ReadFile(filePath)
	if rerr != nil {
		return "", fmt.Errorf("changed_symbols: body for %s in %s: %w", name, filePath, rerr)
	}
	return string(data[startOff:endOff]), nil
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

func processChangedFile(s *store.Store, result *ChangedSymbolsResult, file, changeType string, withBlast, includeBodies, includeTests bool, seen map[string]bool, hunks []vcs.Hunk) error {
	syms, err := s.SearchSymbolsByFile(file, "", nil, includeTests)
	if err != nil {
		return fmt.Errorf("changed_symbols: symbols in %s: %w", file, err)
	}
	for _, sym := range syms {
		if seen[sym.QualifiedName] {
			continue
		}
		seen[sym.QualifiedName] = true

		ct := refinedChangeType(sym, changeType, hunks)
		if ct == "" {
			continue
		}

		cs := ChangedSymbol{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			ChangeType:    ct,
			PosFile:       sym.PosFile,
			PosLine:       sym.PosLine,
		}

		if ct != changeTypeRemoved {
			body, berr := readChangedBody(sym.PosFile, sym.Name, sym.Kind, sym.Receiver, sym.PosLine, includeBodies)
			if berr != nil {
				return berr
			}
			cs.Body = body
		}

		if withBlast {
			br, berr := blastRadiusFor(s, sym.QualifiedName)
			if berr != nil {
				return berr
			}
			cs.BlastRadius = br
		}
		result.Symbols = append(result.Symbols, cs)
		incSummary(ct, result)
	}
	return nil
}

// refinedChangeType narrows a coarse added/modified status to the per-symbol
// change type using the file's hunks; "" means the symbol was not touched by
// any hunk.
func refinedChangeType(sym store.Symbol, changeType string, hunks []vcs.Hunk) string {
	ct := changeType
	if ct == changeTypeModified && hunks != nil {
		endLine := sym.PosLine
		if el, elErr := symbolEndLine(sym.PosFile, sym.PosLine, sym.Receiver, sym.Name, sym.Kind); elErr == nil && el > 0 {
			endLine = el
		}
		ct = refineChangeType(sym.PosLine, endLine, hunks)
	}
	return ct
}

// blastRadiusFor computes a symbol's blast radius, tolerating symbols that are
// no longer in the index (removed files) as a nil radius.
func blastRadiusFor(s *store.Store, qn string) (*BlastRadius, error) {
	br, err := GetBlastRadius(s, qn, 3)
	if err != nil {
		if errors.Is(err, errSymbolMissing) {
			return nil, nil
		}
		return nil, fmt.Errorf("changed_symbols: blast radius for %s: %w", qn, err)
	}
	return br, nil
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
		return nil, fmt.Errorf("context_bundle: callees of %s: %w", qualifiedName, err)
	}

	callers, err := CallersOf(s, qualifiedName, WithDepth(1))
	if err != nil {
		return nil, fmt.Errorf("context_bundle: callers of %s: %w", qualifiedName, err)
	}

	var sameFile []SearchResult
	if bodyResult.PosFile != "" {
		sameFile, err = SymbolsInFile(s, bodyResult.PosFile)
		if err != nil {
			return nil, fmt.Errorf("context_bundle: same-file symbols for %s: %w", bodyResult.PosFile, err)
		}
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
	ChurnError    string  `json:"churn_error,omitempty"`
	PosLine       int     `json:"pos_line"`
	Complexity    int     `json:"complexity"`
	ChurnCount    int     `json:"churn_count"`
	RiskScore     float64 `json:"risk_score"`
}

func Hotspots(s *store.Store, topN, minComplexity, minChurn int) ([]Hotspot, error) {
	if topN <= 0 {
		topN = 10
	}

	// Surface degraded churn from the index so empty ChurnCount is never
	// mistaken for a real "no churn" signal.
	churnError, err := s.ChurnDegraded()
	if err != nil {
		return nil, fmt.Errorf("hotspots: churn degradation: %w", err)
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
			ChurnError:    churnError,
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

func SymbolImportance(s *store.Store, topN, scope int, opts ...Option) ([]ImportanceEntry, error) {
	if topN <= 0 {
		topN = 10
	}
	// scope is reserved: accepted for CLI/MCP compatibility but has no effect
	// on the ranking.
	_ = scope

	options := &Options{}
	for _, o := range opts {
		o(options)
	}

	allSyms, err := s.AllSymbols(false)
	if err != nil {
		return nil, err
	}
	if options.Repo != "" {
		filtered := make([]store.Symbol, 0, len(allSyms))
		for _, sym := range allSyms {
			if sym.Repo == options.Repo {
				filtered = append(filtered, sym)
			}
		}
		allSyms = filtered
	}

	allEdges, err := s.SymbolEdges([]string{edgeTypeCalls, edgeTypeReferences, edgeTypeSatisfies})
	if err != nil {
		return nil, err
	}
	if options.Repo != "" {
		filtered := make([]store.Edge, 0, len(allEdges))
		for _, e := range allEdges {
			if e.Repo == options.Repo {
				filtered = append(filtered, e)
			}
		}
		allEdges = filtered
	}

	nodes, adj, outdeg := buildSymbolAdjacency(allSyms, allEdges)
	n := len(nodes)
	if n == 0 {
		return make([]ImportanceEntry, 0), nil
	}

	rank := pagerank(n, adj, outdeg)

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

// buildSymbolAdjacency maps each symbol node to its outgoing symbol neighbors
// (from-ref → to-refs) using the given symbol-level edges. Only edges whose
// endpoints are indexed symbols are kept; every other node is isolated with no
// out-edges and therefore teleports in PageRank.
func buildSymbolAdjacency(allSyms []store.Symbol, edges []store.Edge) ([]string, map[int][]int, []int) {
	nodeIdx := make(map[string]int)
	nodes := make([]string, 0, len(allSyms))
	for _, sym := range allSyms {
		if _, ok := nodeIdx[sym.QualifiedName]; ok {
			continue
		}
		nodeIdx[sym.QualifiedName] = len(nodes)
		nodes = append(nodes, sym.QualifiedName)
	}

	adj := make(map[int][]int)
	outdeg := make([]int, len(nodes))
	for _, e := range edges {
		fromIdx, ok1 := nodeIdx[e.FromRef]
		toIdx, ok2 := nodeIdx[e.ToRef]
		if !ok1 || !ok2 {
			continue
		}
		adj[fromIdx] = append(adj[fromIdx], toIdx)
		outdeg[fromIdx]++
	}
	return nodes, adj, outdeg
}

// pagerank computes the PageRank vector over an index-based directed graph.
// Damping is 0.85; iteration stops when the residual drops below 1e-6 or after
// 100 iterations. Nodes with no outgoing edges teleport their full mass to the
// uniform base term.
func pagerank(n int, adj map[int][]int, outdeg []int) []float64 {
	const (
		damping = 0.85
		epsilon = 1e-6
		maxIter = 100
	)
	initVal := 1.0 / float64(n)
	rank := make([]float64, n)
	for i := range rank {
		rank[i] = initVal
	}

	for range maxIter {
		newRank := pageStep(n, adj, outdeg, rank, damping)
		if rankResidual(newRank, rank) < epsilon {
			rank = newRank
			break
		}
		rank = newRank
	}
	return rank
}

// pageStep computes one PageRank iteration: base rank plus incoming mass from
// neighbors, with nodes lacking outgoing edges teleporting into the base.
func pageStep(n int, adj map[int][]int, outdeg []int, rank []float64, damping float64) []float64 {
	dangling := 0.0
	for i := range n {
		if outdeg[i] == 0 {
			dangling += rank[i]
		}
	}

	newRank := make([]float64, n)
	base := (1-damping)/float64(n) + damping*dangling/float64(n)
	for i := range n {
		newRank[i] = base
	}
	for i := range n {
		for _, to := range adj[i] {
			newRank[to] += damping * rank[i] / float64(outdeg[i])
		}
	}
	return newRank
}

// rankResidual returns the maximum absolute difference between two rank vectors.
func rankResidual(a, b []float64) float64 {
	residual := 0.0
	for i := range a {
		delta := a[i] - b[i]
		if delta < 0 {
			delta = -delta
		}
		if delta > residual {
			residual = delta
		}
	}
	return residual
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
