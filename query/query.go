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
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"codemap/opt"
	"codemap/store"
	"codemap/vcs"
)

type Options struct {
	Exported          *bool
	Kind              string
	Package           string
	File              string
	Repo              string
	Generated         store.GeneratedFilter
	EdgeTypes         []string
	Depth             int
	IncludeTests      bool
	IncludeUnexported bool
	IncludeFields     bool
	IncludeSymbols    bool
}

// Option mutates query Options. Alias of opt.Option[Options], so existing
// call sites are unaffected by the shared implementation.
type Option = opt.Option[Options]

var (
	// WithRepo scopes a query to one workspace member (module path).
	WithRepo = opt.New(func(o *Options, repo string) { o.Repo = repo })

	WithKind     = opt.New(func(o *Options, kind string) { o.Kind = kind })
	WithExported = opt.New(func(o *Options, exported bool) { o.Exported = &exported })
	WithPackage  = opt.New(func(o *Options, pkg string) { o.Package = pkg })
	WithFile     = opt.New(func(o *Options, file string) { o.File = file })
	WithDepth    = opt.New(func(o *Options, depth int) { o.Depth = depth })

	// WithGenerated selects how search treats generated-code symbols: keep them
	// (store.GeneratedAny, the default), drop them (GeneratedExclude), or keep
	// only them (GeneratedOnly).
	WithGenerated = opt.New(func(o *Options, filter store.GeneratedFilter) { o.Generated = filter })
)

// WithTests includes test packages and symbols in the result.
func WithTests() Option {
	return func(o *Options) {
		o.IncludeTests = true
	}
}

// WithUnexported includes private symbols (package tool).
func WithUnexported() Option {
	return func(o *Options) {
		o.IncludeUnexported = true
	}
}

// WithFields attaches struct fields to package and search results.
func WithFields() Option {
	return func(o *Options) {
		o.IncludeFields = true
	}
}

// WithSymbols makes overview include each package's full symbol list. Off by
// default: the inventory was 248 KB on a mid-sized repo, always over the
// response limit, so orientation cost a round trip before it could be read.
func WithSymbols() Option {
	return func(o *Options) {
		o.IncludeSymbols = true
	}
}

// WithEdgeTypes restricts returned edges to the given types. Kept variadic
// so call sites can pass both single values and spread slices.
func WithEdgeTypes(types ...string) Option {
	return func(o *Options) {
		o.EdgeTypes = types
	}
}

type PackageSummary struct {
	Path        string         `json:"path"`
	Name        string         `json:"name"`
	Doc         string         `json:"doc"`
	Repo        string         `json:"repo,omitempty"`
	Symbols     []SymbolDetail `json:"symbols,omitempty"`
	ImportCount int            `json:"import_count"`
	SymCount    int            `json:"sym_count"`
}

type SymbolDetail struct {
	QualifiedName string              `json:"qualified_name"`
	Kind          string              `json:"kind"`
	Receiver      string              `json:"receiver"`
	Signature     string              `json:"signature"`
	Doc           string              `json:"doc"`
	PosFile       string              `json:"pos_file"`
	Repo          string              `json:"repo,omitempty"`
	Fields        []store.SymbolField `json:"fields,omitempty"`
	PosLine       int                 `json:"pos_line"`
	Exported      bool                `json:"exported"`
	Generated     bool                `json:"generated"`
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
	External  bool         `json:"external,omitempty"`
	Dispatch  bool         `json:"dispatch,omitempty"`
}

type ShowResult struct {
	IncomingEdges []EdgeDetail `json:"incoming_edges"`
	OutgoingEdges []EdgeDetail `json:"outgoing_edges"`
	Symbol        SymbolDetail `json:"symbol"`
	// Declarations counts declarations sharing the queried qualified name. More
	// than one means the payload describes one of several and its edges are
	// merged across all of them, so a reader must know which file it is seeing.
	Declarations int `json:"declarations,omitempty"`
}

type OverviewResult struct {
	Packages      []PackageSummary `json:"packages"`
	Repos         []RepoSummary    `json:"repos"`
	TotalPackages int              `json:"total_packages"`
	TotalSymbols  int              `json:"total_symbols"`
	TotalEdges    int              `json:"total_edges"`
}

// RepoSummary reports one workspace member's lifecycle state and index time.
type RepoSummary struct {
	ModulePath string `json:"module_path"`
	Dir        string `json:"dir"`
	State      string `json:"state"`
	IndexedAt  string `json:"indexed_at"`
}

type SearchResult struct {
	QualifiedName string              `json:"qualified_name"`
	Kind          string              `json:"kind"`
	Signature     string              `json:"signature"`
	Doc           string              `json:"doc"`
	Receiver      string              `json:"receiver"`
	PosFile       string              `json:"pos_file"`
	Repo          string              `json:"repo,omitempty"`
	Fields        []store.SymbolField `json:"fields,omitempty"`
	PosLine       int                 `json:"pos_line"`
	Exported      bool                `json:"exported"`
	Generated     bool                `json:"generated"`
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
const kindAlias = "alias"

const changeTypeModified = "modified"
const changeTypeAdded = "added"
const changeTypeRemoved = "removed"
const changeTypeRenamed = "renamed"

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
	opt.Apply(options, opts)
	if err := requireSymbol(s, qualifiedName); err != nil {
		return nil, err
	}
	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = allEdgeTypes
	}
	edges, err := traverse(s, qualifiedName, false, options.Depth, allowTypes)
	if err != nil {
		return nil, err
	}
	if err := markExternal(s, edges); err != nil {
		return nil, err
	}
	dispatch, err := dispatchRows(s, qualifiedName)
	if err != nil {
		return nil, err
	}
	return filterEdges(append(edges, dispatch...), options.Repo), nil
}

// dispatchRows returns the sites where a method's receiver type escapes into an
// interface. Such a method has no calls edge onto itself, so without these rows
// a method reached only through interface dispatch reads as uncalled. The rows
// point at the receiver type, not at a synthesised call: dispatch does not
// claim a call site that the code does not have.
func dispatchRows(s *store.Store, qualifiedName string) ([]EdgeDetail, error) {
	sym, err := s.SymbolByName(qualifiedName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if sym.Kind != kindMethod || sym.Receiver == "" {
		return nil, nil
	}
	recvQN := sym.PackagePath + "." + receiverBaseName(sym.Receiver)
	recv, err := s.SymbolByName(recvQN)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if recv.Kind == kindInterface {
		return nil, nil
	}
	edges, err := s.EdgesTo(recvQN)
	if err != nil {
		return nil, err
	}
	rows := make([]EdgeDetail, 0, len(edges))
	for _, e := range edges {
		if e.EdgeType != edgeTypeReferences && e.EdgeType != edgeTypeSatisfies && e.EdgeType != edgeTypeEmbeds {
			continue
		}
		if e.FromRef == recvQN || strings.HasPrefix(e.FromRef, recvQN+".") {
			continue
		}
		row := edgeToDetail(e)
		row.Dispatch = true
		rows = append(rows, row)
	}
	return rows, nil
}

func CalleesOf(s *store.Store, qualifiedName string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	opt.Apply(options, opts)
	if err := requireSymbol(s, qualifiedName); err != nil {
		return nil, err
	}
	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = allEdgeTypes
	}
	edges, err := traverse(s, qualifiedName, true, options.Depth, allowTypes)
	if err != nil {
		return nil, err
	}
	if err := markExternal(s, edges); err != nil {
		return nil, err
	}
	return filterEdges(edges, options.Repo), nil
}

// markExternal flags edges whose far endpoint is not in the index, so a reader
// can tell "this call goes to stdlib or a dependency" from "this symbol is in
// the project". Without it every graph read ends in a follow-up show that
// errors, and the caller has to know Go's stdlib by heart to interpret a ref.
// The check is one batched lookup per call, not one per edge.
func markExternal(s *store.Store, edges []EdgeDetail) error {
	if len(edges) == 0 {
		return nil
	}
	names := make([]string, 0, len(edges))
	for _, e := range edges {
		if e.ToRef != "" {
			names = append(names, e.ToRef)
		}
	}
	if len(names) == 0 {
		return nil
	}
	known, err := s.SymbolsByNames(names)
	if err != nil {
		return fmt.Errorf("mark external refs: %w", err)
	}
	present := make(map[string]bool, len(known))
	for _, sym := range known {
		present[sym.QualifiedName] = true
	}
	for i := range edges {
		edges[i].External = edges[i].ToRef != "" && !present[edges[i].ToRef]
	}
	return nil
}

// requireSymbol rejects an unknown start node. Without it a typo (or a
// receiver that does not exist, e.g. Store.f for a free function f) traverses
// from a node that has no edges and returns an empty list, which reads as
// "no callers" instead of "no such symbol".
func requireSymbol(s *store.Store, qualifiedName string) error {
	if _, err := s.SymbolByName(qualifiedName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return symbolMissError(s, qualifiedName)
		}
		return err
	}
	return nil
}

func Show(s *store.Store, qualifiedName string, opts ...Option) (*ShowResult, error) {
	options := &Options{}
	opt.Apply(options, opts)

	decls, err := s.SymbolsByNameExact(qualifiedName, options.IncludeTests)
	if err != nil {
		return nil, err
	}
	if len(decls) == 0 {
		return nil, symbolMissError(s, qualifiedName)
	}
	sym := decls[0]
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
			Fields:        sym.Fields,
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
			Generated:     sym.IsGenerated,
			Repo:          sym.Repo,
		},
		Declarations:  len(decls),
		IncomingEdges: []EdgeDetail{},
		OutgoingEdges: []EdgeDetail{},
	}

	for _, e := range incoming {
		result.IncomingEdges = append(result.IncomingEdges, edgeToDetail(e))
	}

	for _, e := range outgoing {
		result.OutgoingEdges = append(result.OutgoingEdges, edgeToDetail(e))
	}
	if err := markExternal(s, result.OutgoingEdges); err != nil {
		return nil, err
	}

	return result, nil
}

func Overview(s *store.Store, opts ...Option) (*OverviewResult, error) {
	options := &Options{}
	opt.Apply(options, opts)

	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}

	result := &OverviewResult{Repos: []RepoSummary{}}

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

		summary, err := buildPackageSummary(s, p, options.IncludeTests, options.IncludeSymbols, importCounts)
		if err != nil {
			return nil, err
		}
		result.Packages = append(result.Packages, summary)
	}

	result.TotalPackages = len(result.Packages)
	// Totals are index-wide, matching reindex and health. Summing the exported
	// rows per package reported roughly a third of the index and made the two
	// tools disagree about how big the repo is.
	_, totalSymbols, totalEdges, err := s.Counts()
	if err != nil {
		return nil, fmt.Errorf("overview: counting rows: %w", err)
	}
	result.TotalSymbols = totalSymbols
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

func buildPackageSummary(s *store.Store, p store.Package, includeTests, includeSymbols bool, importCounts map[string]int) (PackageSummary, error) {
	summary := PackageSummary{
		Path:        p.Path,
		Name:        p.Name,
		Doc:         p.Doc,
		Repo:        p.Repo,
		ImportCount: importCounts[p.Path],
		SymCount:    p.SymCount,
	}
	if !includeSymbols {
		return summary, nil
	}

	syms, err := s.SymbolsByPackage(p.Path, includeTests)
	if err != nil {
		return PackageSummary{}, fmt.Errorf("overview: symbols for %s: %w", p.Path, err)
	}
	summary.Symbols = symbolDetails(syms, false, false)
	return summary, nil
}

// symbolDetails converts store symbols to query details, keeping only exported
// symbols unless includeUnexported is set. Fields are attached only on request.
func symbolDetails(syms []store.Symbol, includeUnexported, includeFields bool) []SymbolDetail {
	symbols := make([]SymbolDetail, 0, len(syms))
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
			Fields:        fieldsIfRequested(sym, includeFields),
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
			Generated:     sym.IsGenerated,
			Repo:          sym.Repo,
		})
	}
	return symbols
}

func fieldsIfRequested(sym store.Symbol, include bool) []store.SymbolField {
	if !include {
		return nil
	}
	return sym.Fields
}

// searchResultsFromSymbols projects store rows into search results, attaching
// fields only when the caller asked for them.
func searchResultsFromSymbols(syms []store.Symbol, includeFields bool) []SearchResult {
	result := make([]SearchResult, 0, len(syms))
	for _, sym := range syms {
		result = append(result, SearchResult{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Signature:     sym.Signature,
			Doc:           sym.Doc,
			Receiver:      sym.Receiver,
			PosFile:       sym.PosFile,
			Fields:        fieldsIfRequested(sym, includeFields),
			PosLine:       sym.PosLine,
			Exported:      sym.Exported,
			Generated:     sym.IsGenerated,
			Repo:          sym.Repo,
		})
	}
	return result
}

func Search(s *store.Store, pattern string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	opt.Apply(options, opts)

	var syms []store.Symbol
	var err error
	if options.File != "" {
		syms, err = s.SearchSymbolsByFile(options.File, "", nil, options.IncludeTests, options.Generated)
		if err == nil {
			syms = filterSymbolsByPattern(syms, pattern)
		}
	} else {
		syms, err = s.SearchSymbols(pattern, "", nil, options.Package, options.IncludeTests, options.Generated)
	}
	if err != nil {
		return nil, err
	}

	// Rank the matches before projecting them, so the ordering keys (test and
	// generated provenance) do not have to be carried on the result payload.
	sortSymbolsForSearch(syms, strings.ToLower(pattern))

	result := searchResultsFromSymbols(syms, options.IncludeFields)

	return filterSearchResults(filterSearchResultsByOptions(result, options), options.Repo), nil
}

// filterSearchResultsByOptions applies the kind, exported and file filters to a
// projected result set. Every search mode goes through it, so an argument the
// caller passed is never silently dropped; prefix and method mode used to
// ignore kind and exported entirely.
func filterSearchResultsByOptions(results []SearchResult, options *Options) []SearchResult {
	if options.Kind == "" && options.Exported == nil && options.File == "" {
		return results
	}
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		if !kindMatches(r.Kind, options.Kind) {
			continue
		}
		if options.Exported != nil && r.Exported != *options.Exported {
			continue
		}
		if options.File != "" && !strings.Contains(r.PosFile, options.File) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// kindMatches reports whether a symbol kind satisfies a kind filter. "type"
// also accepts interfaces and aliases: both name a type, and excluding them
// made a type lookup miss a live alias with no hint that it existed under
// another kind. A filter naming interface or alias stays exact.
func kindMatches(kind, want string) bool {
	if want == "" {
		return true
	}
	if kind == want {
		return true
	}
	return want == kindType && (kind == kindInterface || kind == kindAlias)
}

// filterSymbolsByPattern applies the pattern to symbols already narrowed by a
// file filter. The file query only knows the path, so without this the pattern
// was silently dropped and a scoped search returned every symbol in the file.
// Matching mirrors the FTS columns: name, qualified name, signature and doc.
func filterSymbolsByPattern(syms []store.Symbol, pattern string) []store.Symbol {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return syms
	}
	out := make([]store.Symbol, 0, len(syms))
	for _, sym := range syms {
		if symbolMatchesPattern(sym, pattern) {
			out = append(out, sym)
		}
	}
	return out
}

func symbolMatchesPattern(sym store.Symbol, patternLower string) bool {
	for _, field := range []string{sym.Name, sym.QualifiedName, sym.Signature, sym.Doc} {
		if strings.Contains(strings.ToLower(field), patternLower) {
			return true
		}
	}
	return false
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
	case kindInterface, kindType, kindAlias:
		return 1
	default:
		return 2
	}
}

// sortSymbolsForSearch orders search matches by name-match tier, then exported,
// then kind, and finally pushes test and generated symbols after non-test,
// non-generated ones at the same rank. Down-ranking never removes a match.
func sortSymbolsForSearch(syms []store.Symbol, patternLower string) {
	// Tiers are keyed by qualified name, not by slice position: sort.SliceStable
	// swaps elements, so a position-keyed cache would compare the wrong tiers.
	// The tier takes the better of the bare name and the qualified name, so an
	// exact name match (config.Config for "Config") outranks a doc match.
	tiers := make(map[string]int, len(syms))
	for _, sym := range syms {
		tiers[sym.QualifiedName] = min(
			matchTier(strings.ToLower(sym.Name), patternLower),
			matchTier(strings.ToLower(sym.QualifiedName), patternLower),
		)
	}
	sort.SliceStable(syms, func(i, j int) bool {
		a := &syms[i]
		b := &syms[j]

		if aTier, bTier := tiers[a.QualifiedName], tiers[b.QualifiedName]; aTier != bTier {
			return aTier < bTier
		}

		if a.Exported != b.Exported {
			return a.Exported
		}

		if aKind, bKind := kindPriority(a.Kind), kindPriority(b.Kind); aKind != bKind {
			return aKind < bKind
		}

		if a.IsTest != b.IsTest {
			return !a.IsTest
		}

		if a.IsGenerated != b.IsGenerated {
			return !a.IsGenerated
		}

		return false
	})
}

// filterGenerated applies the generated filter to results already fetched from
// the store. It is used by the search modes whose store queries do not carry the
// filter.
func filterGenerated(results []SearchResult, filter store.GeneratedFilter) []SearchResult {
	if filter != store.GeneratedExclude && filter != store.GeneratedOnly {
		return results
	}
	keepGenerated := filter == store.GeneratedOnly
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		if r.Generated == keepGenerated {
			out = append(out, r)
		}
	}
	return out
}

// resolveTypeName maps a caller's type name to the qualified name it declares.
// Both forms are accepted, because a caller has no way to know which one the
// tool it is about to call expects: "MessageStore" and
// "emak/pkg/mqueue.MessageStore" name the same type. An unresolvable name is an
// error, never an empty result — "no implementers" and "no such type" are
// different answers.
func resolveTypeName(s *store.Store, typeName string) (string, error) {
	pkgPath, short := splitTypeQualifier(typeName)
	decls, err := s.TypeDeclarationsNamed(short, true)
	if err != nil {
		return "", err
	}
	if pkgPath != "" {
		decls = filterByPackage(decls, pkgPath)
	}
	if len(decls) == 0 {
		return "", fmt.Errorf("type not found: %s", typeName)
	}
	if len(decls) > 1 {
		return "", fmt.Errorf("type not found: %s is declared in %d packages (%s, ...); qualify it",
			typeName, len(decls), decls[0].PackagePath)
	}
	return decls[0].QualifiedName, nil
}

func MethodsOf(s *store.Store, typeName string, opts ...Option) ([]SymbolDetail, error) {
	options := &Options{}
	opt.Apply(options, opts)

	resolved, err := resolveTypeName(s, typeName)
	if err != nil {
		return nil, err
	}
	// The resolved name is exact, so a qualified query cannot pick up the
	// methods of a same-named type in another package.
	syms, err := s.MethodsByQualifiedReceiver(resolved, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	if len(syms) == 0 {
		return []SymbolDetail{}, nil
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
			Generated:     sym.IsGenerated,
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
	Path        string         `json:"path"`
	Name        string         `json:"name"`
	Doc         string         `json:"doc"`
	Repo        string         `json:"repo,omitempty"`
	Symbols     []SymbolDetail `json:"symbols"`
	ImportCount int            `json:"import_count"`
}

func Package(s *store.Store, pkgPath string, opts ...Option) (*PackageResult, error) {
	options := &Options{}
	opt.Apply(options, opts)

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

	importCounts, err := s.ImportCounts()
	if err != nil {
		return nil, fmt.Errorf("package %s: import counts: %w", pkgPath, err)
	}

	return &PackageResult{
		Path:        found.Path,
		Name:        found.Name,
		Doc:         found.Doc,
		ImportCount: importCounts[pkgPath],
		Symbols:     symbolDetails(syms, options.IncludeUnexported, options.IncludeFields),
		Repo:        found.Repo,
	}, nil
}

func EdgesByType(s *store.Store, edgeType string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

	allowTypes := options.EdgeTypes
	if len(allowTypes) == 0 {
		allowTypes = []string{edgeTypeImports}
	}

	edges, err := s.EdgesTo(pkgPath)
	if err != nil {
		return nil, err
	}

	result := filterEdgesByType(dropTestOnlyImports(edges), allowTypes)
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

// dropTestOnlyImports removes import edges whose only sites are _test.go files,
// so "who imports this package" answers with the runtime dependency surface and
// agrees with ImportsOf. Without it a package imported solely by a test showed
// up as an importer while its own outgoing edges omitted the same import.
func dropTestOnlyImports(edges []store.Edge) []store.Edge {
	out := edges[:0]
	for _, e := range edges {
		if e.EdgeType == edgeTypeImports && !dropTestImportSites(&e) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// runtimeImportEdges loads every import edge with its test-only sites removed.
// Every import reader goes through it so one package's imports cannot differ
// between imports_of, transitive imports, dependency_flow and the hub counts.
func runtimeImportEdges(s *store.Store) ([]store.Edge, error) {
	edges, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, err
	}
	return dropTestOnlyImports(edges), nil
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
	allImports, err := runtimeImportEdges(s)
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
	opt.Apply(options, opts)

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
		if !edgeTypeAllowed(e.EdgeType, allowTypes) {
			continue
		}
		if e.EdgeType == edgeTypeImports && !dropTestImportSites(&e) {
			continue
		}
		d := edgeToDetail(e)
		if e.EdgeType == edgeTypeImports && !indexed[e.ToRef] {
			if targetModule := modulePathForRef(e.ToRef, knownModules); targetModule != "" {
				d.State = "not indexed"
			}
		}
		result = append(result, d)
	}
	return filterEdges(result, options.Repo), nil
}

// dropTestImportSites removes the _test.go sites from an import edge and
// reports whether any remain. A test-only import is not part of the package's
// runtime dependency surface, so listing it made the graph say emak/config
// imports testing. An import used by both keeps its non-test sites.
func dropTestImportSites(e *store.Edge) bool {
	kept := make([]store.Site, 0, len(e.Sites))
	for _, site := range e.Sites {
		if !strings.HasSuffix(site.File, "_test.go") {
			kept = append(kept, site)
		}
	}
	if len(kept) == 0 {
		return false
	}
	e.Sites = kept
	e.SiteCount = len(kept)
	e.PosFile = kept[0].File
	e.PosLine = kept[0].Line
	return true
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
	opt.Apply(options, opts)

	syms, err := s.SearchByQualifiedNamePrefix(prefix, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	sortSymbolsForSearch(syms, strings.ToLower(prefix))

	result := searchResultsFromSymbols(syms, options.IncludeFields)
	result = filterSearchResultsByOptions(filterGenerated(result, options.Generated), options)

	return filterSearchResults(result, options.Repo), nil
}

func TransitiveImports(s *store.Store, pkgPath string, opts ...Option) ([]EdgeDetail, error) {
	options := &Options{}
	opt.Apply(options, opts)

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

	allImports, err := runtimeImportEdges(s)
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

// mentionsTypeAs reports whether text uses name as a type: bare, or qualified
// by one of the packages that declare it. "gitlab.Response" and "FirstRun bool"
// must not count for Response and Run.
func mentionsTypeAs(text, name string, pkgs map[string]bool) bool {
	for start := 0; ; {
		i := strings.Index(text[start:], name)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(name)
		if (i == 0 || !isIdentChar(text[i-1])) && (end == len(text) || !isIdentChar(text[end])) {
			if i == 0 || text[i-1] != '.' || qualifiedByKnownPkg(text[:i-1], pkgs) {
				return true
			}
		}
		start = i + 1
	}
}

// qualifiedByKnownPkg reports whether the text before a dot ends with one of
// the declaring package names.
func qualifiedByKnownPkg(prefix string, pkgs map[string]bool) bool {
	for name := range pkgs {
		if strings.HasSuffix(prefix, name) {
			return true
		}
	}
	return false
}

func fieldsMentionTypeAs(fields []store.SymbolField, name string, pkgs map[string]bool) bool {
	for _, f := range fields {
		if mentionsTypeAs(f.GoType, name, pkgs) {
			return true
		}
	}
	return false
}

func isIdentChar(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// TypeUsage returns the symbols whose signatures or fields use the named type.
// A short name is resolved to its declaration first: matching a bare string
// against signatures reported "*gitlab.Response" and "fantasy.ToolResponse" for
// "Response", and matched field names like "FirstRun" for "Run". A qualified
// name must name a package the index knows, so a stale path errors instead of
// silently answering about a different package.
func TypeUsage(s *store.Store, typeName string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	opt.Apply(options, opts)

	pkgPath, short := splitTypeQualifier(typeName)
	decls, err := s.TypeDeclarationsNamed(short, options.IncludeTests)
	if err != nil {
		return nil, err
	}
	if len(decls) == 0 {
		return nil, fmt.Errorf("type not found: %s", short)
	}

	// Ambiguity is judged before narrowing to one package: two packages
	// declaring the same name make a bare mention undecidable, and a qualified
	// query still has to tell the two apart.
	ambiguous := len(decls) > 1
	if pkgPath != "" {
		decls = filterByPackage(decls, pkgPath)
		if len(decls) == 0 {
			return nil, fmt.Errorf("type not found: %s", typeName)
		}
	}

	names := make(map[string]bool, len(decls))
	pkgs := make(map[string]bool, len(decls))
	for _, d := range decls {
		names[path.Base(d.PackagePath)] = true
		pkgs[d.PackagePath] = true
	}

	// With the name declared more than once, a bare mention belongs to whichever
	// declaring package the using package imports, and a qualified query must
	// still exclude the other declaration's users.
	var imports map[string]map[string]bool
	if ambiguous {
		imports, err = packageImports(s)
		if err != nil {
			return nil, err
		}
	}

	users, err := signatureUsers(s, short, pkgs, names, imports, options.IncludeTests)
	if err != nil {
		return nil, err
	}
	embedders, err := embeddingSymbols(s, short, pkgs, options.IncludeTests)
	if err != nil {
		return nil, err
	}
	// A type is also "used" by code that names it outside a signature: a method
	// receiver, a composite literal, a type assertion. show already reports those
	// as incoming references, so type_usage answers from the same set instead of
	// being narrower than show for the same question.
	referrers, err := referencingSymbols(s, decls, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	merged := mergeUsage(users, embedders, referrers)
	return filterSearchResults(searchResultsFromSymbols(merged, false), options.Repo), nil
}

// mergeUsage concatenates usage lists, keeping one row per qualified name. The
// same symbol can reach a type through a signature and through a reference.
func mergeUsage(lists ...[]store.Symbol) []store.Symbol {
	var total int
	for _, l := range lists {
		total += len(l)
	}
	seen := make(map[string]bool, total)
	merged := make([]store.Symbol, 0, total)
	for _, l := range lists {
		for _, sym := range l {
			if seen[sym.QualifiedName] {
				continue
			}
			seen[sym.QualifiedName] = true
			merged = append(merged, sym)
		}
	}
	return merged
}

// referencingSymbols returns the symbols with a reference edge to one of the
// type's declarations. Receivers, composite literals and type assertions reach
// the type this way and through no signature, so without it type_usage missed
// live types that show reported three incoming references for.
func referencingSymbols(s *store.Store, decls []store.Symbol, includeTests bool) ([]store.Symbol, error) {
	names := make([]string, 0, len(decls))
	for _, d := range decls {
		names = append(names, d.QualifiedName)
	}
	edges, err := s.EdgesForNodes(names, []string{edgeTypeReferences, edgeTypeCalls}, false)
	if err != nil {
		return nil, fmt.Errorf("type usage: references: %w", err)
	}
	froms := make([]string, 0, len(edges))
	for _, e := range edges {
		if e.EdgeType == edgeTypeReferences || e.EdgeType == edgeTypeCalls {
			froms = append(froms, e.FromRef)
		}
	}
	if len(froms) == 0 {
		return nil, nil
	}
	syms, err := s.SymbolsByNames(froms)
	if err != nil {
		return nil, fmt.Errorf("type usage: referencing symbols: %w", err)
	}
	out := make([]store.Symbol, 0, len(syms))
	for _, sym := range syms {
		if !includeTests && sym.IsTest {
			continue
		}
		out = append(out, sym)
	}
	return out, nil
}

// splitTypeQualifier splits "pkg/path.Type" into its package path and bare
// name. The split is at the last dot: a module path contains dots of its own
// ("example.com/p.Widget"), so a first-dot cut yields a bogus package.
func splitTypeQualifier(typeName string) (string, string) {
	pkgPath, short, ok := strings.CutLast(typeName, ".")
	if !ok {
		return "", typeName
	}
	return pkgPath, short
}

func filterByPackage(syms []store.Symbol, pkgPath string) []store.Symbol {
	out := make([]store.Symbol, 0, len(syms))
	for _, sym := range syms {
		if sym.PackagePath == pkgPath {
			out = append(out, sym)
		}
	}
	return out
}

// signatureUsers returns the symbols whose signature or fields use the type.
// A mention qualifies when it is bare or carries one of the declaring
// packages' names, so "types.ServerSync" counts and "gitlab.Response" does not
// for Response. When imports is non-nil the name is declared more than once,
// so a bare mention only counts inside a package that imports a declaring one.
func signatureUsers(s *store.Store, short string, declaringPkg, names map[string]bool, imports map[string]map[string]bool, includeTests bool) ([]store.Symbol, error) {
	syms, err := s.SearchByType(short, includeTests)
	if err != nil {
		return nil, err
	}
	out := make([]store.Symbol, 0, len(syms))
	for _, sym := range syms {
		if !signatureUsesType(sym, short, declaringPkg, names, imports) {
			continue
		}
		out = append(out, sym)
	}
	return out, nil
}

func signatureUsesType(sym store.Symbol, short string, declaringPkg, names map[string]bool, imports map[string]map[string]bool) bool {
	if !mentionsTypeAs(sym.Signature, short, names) && !fieldsMentionTypeAs(sym.Fields, short, names) {
		return false
	}
	if imports == nil {
		return true
	}
	// A declaring package uses its own type bare; any other package must name
	// it qualified or import a declaring one.
	if declaringPkg[sym.PackagePath] {
		return true
	}
	return mentionsQualified(sym.Signature, short, names) || importsDeclaring(sym.PackagePath, names, imports)
}

// mentionsQualified reports whether text names the type behind one of the
// declaring packages' names, which settles an ambiguous short name.
func mentionsQualified(text, name string, pkgs map[string]bool) bool {
	for pkg := range pkgs {
		if strings.Contains(text, pkg+"."+name) {
			return true
		}
	}
	return false
}

// importsDeclaring reports whether pkgPath imports any package whose name is
// one of pkgs. Two packages can share a name, so the import's own last element
// is what a bare mention would resolve to.
func importsDeclaring(pkgPath string, names map[string]bool, imports map[string]map[string]bool) bool {
	for imp := range imports[pkgPath] {
		if names[path.Base(imp)] {
			return true
		}
	}
	return false
}

// packageImports maps each package path to the import paths it declares.
func packageImports(s *store.Store) (map[string]map[string]bool, error) {
	edges, err := runtimeImportEdges(s)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]bool, len(edges))
	for _, e := range edges {
		if out[e.FromRef] == nil {
			out[e.FromRef] = make(map[string]bool)
		}
		out[e.FromRef][e.ToRef] = true
	}
	return out, nil
}

// embedTargetInPkgs reports whether an embeds edge target names typeName in one
// of the packages that declare it. The target is "<pkg>.<Type>", so the package
// is everything before the last dot.
func embedTargetInPkgs(toRef, typeName string, pkgs map[string]bool) bool {
	if !strings.HasSuffix(toRef, "."+typeName) {
		return false
	}
	return pkgs[strings.TrimSuffix(toRef, "."+typeName)]
}

// embeddingSymbols returns the structs that embed typeName. The signature scan
// in SearchByType cannot see these: an embedded field ends in ";" or a newline,
// neither of which is a signature boundary it matches on, so a type used only
// through embedding reported zero usages while blast_radius counted its
// embedders correctly.
func embeddingSymbols(s *store.Store, typeName string, pkgs map[string]bool, includeTests bool) ([]store.Symbol, error) {
	edges, err := s.EdgesByType(edgeTypeEmbeds)
	if err != nil {
		return nil, err
	}

	suffix := "." + typeName
	var refs []string
	seen := make(map[string]bool)
	for _, e := range edges {
		if e.ToRef != typeName && !strings.HasSuffix(e.ToRef, suffix) {
			continue
		}
		if !embedTargetInPkgs(e.ToRef, typeName, pkgs) {
			continue
		}
		if seen[e.FromRef] {
			continue
		}
		seen[e.FromRef] = true
		refs = append(refs, e.FromRef)
	}

	syms, err := s.SymbolsByNames(refs)
	if err != nil {
		return nil, err
	}
	if includeTests {
		return syms, nil
	}
	out := make([]store.Symbol, 0, len(syms))
	for _, sym := range syms {
		if !sym.IsTest {
			out = append(out, sym)
		}
	}
	return out, nil
}

type PathStep struct {
	From     string `json:"from"`
	To       string `json:"to"`
	EdgeType string `json:"edge_type"`
	PosFile  string `json:"pos_file"`
	Repo     string `json:"repo"`
	PosLine  int    `json:"pos_line"`
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
	opt.Apply(options, opts)
	if maxDepth <= 0 {
		maxDepth = 10
	}

	// Reject a mistyped endpoint the same way callers_of does. Returning
	// "no path" for a name that does not exist reads as "these are unrelated".
	if err := requireSymbol(s, from); err != nil {
		return nil, err
	}
	if err := requireSymbol(s, to); err != nil {
		return nil, err
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

// expandPathNode enqueues every out-edge of current (respecting a repo scope)
// plus forward cross-repo contract edges as the next BFS frontier.
//
// Only forward edges are followed. Traversing in-edges produced chains that
// looked contiguous but were not reachability: "A reaches B" was answered with
// a hop that only exists as "B references A", and paths were routed through
// type nodes that cannot call anything.
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

	for _, c := range contracts {
		if c.Suggested || c.FromRef != current.name {
			continue
		}
		if !repoMismatch(c.Repo, repo) {
			queue = enqueueIfUnvisited(c.ToRef, contractToEdge(c), current.path, visited, queue)
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
	opt.Apply(options, opts)

	syms, err := s.MethodsByName(methodName, options.IncludeTests)
	if err != nil {
		return nil, err
	}

	sortSymbolsForSearch(syms, strings.ToLower(methodName))

	result := searchResultsFromSymbols(syms, options.IncludeFields)
	result = filterSearchResultsByOptions(filterGenerated(result, options.Generated), options)

	return filterSearchResults(result, options.Repo), nil
}

type InterfaceImpl struct {
	Interface  string `json:"interface"`
	ImplType   string `json:"impl_type"`
	ImplMethod string `json:"impl_method"`
	PosFile    string `json:"pos_file"`
	PosLine    int    `json:"pos_line"`
}

func InterfaceImplementations(s *store.Store, interfaceName string, opts ...Option) ([]InterfaceImpl, error) {
	resolved, err := resolveTypeName(s, interfaceName)
	if err != nil {
		return nil, err
	}
	interfaceName = resolved

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

	methodNames, err := interfaceMethodNames(s, interfaceName)
	if err != nil {
		return nil, err
	}

	results := make([]InterfaceImpl, 0)
	for _, implType := range implTypes {
		methods, err := s.MethodsByQualifiedReceiver(implType, true)
		if err != nil {
			continue
		}
		for _, m := range methods {
			if !methodNames[m.Name] {
				continue
			}
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

// interfaceMethodNames collects the method names an interface declares,
// following embedded interfaces so a composed interface reports every method
// its implementers must supply. Only these names belong in an implementation
// report; the implementer's other methods are unrelated surface.
//
// Test symbols are included: an interface declared in a _test.go file has its
// methods indexed as test symbols, and excluding them made interface_impls
// report zero implementations for such an interface while all_edges reported
// the satisfies edge.
func interfaceMethodNames(s *store.Store, interfaceName string) (map[string]bool, error) {
	names := make(map[string]bool)
	visited := map[string]bool{interfaceName: true}
	queue := []string{interfaceName}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		methods, err := s.MethodsByQualifiedReceiver(current, true)
		if err != nil {
			return nil, err
		}
		for _, m := range methods {
			names[m.Name] = true
		}

		edges, err := s.EdgesFrom(current)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			if e.EdgeType != edgeTypeEmbeds || visited[e.ToRef] {
				continue
			}
			visited[e.ToRef] = true
			queue = append(queue, e.ToRef)
		}
	}
	return names, nil
}

// symbolMissError turns a lookup miss into an actionable error with near-name
// candidates so agents recover without a blind second guess.
func symbolMissError(s *store.Store, qualifiedName string) error {
	short := qualifiedName
	if i := strings.LastIndexAny(short, "./"); i >= 0 {
		short = short[i+1:]
	}
	pkgPrefix := ""
	if before, _, ok := strings.CutLast(qualifiedName, "."); ok {
		pkgPrefix = before
	}

	candidates := symbolCandidates(s, short, pkgPrefix)
	if len(candidates) == 0 {
		return fmt.Errorf("symbol not found: %s", qualifiedName)
	}
	return fmt.Errorf("symbol not found: %s; did you mean: %s",
		qualifiedName, strings.Join(candidates, ", "))
}

// symbolCandidates prefers the short-name search and falls back to a
// qualified-name prefix search when the short name matches nothing (e.g. a
// package-path-only input).
func symbolCandidates(s *store.Store, short, pkgPrefix string) []string {
	syms, err := s.SearchSymbols(short, "", nil, "", false, store.GeneratedAny)
	if err != nil || len(syms) == 0 {
		syms, err = s.SearchByQualifiedNamePrefix(short, false)
		if err != nil {
			return nil
		}
	}
	return rankSymbolCandidates(syms, short, pkgPrefix, 5)
}

func rankSymbolCandidates(syms []store.Symbol, short, pkgPrefix string, limit int) []string {
	seen := make(map[string]bool, len(syms))
	ranked := make([]store.Symbol, 0, len(syms))
	for _, sym := range syms {
		if seen[sym.QualifiedName] {
			continue
		}
		seen[sym.QualifiedName] = true
		ranked = append(ranked, sym)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return symbolCandidateRank(ranked[i], short, pkgPrefix) < symbolCandidateRank(ranked[j], short, pkgPrefix)
	})

	names := make([]string, 0, limit)
	for _, sym := range ranked {
		if len(names) == limit {
			break
		}
		names = append(names, sym.QualifiedName)
	}
	return names
}

// symbolCandidateRank orders exact short-name matches first, then candidates in
// the package prefix the caller supplied, then everything else.
func symbolCandidateRank(sym store.Symbol, short, pkgPrefix string) int {
	switch {
	case sym.Name == short:
		return 0
	case pkgPrefix != "" && strings.HasPrefix(sym.QualifiedName, pkgPrefix+"."):
		return 1
	default:
		return 2
	}
}

type UnusedSymbol struct {
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Signature     string `json:"signature"`
	PosFile       string `json:"pos_file"`
	PosLine       int    `json:"pos_line"`
}

// reachableSymbols returns every qualified name reachable from the live root
// set by following outgoing calls/references/satisfies/embeds edges.
//
// This is what separates "nobody calls it" from "only dead code calls it":
// a symbol referenced solely by an unreachable function is itself unreachable.
// The roots are the declarations that keep a program running regardless of
// whether anything calls them: the Go runtime's own entry points (main, init),
// test entry points, and every exported symbol, since a consumer outside the
// indexed set may call it.
func reachableSymbols(s *store.Store, allSyms []store.Symbol) (map[string]bool, error) {
	pkgMain, err := mainPackages(s)
	if err != nil {
		return nil, err
	}
	roots, err := rootSymbols(s, allSyms, pkgMain)
	if err != nil {
		return nil, err
	}
	return s.ReachableFrom(roots, liveEdgeTypes)
}

// rootSymbols collects the names that are live without being called: the
// runtime's main/init, exported symbols (their caller may be outside the
// index), and test entry points.
//
// Test roots come from a fresh AllSymbols(true) scan rather than the caller's
// slice, because callers commonly pass includeTests=false for the report while
// test functions still appear in the edge graph. Leaving them out as roots
// would make every test-only helper look unreachable.
func rootSymbols(s *store.Store, allSyms []store.Symbol, pkgMain map[string]bool) ([]string, error) {
	roots := make([]string, 0, len(allSyms))
	for _, sym := range allSyms {
		if sym.Exported || liveByDesign(sym, pkgMain) {
			roots = append(roots, sym.QualifiedName)
		}
	}

	all, err := s.AllSymbols(true)
	if err != nil {
		return nil, err
	}
	for _, sym := range all {
		if sym.IsTest && sym.Receiver == "" && isTestEntryName(sym.Name) {
			roots = append(roots, sym.QualifiedName)
		}
	}
	return roots, nil
}

// liveEdgeTypes are the edge types that carry liveness: anything an outgoing
// reference, call, embedding, or interface satisfaction points at is reachable.
var liveEdgeTypes = []string{edgeTypeCalls, edgeTypeReferences, edgeTypeSatisfies, edgeTypeEmbeds}

func UnusedSymbols(s *store.Store, opts ...Option) ([]UnusedSymbol, error) {
	options := &Options{}
	opt.Apply(options, opts)

	allSyms, err := s.AllSymbols(options.IncludeTests)
	if err != nil {
		return nil, err
	}

	reachable, err := reachableSymbols(s, allSyms)
	if err != nil {
		return nil, err
	}

	pkgMain, err := mainPackages(s)
	if err != nil {
		return nil, err
	}

	unused := make([]UnusedSymbol, 0)
	interfaceNames := interfaceQualifiedNames(allSyms)
	dispatched, err := interfaceDispatchedMethods(s, allSyms, reachable)
	if err != nil {
		return nil, err
	}
	for _, sym := range allSyms {
		if sym.Exported || sym.IsGenerated {
			continue
		}
		if liveByDesign(sym, pkgMain) {
			continue
		}
		// An interface method is a declaration implementers satisfy, not a
		// callable that needs a caller; removing it breaks every implementer.
		if sym.Kind == kindMethod && interfaceNames[strings.TrimSuffix(sym.QualifiedName, "."+sym.Name)] {
			continue
		}
		if !reachable[sym.QualifiedName] && !dispatched[sym.QualifiedName] {
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

// interfaceDispatchedMethods returns the concrete methods reachable only through
// an interface. A call to iface.Method is recorded against the interface's
// declaration, so without this every implementer's same-named method looks
// uncalled — and deleting one would break a live call path.
func interfaceDispatchedMethods(s *store.Store, allSyms []store.Symbol, reachable map[string]bool) (map[string]bool, error) {
	edges, err := s.EdgesByType(edgeTypeSatisfies)
	if err != nil {
		return nil, err
	}

	implementers := make(map[string][]string)
	for _, e := range edges {
		implementers[e.ToRef] = append(implementers[e.ToRef], e.FromRef)
	}

	dispatched := make(map[string]bool)
	for _, sym := range allSyms {
		if sym.Kind != kindMethod || !reachable[sym.QualifiedName] {
			continue
		}
		iface := strings.TrimSuffix(sym.QualifiedName, "."+sym.Name)
		for _, impl := range implementers[iface] {
			dispatched[impl+"."+sym.Name] = true
		}
	}
	return dispatched, nil
}

type Cycle struct {
	Path []string `json:"path"`
	// Truncated is set on the last element of a truncated result: the graph
	// contains more distinct cycles than the returned set (limited by maxCycles).
	Truncated bool `json:"truncated"`
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

// Diagnostics returns indexed type-check/parse errors, optionally narrowed to
// one file (suffix match as passed) and one workspace repo.
func Diagnostics(s *store.Store, file string, opts ...Option) ([]store.Diagnostic, error) {
	options := &Options{}
	opt.Apply(options, opts)
	diags, err := s.Diagnostics(file)
	if err != nil {
		return nil, err
	}
	if options.Repo == "" {
		return diags, nil
	}
	out := make([]store.Diagnostic, 0, len(diags))
	for _, d := range diags {
		if d.Repo == options.Repo {
			out = append(out, d)
		}
	}
	return out, nil
}

// SymbolsInFile returns every symbol defined in the given file (path or
// substring, following the store's file matching).
func SymbolsInFile(s *store.Store, filePath string, opts ...Option) ([]SearchResult, error) {
	options := &Options{}
	opt.Apply(options, opts)
	syms, err := s.SearchSymbolsByFile(filePath, "", nil, false, store.GeneratedAny)
	if err != nil {
		return nil, err
	}

	result := searchResultsFromSymbols(syms, false)
	return filterSearchResults(result, options.Repo), nil
}

type BlastRadius struct {
	Symbol            string `json:"symbol"`
	DirectCallers     int    `json:"direct_callers"`
	TransitiveCallers int    `json:"transitive_callers"`
	Implementations   int    `json:"implementations"`
	Embedders         int    `json:"embedders"`
	TypeUsers         int    `json:"type_users"`
}

// errSymbolMissing distinguishes a non-existent symbol from a query failure in
// blast-radius lookups; internal consumers (changed_symbols) tolerate it for
// symbols that were removed from the index.
var errSymbolMissing = errors.New("symbol not found")

func GetBlastRadius(s *store.Store, qualifiedName string, depth int, opts ...Option) (*BlastRadius, error) {
	options := &Options{}
	opt.Apply(options, opts)
	if depth <= 0 {
		depth = 3
	}

	// Verify the symbol exists so an empty blast radius for a typo'd or removed
	// symbol is never mistaken for a genuine (empty) result.
	target, err := s.SymbolByName(qualifiedName)
	if err != nil {
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

	// Only a declared type has signature users. A function or const has none,
	// and asking TypeUsage for it now errors instead of returning the substring
	// noise the loose matcher produced.
	typeUsers := 0
	if target.Kind == kindType || target.Kind == kindInterface || target.Kind == kindAlias {
		users, err := TypeUsage(s, qualifiedName, opts...)
		if err != nil {
			return nil, err
		}
		typeUsers = len(users)
	}

	return &BlastRadius{
		Symbol:            qualifiedName,
		DirectCallers:     countDirectCallers(directCallers, options.Repo),
		TransitiveCallers: len(transitiveCallers),
		Implementations:   implCount,
		Embedders:         embedCount,
		TypeUsers:         typeUsers,
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
	opt.Apply(options, opts)

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
	opt.Apply(options, opts)

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
	node         ast.Node
	startOff     int
	endOff       int
	docStartOff  int
	groupMembers int
	partOfGroup  bool
	// stale records that the exact-line match failed and the declaration was
	// re-located by name, so the caller knows the index was behind.
	stale bool
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
		node:        d,
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
		node:         d,
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
		node:         d,
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

// declSpan is a declaration located in a source file: the file's bytes, the
// declaration node, and the offsets a caller needs to read or rewrite it.
type declSpan struct {
	fset         *token.FileSet
	node         ast.Node
	data         []byte
	startOff     int
	endOff       int
	docStartOff  int
	groupMembers int
	partOfGroup  bool
	// stale records that the exact-line match failed and the declaration was
	// re-located by name, so the caller knows the index was behind.
	stale bool
}

// symbolSpan re-parses filePath and returns the declaration matching name and
// kind, preferring the declaration at posLine and falling back to a name match
// when the index is stale.
func symbolSpan(filePath string, posLine int, receiver, name, kind string) (*declSpan, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read source file %s: %w", filePath, err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse source file %s: %w", filePath, err)
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

	span := func(m *spanMatch) *declSpan {
		return &declSpan{
			fset:         fset,
			data:         data,
			node:         m.node,
			startOff:     m.startOff,
			endOff:       m.endOff,
			docStartOff:  m.docStartOff,
			partOfGroup:  m.partOfGroup,
			groupMembers: m.groupMembers,
			stale:        m.stale,
		}
	}

	// Exact-line match first: the fast path when the index is fresh.
	if m := matchAtLine(posLine); m != nil {
		return span(m), nil
	}

	// Name-only fallback: the index may be stale (file edited since indexing),
	// so the recorded line no longer matches. Top-level names are unique per
	// file and methods are disambiguated by receiver, so this stays safe.
	if posLine != 0 {
		if m := matchAtLine(0); m != nil {
			m.stale = true
			return span(m), nil
		}
	}

	return nil, fmt.Errorf("declaration not found: %s (kind=%s) at line %d in %s", name, kind, posLine, filePath)
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
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	PosFile       string `json:"pos_file"`
	Body          string `json:"body"`
	ContextBefore string `json:"context_before,omitempty"`
	ContextAfter  string `json:"context_after,omitempty"`
	PosLine       int    `json:"pos_line"`
	PosEndLine    int    `json:"pos_end_line"`
	GroupMembers  int    `json:"group_members,omitempty"`
	PartOfGroup   bool   `json:"part_of_group"`
}

func GetSymbolBody(s *store.Store, qualifiedName string, contextLines int, includeDoc bool) (*SymbolBodyResult, error) {
	sym, err := s.SymbolByName(qualifiedName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, symbolMissError(s, qualifiedName)
		}
		return nil, err
	}

	span, err := symbolSpan(sym.PosFile, sym.PosLine, sym.Receiver, sym.Name, sym.Kind)
	if err != nil {
		return nil, err
	}

	bodyStart := span.startOff
	if includeDoc && span.docStartOff >= 0 {
		bodyStart = span.docStartOff
	}
	body := string(span.data[bodyStart:span.endOff])

	lineOffsets := computeLineOffsets(span.data)
	posEndLine := offsetToLine(lineOffsets, span.endOff)
	posStartLine := offsetToLine(lineOffsets, bodyStart)

	var before, after string
	if contextLines > 0 {
		startLineIdx := max(posStartLine-1-contextLines, 0)
		endLineIdx := posStartLine - 1
		if endLineIdx >= 0 && startLineIdx <= endLineIdx {
			before = sliceLines(span.data, lineOffsets, startLineIdx, endLineIdx)
		}

		afterStart := posEndLine
		afterEnd := posEndLine - 1 + contextLines
		if afterStart < len(lineOffsets) && afterEnd < len(lineOffsets) {
			after = sliceLines(span.data, lineOffsets, afterStart, afterEnd)
		}
	}

	return &SymbolBodyResult{
		QualifiedName: sym.QualifiedName,
		Kind:          sym.Kind,
		PosFile:       sym.PosFile,
		PosLine:       sym.PosLine,
		PosEndLine:    posEndLine,
		Body:          body,
		PartOfGroup:   span.partOfGroup,
		GroupMembers:  span.groupMembers,
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
	Packages []string `json:"packages"`
	Level    int      `json:"level"`
}

type Hub struct {
	Package string `json:"package"`
	FanIn   int    `json:"fan_in"`
	FanOut  int    `json:"fan_out"`
}

type LayersResult struct {
	Layers []Layer `json:"layers"`
	Hubs   []Hub   `json:"hubs"`
}

type hubEntry struct {
	pkg    string
	fanIn  int
	fanOut int
}

func buildProjectGraph(s *store.Store, projectSet map[string]bool) (graph map[string][]string, fanIn, fanOut map[string]int, err error) {
	imports, err := runtimeImportEdges(s)
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
	if _, after, ok := strings.CutLast(toRef, "/"); ok {
		bare := after
		if projectSet[bare] {
			return bare
		}
	}
	return ""
}

type FlowResult struct {
	Package                  string       `json:"package"`
	Imports                  []EdgeDetail `json:"imports"`
	Importers                []EdgeDetail `json:"importers"`
	TransitiveProjectImports []EdgeDetail `json:"transitive_project_imports"`
}

func directImports(pkgPath string, allImports []store.Edge, projectSet map[string]bool) []EdgeDetail {
	out := []EdgeDetail{}
	for _, e := range allImports {
		if e.FromRef == pkgPath && (projectSet[e.ToRef] || resolveProjectTarget(e.ToRef, projectSet) != "") {
			out = append(out, edgeToDetail(e))
		}
	}
	return out
}

func directImporters(pkgPath string, allImports []store.Edge, reverseMap map[string]string) []EdgeDetail {
	out := []EdgeDetail{}
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
	allImports, err := runtimeImportEdges(s)
	if err != nil {
		return nil, err
	}
	reverseMap := buildImportPathMap(allImports, projectSet)

	return &FlowResult{
		Package:                  pkgPath,
		Imports:                  directImports(pkgPath, allImports, projectSet),
		Importers:                directImporters(pkgPath, allImports, reverseMap),
		TransitiveProjectImports: transitiveImports(pkgPath, store.BuildImportAdjacency(allImports), projectSet),
	}, nil
}

type EntryPoint struct {
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Signature     string `json:"signature"`
	PosFile       string `json:"pos_file"`
	Reason        string `json:"reason"`
	PosLine       int    `json:"pos_line"`
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
	// HTTP handlers are usually plain functions (func Sync(w, r)), not methods;
	// restricting this to methods made the heuristic return nothing on an
	// HTTP-serving repo.
	if allowed["handler_sig"] && !isTest && containsHTTPHandler(sym.Signature) {
		out = addEntryPoint(out, seen, sym, "handler_sig")
	}
	// A name-only heuristic: Run/Start/Serve are as often worker loops as HTTP
	// handlers, so this reason means "named lifecycle entry point", not
	// "handler". handler_sig is the one that proves a handler signature.
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

// liveByDesign reports whether a symbol must never be reported as dead even
// with no incoming edge. These are the declarations the Go runtime invokes
// itself (main, init) and the blank identifier, which names nothing
// addressable. UnusedSymbols and EntryPoints share this so the two cannot
// disagree about what "nothing calls it" means.
//
// pkgMain is keyed by package path and is true only for packages whose clause
// is `main`, so a library func main() stays reportable.
func liveByDesign(sym store.Symbol, pkgMain map[string]bool) bool {
	if sym.Name == "_" {
		return true
	}
	if sym.Receiver != "" || sym.Kind != kindFunction {
		return false
	}
	if sym.Name == "init" {
		return true
	}
	return sym.Name == kindMain && pkgMain[sym.PackagePath]
}

// interfaceQualifiedNames returns the qualified names of every interface in
// the given symbol set, so a method can be traced back to an interface receiver.
func interfaceQualifiedNames(syms []store.Symbol) map[string]bool {
	names := make(map[string]bool)
	for _, sym := range syms {
		if sym.Kind == "interface" {
			names[sym.QualifiedName] = true
		}
	}
	return names
}

// mainPackages returns the set of package paths whose package clause is
// `main`. It is the authoritative answer to "does a func main() here actually
// start a program", which a path suffix check cannot give.
func mainPackages(s *store.Store) (map[string]bool, error) {
	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	pkgMain := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		if p.Name == kindMain {
			pkgMain[p.Path] = true
		}
	}
	return pkgMain, nil
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
	before, _, ok := strings.CutLast(qn, ".")
	if !ok {
		return ""
	}
	return before
}

type ChangedSymbol struct {
	BlastRadius   *BlastRadius `json:"blast_radius,omitempty"`
	QualifiedName string       `json:"qualified_name"`
	Kind          string       `json:"kind"`
	ChangeType    string       `json:"change_type"`
	RenamedFrom   string       `json:"renamed_from,omitempty"`
	shape         string
	PosFile       string `json:"pos_file"`
	Body          string `json:"body,omitempty"`
	Repo          string `json:"repo,omitempty"`
	PosLine       int    `json:"pos_line"`
}

// RenamedFile records a file that moved, so a changed symbol's PosFile can be
// traced back to the path callers and imports still reference.
type RenamedFile struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ChangedSymbolsSummary struct {
	Modified     int `json:"modified"`
	Added        int `json:"added"`
	Removed      int `json:"removed"`
	FilesChanged int `json:"files_changed"`
	Renamed      int `json:"renamed"`
}

type ChangedSymbolsResult struct {
	Symbols       []ChangedSymbol       `json:"symbols"`
	Renamed       []RenamedFile         `json:"renamed"`
	CompileErrors []store.Diagnostic    `json:"compile_errors,omitempty"`
	Summary       ChangedSymbolsSummary `json:"summary"`
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
	untracked, err := vcs.GitUntrackedFiles(repoDir)
	if err != nil {
		return nil, err
	}

	result := &ChangedSymbolsResult{
		Renamed: []RenamedFile{},
		Summary: ChangedSymbolsSummary{FilesChanged: len(statuses) + len(untracked)},
	}
	seen := make(map[string]bool)

	for _, st := range statuses {
		if st.Status == "D" {
			if err := processDeletedFile(s, repoDir, ref, result, st.Path, withBlast, seen); err != nil {
				return nil, err
			}
			continue
		}
		if st.Status != "" && (st.Status[0] == 'R' || st.Status[0] == 'C') {
			result.Renamed = append(result.Renamed, RenamedFile{From: st.OldPath, To: st.Path})
			result.Summary.Renamed++
		}
		if err := processChangedFile(s, repoDir, ref, result, filepath.Join(repoDir, st.Path), withBlast, includeBodies, includeTests, seen); err != nil {
			return nil, err
		}
	}
	for _, path := range untracked {
		if err := processChangedFile(s, repoDir, ref, result, filepath.Join(repoDir, path), withBlast, includeBodies, includeTests, seen); err != nil {
			return nil, err
		}
	}

	reportRenames(result)
	if err := attachCompileErrors(s, result); err != nil {
		return nil, err
	}
	return result, nil
}

// attachCompileErrors copies the index's recorded type-check and parse errors
// onto the delta, unfiltered: a dangling call site lives in a file the change
// did not touch, which is precisely the case the section exists to surface.
func attachCompileErrors(s *store.Store, result *ChangedSymbolsResult) error {
	diags, err := s.Diagnostics("")
	if err != nil {
		return fmt.Errorf("changed_symbols: diagnostics: %w", err)
	}
	if len(diags) > 0 {
		result.CompileErrors = diags
	}
	return nil
}

// reportRenames turns a removed+added pair with the same masked shape into one
// renamed symbol. Without it a rename surfaced as a modification of the new
// name plus nothing at all for the old one, so the old name callers still use
// disappeared from the answer.
func reportRenames(result *ChangedSymbolsResult) {
	removed := make(map[string][]int)
	for i, sym := range result.Symbols {
		if sym.ChangeType == changeTypeRemoved && sym.shape != "" {
			removed[sym.shape] = append(removed[sym.shape], i)
		}
	}
	if len(removed) == 0 {
		return
	}
	dropped := make(map[int]bool)
	for i := range result.Symbols {
		sym := &result.Symbols[i]
		if sym.ChangeType != changeTypeAdded || sym.shape == "" {
			continue
		}
		candidates := removed[sym.shape]
		if len(candidates) == 0 {
			continue
		}
		from := candidates[0]
		removed[sym.shape] = candidates[1:]
		dropped[from] = true
		sym.ChangeType = changeTypeRenamed
		sym.RenamedFrom = result.Symbols[from].QualifiedName
	}
	if len(dropped) == 0 {
		return
	}
	kept := result.Symbols[:0]
	for i, sym := range result.Symbols {
		if !dropped[i] {
			kept = append(kept, sym)
		}
	}
	result.Symbols = kept
	result.Summary = recountChangedSummary(result.Symbols, result.Summary.FilesChanged, result.Summary.Renamed)
}

// maskedShape returns a declaration's text with its declared identifier
// replaced by a marker, so a rename leaves the shape unchanged while any other
// edit does not. Masking is by AST offset, not text substitution: a body string
// literal containing the old name must not defeat the match. A grouped spec is
// sliced to its own range, so renaming one entry does not make its siblings
// look modified.
func maskedShape(fset *token.FileSet, data []byte, node ast.Node, bare string) string {
	if spec := specForName(node, bare); spec != nil {
		node = spec
	}
	start := fset.Position(node.Pos()).Offset
	end := fset.Position(node.End()).Offset
	name := shapeIdent(node, bare)
	if name == nil {
		return string(data[start:end])
	}
	nameOff := fset.Position(name.Pos()).Offset
	return string(data[start:nameOff]) + "\x00name\x00" + string(data[nameOff+len(name.Name):end])
}

// specForName narrows a grouped declaration to the spec declaring bare, so the
// shape covers one entry instead of the whole block.
func specForName(node ast.Node, bare string) ast.Node {
	d, ok := node.(*ast.GenDecl)
	if !ok {
		return nil
	}
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if s.Name != nil && s.Name.Name == bare {
				return s
			}
		case *ast.ValueSpec:
			for _, n := range s.Names {
				if n != nil && n.Name == bare {
					return s
				}
			}
		}
	}
	return nil
}

// shapeIdent returns the identifier a rename would replace in node.
func shapeIdent(node ast.Node, bare string) *ast.Ident {
	switch d := node.(type) {
	case *ast.FuncDecl:
		return d.Name
	case *ast.TypeSpec:
		return d.Name
	case *ast.ValueSpec:
		for _, n := range d.Names {
			if n != nil && n.Name == bare {
				return n
			}
		}
	}
	return nil
}

// recountChangedSummary rebuilds the per-change-type counters from the final
// symbol list, so a rename reclassification cannot leave the totals stale.
func recountChangedSummary(syms []ChangedSymbol, filesChanged, renamedFiles int) ChangedSymbolsSummary {
	summary := ChangedSymbolsSummary{FilesChanged: filesChanged, Renamed: renamedFiles}
	for _, sym := range syms {
		switch sym.ChangeType {
		case changeTypeAdded:
			summary.Added++
		case changeTypeModified:
			summary.Modified++
		case changeTypeRemoved:
			summary.Removed++
		case changeTypeRenamed:
			summary.Renamed++
		}
	}
	return summary
}

// WorkspaceChangedSymbols computes a workspace-wide diff: changed symbols from
// every indexed member, each computed against that repo's own default ref and
// tagged with its repo (module path). Repos with no checkout are skipped.
func WorkspaceChangedSymbols(s *store.Store, withBlast, includeBodies, includeTests bool) (*ChangedSymbolsResult, error) {
	repos, err := s.ListRepos()
	if err != nil {
		return nil, err
	}

	result := &ChangedSymbolsResult{Renamed: []RenamedFile{}}
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

func processDeletedFile(s *store.Store, repoDir, ref string, result *ChangedSymbolsResult, file string, withBlast bool, seen map[string]bool) error {
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
	span, spanErr := symbolSpan(filePath, posLine, receiver, name, kind)
	if spanErr != nil {
		return "", fmt.Errorf("changed_symbols: body for %s in %s: %w", name, filePath, spanErr)
	}
	return string(span.data[span.startOff:span.endOff]), nil
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

// processChangedFile classifies each symbol in a changed file by comparing its
// masked declaration shape at ref with the shape on disk. A hunk-overlap test
// cannot do this: git reports an insertion as a zero-length old range *before*
// the hunk, so an appended function looked modified, and a renamed function was
// reported under its new name as a modification of itself.
func processChangedFile(s *store.Store, repoDir, ref string, result *ChangedSymbolsResult, file string, withBlast, includeBodies, includeTests bool, seen map[string]bool) error {
	syms, err := s.SymbolsInFile(file, includeTests)
	if err != nil {
		return fmt.Errorf("changed_symbols: symbols in %s: %w", file, err)
	}
	if len(syms) == 0 {
		return nil
	}
	pkgPath := syms[0].PackagePath

	rel, relErr := filepath.Rel(repoDir, file)
	if relErr != nil {
		return fmt.Errorf("changed_symbols: relative path for %s: %w", file, relErr)
	}
	oldText, oldErr := symbolTextsAtRef(repoDir, ref, rel, pkgPath)
	if oldErr != nil {
		return oldErr
	}
	curText := symbolTextsOnDisk(syms)

	for _, sym := range syms {
		if seen[sym.QualifiedName] {
			continue
		}
		seen[sym.QualifiedName] = true

		ct := classifySymbol(sym.QualifiedName, curText, oldText)
		if ct == "" {
			continue
		}
		if err := appendChangedSymbol(s, result, sym, ct, "", curText[sym.QualifiedName], withBlast, includeBodies); err != nil {
			return err
		}
	}
	return appendRemovedFromRef(s, result, oldText, curText, pkgPath, withBlast, seen)
}

// appendRemovedFromRef reports the declarations that existed at ref but not on
// disk. They are absent from the index, so nothing in the current symbol set
// can surface them; the ref-side parse is the only source for a removal or the
// "from" half of a rename.
func appendRemovedFromRef(s *store.Store, result *ChangedSymbolsResult, oldText, curText map[string]string, pkgPath string, withBlast bool, seen map[string]bool) error {
	names := make([]string, 0, len(oldText))
	for qn := range oldText {
		if _, exists := curText[qn]; !exists {
			names = append(names, qn)
		}
	}
	sort.Strings(names)
	for _, qn := range names {
		if seen[qn] {
			continue
		}
		seen[qn] = true
		sym := store.Symbol{QualifiedName: qn, PackagePath: pkgPath, Kind: kindOfQualifiedName(qn)}
		if err := appendChangedSymbol(s, result, sym, changeTypeRemoved, "", oldText[qn], withBlast, false); err != nil {
			return err
		}
	}
	return nil
}

// appendChangedSymbol records one changed symbol and its counters.
func appendChangedSymbol(s *store.Store, result *ChangedSymbolsResult, sym store.Symbol, ct, renamedFrom, shape string, withBlast, includeBodies bool) error {
	cs := ChangedSymbol{
		QualifiedName: sym.QualifiedName,
		Kind:          sym.Kind,
		ChangeType:    ct,
		RenamedFrom:   renamedFrom,
		PosFile:       sym.PosFile,
		PosLine:       sym.PosLine,
		shape:         shape,
	}
	if ct != changeTypeRemoved {
		body, err := readChangedBody(sym.PosFile, sym.Name, sym.Kind, sym.Receiver, sym.PosLine, includeBodies)
		if err != nil {
			return err
		}
		cs.Body = body
	}
	if withBlast {
		br, err := blastRadiusFor(s, sym.QualifiedName)
		if err != nil {
			return err
		}
		cs.BlastRadius = br
	}
	result.Symbols = append(result.Symbols, cs)
	incSummary(ct, result)
	return nil
}

// kindOfQualifiedName recovers a symbol kind from a qualified name. The ref-side
// parse knows the declaration form, so this only has to tell a method from a
// function: a name with three dot-separated parts is pkg.Receiver.Method.
func kindOfQualifiedName(qn string) string {
	if strings.Count(qn, ".") >= 2 {
		return kindMethod
	}
	return kindFunction
}

// classifySymbol reports how one symbol changed. A name present only now was
// added, a name present only at ref was removed (or renamed — see
// reportRenames), and a name whose declaration text differs was modified. A
// symbol whose text is identical was not touched, even if a hunk covers its
// line range.
func classifySymbol(qn string, cur, old map[string]string) string {
	curText, inCur := cur[qn]
	oldText, inOld := old[qn]
	switch {
	case inCur && !inOld:
		return changeTypeAdded
	case !inCur && inOld:
		return changeTypeRemoved
	case curText != oldText:
		return changeTypeModified
	default:
		return ""
	}
}

// symbolTextsOnDisk reads each symbol's masked declaration shape from the
// working tree. A symbol whose span cannot be located is skipped rather than
// failing the whole diff: an unparsable file is already reported by diagnostics.
func symbolTextsOnDisk(syms []store.Symbol) map[string]string {
	out := make(map[string]string, len(syms))
	for _, sym := range syms {
		span, err := symbolSpan(sym.PosFile, sym.PosLine, sym.Receiver, sym.Name, sym.Kind)
		if err != nil {
			continue
		}
		out[sym.QualifiedName] = maskedShape(span.fset, span.data, span.node, sym.Name)
	}
	return out
}

// symbolTextsAtRef parses the file as it exists at ref and returns each
// declaration's masked shape keyed by the qualified name the current index
// would use. The key is file-based like the removed-symbol path, so a symbol
// that only exists at ref is still nameable.
func symbolTextsAtRef(repoDir, ref, file, pkgPath string) (map[string]string, error) {
	data, err := vcs.GitShowFile(repoDir, ref, file)
	if errors.Is(err, vcs.ErrFileNotInRef) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("changed_symbols: %s@%s: %w", file, ref, err)
	}
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, file, data, parser.ParseComments)
	if perr != nil {
		return nil, fmt.Errorf("changed_symbols: parse %s@%s: %w", file, ref, perr)
	}
	out := make(map[string]string)
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			out[funcSymbolKey(pkgPath, d)] = maskedShape(fset, data, d, d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				for _, key := range genDeclKeys(pkgPath, spec) {
					bare := key[strings.LastIndex(key, ".")+1:]
					out[key] = maskedShape(fset, data, spec, bare)
				}
			}
		}
	}
	return out, nil
}

// funcSymbolKey rebuilds the qualified name the index uses for a function or
// method declaration: pkg.Func, or pkg.Receiver.Func for a method.
func funcSymbolKey(pkgPath string, d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return pkgPath + "." + d.Name.Name
	}
	return pkgPath + "." + astReceiverName(d.Recv.List[0].Type) + "." + d.Name.Name
}

func genDeclKeys(pkgPath string, spec ast.Spec) []string {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		if s.Name != nil {
			return []string{pkgPath + "." + s.Name.Name}
		}
	case *ast.ValueSpec:
		keys := make([]string, 0, len(s.Names))
		for _, n := range s.Names {
			if n != nil {
				keys = append(keys, pkgPath+"."+n.Name)
			}
		}
		return keys
	}
	return nil
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

// estimateTokens mirrors the rendered bundle at roughly four bytes per token,
// counting every header the renderer emits. Measuring only the body and the
// edges under-reported a 1.2 KB bundle as 115 tokens: at a small budget the
// fixed symbol block is most of the output, so the number must include it.
func estimateTokens(b *Bundle) int {
	n := len(b.QualifiedName) + bundleHeaderBytes
	n += bundleSymbolBytes(b.Symbol)
	if b.Body != "" {
		n += len(b.Body) + bundleBodyFenceBytes
	}
	if len(b.Callees) > 0 {
		n += bundleSectionBytes
		for _, c := range b.Callees {
			n += len(c.ToRef) + len(c.EdgeType) + bundleEntryBytes
		}
	}
	if len(b.Callers) > 0 {
		n += bundleSectionBytes
		for _, c := range b.Callers {
			n += len(c.FromRef) + len(c.EdgeType) + bundleEntryBytes
		}
	}
	if len(b.SameFile) > 0 {
		n += bundleSectionBytes
		for _, s := range b.SameFile {
			n += len(s.QualifiedName) + len(s.Kind) + bundleEntryBytes
		}
	}
	return n / 4
}

// Fixed per-section costs in the rendered bundle, in bytes.
const (
	bundleHeaderBytes    = 40 // "Bundle: <qn> (est. N tokens)\n\n"
	bundleSectionBytes   = 24 // "Callees (12):\n" and friends
	bundleBodyFenceBytes = 13 // "```go\n" ... "\n```\n\n"
	bundleEntryBytes     = 10 // "  -> " + " (calls)" + "\n"
)

// bundleSymbolBytes measures the symbol block rendered ahead of the body. The
// doc is trimmed the way the renderer trims it, and nested anonymous structs
// count their own entries because the renderer recurses into them.
func bundleSymbolBytes(sym *SymbolDetail) int {
	if sym == nil {
		return 0
	}
	n := len(sym.QualifiedName) + len(sym.Kind) + len(sym.Signature) + len(sym.PosFile) + 50
	if doc := firstSentenceLen(sym.Doc); doc > 0 {
		n += doc + 8
	}
	return n + bundleFieldBytes(sym.Fields)
}

// bundleFieldBytes measures one level of the fields tree.
func bundleFieldBytes(fields []store.SymbolField) int {
	if len(fields) == 0 {
		return 0
	}
	n := 10 // "  fields:\n"
	for _, f := range fields {
		n += len(f.GoName) + len(f.GoType) + 5
		if f.WireName != f.GoName {
			n += len(f.WireName) + 7
		}
		n += bundleFieldBytes(f.Fields)
	}
	return n
}

// firstSentenceLen returns the byte length the renderer would keep.
func firstSentenceLen(s string) int {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "."); i >= 0 {
		return i + 1
	}
	if len(s) > 100 {
		return 103
	}
	return len(s)
}

// trimBundle drops the least essential sections until the estimate fits, then
// recomputes so the reported number describes what is actually returned. The
// symbol block is measured but never dropped: a bundle without its signature
// is not a bundle.
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
	b.TokenEstimate = estimateTokens(b)
}

// filterOutGenerated drops generated-code symbols. hotspots, importance, and
// unused make claims ("top risk", "dead code") that generated output cannot
// meaningfully satisfy, so provenance is excluded there rather than ranked.
func filterOutGenerated(syms []store.Symbol) []store.Symbol {
	out := make([]store.Symbol, 0, len(syms))
	for _, sym := range syms {
		if !sym.IsGenerated {
			out = append(out, sym)
		}
	}
	return out
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
	allSyms = filterOutGenerated(allSyms)

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
	opt.Apply(options, opts)

	allSyms, err := s.AllSymbols(false)
	if err != nil {
		return nil, err
	}
	allSyms = filterOutGenerated(allSyms)
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
