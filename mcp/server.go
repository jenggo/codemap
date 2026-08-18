package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"codemap/contract"
	"codemap/parse"
	"codemap/query"
	"codemap/render"
	"codemap/resolve"
	"codemap/store"
	"codemap/vcs"
	"codemap/workspace"
)

type Server struct {
	store *store.Store

	// Freshness/backoff memoization prevents repeated git/tree-walk and go list.
	freshness     map[string]freshnessEntry
	failedIndexes map[string]failedIndex

	// Test/override seams.
	handlers       map[string]toolHandler
	stalenessCheck func(dbPath, repoPath string) (bool, error)
	indexFn        func(absPath string) error
	dbPath         string
	repoPath       string
	warnings       []string
	reindexed      []string

	// Filesystem allowlist roots (explicitly configured; derived otherwise).
	allowedPaths []string

	contractCfg        contract.Config
	freshnessTTL       time.Duration
	failedIndexBackoff time.Duration

	// State mutated during dispatch; guarded by mu.
	mu             sync.Mutex
	hasContractCfg bool
}

// freshnessEntry memoizes one staleness-check result with its timestamp.
type freshnessEntry struct {
	checkedAt time.Time
}

// failedIndex records a failed auto-index attempt with its error and time.
type failedIndex struct {
	at  time.Time
	err string
}

const (
	defaultFreshnessTTL       = 3 * time.Second
	defaultFailedIndexBackoff = 30 * time.Second
)

func New(s *store.Store) *Server {
	return &Server{
		store:              s,
		repoPath:           ".",
		freshness:          make(map[string]freshnessEntry),
		failedIndexes:      make(map[string]failedIndex),
		freshnessTTL:       defaultFreshnessTTL,
		failedIndexBackoff: defaultFailedIndexBackoff,
	}
}

func NewLazy(dbPath string) *Server {
	return &Server{
		dbPath:             dbPath,
		repoPath:           ".",
		freshness:          make(map[string]freshnessEntry),
		failedIndexes:      make(map[string]failedIndex),
		freshnessTTL:       defaultFreshnessTTL,
		failedIndexBackoff: defaultFailedIndexBackoff,
	}
}

// ttl returns the configured freshness TTL; zero means the default, negative
// disables caching (every request rechecks).
func (s *Server) ttl() time.Duration {
	if s.freshnessTTL < 0 {
		return -1
	}
	if s.freshnessTTL == 0 {
		return defaultFreshnessTTL
	}
	return s.freshnessTTL
}

func (s *Server) backoff() time.Duration {
	if s.failedIndexBackoff <= 0 {
		return defaultFailedIndexBackoff
	}
	return s.failedIndexBackoff
}

// contractConfig returns the contract analysis config: the workspace-declared
// config when one was loaded, otherwise defaults.
func (s *Server) contractConfig() contract.Config {
	if s.hasContractCfg {
		return s.contractCfg
	}
	return contract.DefaultConfig()
}

// contractConfigFromWorkspace builds a contract analysis config from a parsed
// workspace config (suppress pairs + custom runtime extractors). Config errors
// (negative arg_index, malformed suppress separator, invalid normalize_rules)
// surface to the caller instead of silently degrading.
func contractConfigFromWorkspace(cfg *workspace.Config) (contract.Config, error) {
	return cfg.ContractConfig()
}

func (s *Server) getStore() (*store.Store, error) {
	s.mu.Lock()
	s.reindexed = nil
	s.mu.Unlock()
	dbPath := s.resolveDBPath()

	if s.store == nil {
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			if err := s.autoIndexMemoized(s.repoPath); err != nil {
				return nil, fmt.Errorf("auto-index failed: %w", err)
			}
			return s.store, nil
		}
		st, err := store.Open(dbPath)
		if err != nil {
			if err := s.autoIndexMemoized(s.repoPath); err != nil {
				return nil, fmt.Errorf("auto-index failed: %w", err)
			}
			return s.store, nil
		}
		s.store = st
	}

	if err := s.refreshStale(dbPath); err != nil {
		return nil, err
	}
	return s.store, nil
}

// refreshStale re-indexes any stale repo before the query answers. Workspace
// databases reindex per member; single-repo databases keep the legacy
// full-rebuild path. Repos reindexed this call are recorded for the response.
// Results are memoized per database for the freshness TTL so the git/tree-walk
// work runs at most once per window.
func (s *Server) refreshStale(dbPath string) error {
	repos, err := s.store.ListRepos()
	if err == nil && len(repos) > 0 {
		return s.refreshWorkspaceStale(dbPath)
	}
	return s.refreshSingleStale(dbPath)
}

func (s *Server) refreshWorkspaceStale(dbPath string) error {
	key := "workspace:" + dbPath
	if s.freshnessValid(key) {
		return nil
	}

	reindexed, rerr := workspace.ReindexStale(dbPath)
	if rerr != nil {
		return rerr
	}

	s.mu.Lock()
	s.freshnessEntries()[key] = freshnessEntry{checkedAt: time.Now()}
	if len(reindexed) > 0 {
		s.reindexed = append(s.reindexed, reindexed...)
	}
	// Reanalyze stale contract data (non-fatal).
	contractReanalyzed, cerr := contract.ReanalyzeStaleContracts(s.store, s.contractConfig())
	if cerr == nil && len(contractReanalyzed) > 0 {
		for _, repo := range contractReanalyzed {
			s.reindexed = append(s.reindexed, repo+" (contracts)")
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Server) refreshSingleStale(dbPath string) error {
	key := "single:" + s.repoPath
	if s.freshnessValid(key) {
		return nil
	}

	stale, err := s.staleCheck(dbPath, s.repoPath)
	if err != nil {
		return err
	}
	s.recordFresh(key)
	if stale {
		// Capture why the index was considered stale *before* rebuilding, so the
		// notice that accompanies the answer can explain it.
		reason := "stale database"
		if _, r, rErr := store.StaleReason(dbPath, s.repoPath); rErr == nil && r != "" {
			reason = r
		}
		if err := s.autoIndexMemoized(s.repoPath); err != nil {
			return err
		}
		// Informational notice, emitted only after the rebuild succeeds. Framed
		// as a completed action so agents trust the result below instead of
		// mistaking the auto-reindex for stale or failed data.
		s.mu.Lock()
		s.warnings = append(s.warnings,
			fmt.Sprintf("index was stale (%s) and was auto-rebuilt before answering; results below reflect the latest code", reason))
		s.mu.Unlock()
	}
	return nil
}

// staleCheck is the single-repo staleness probe, overridable in tests.
func (s *Server) staleCheck(dbPath, repoPath string) (bool, error) {
	if s.stalenessCheck != nil {
		return s.stalenessCheck(dbPath, repoPath)
	}
	return store.IsStale(dbPath, repoPath)
}

// freshnessValid reports whether the memoized staleness result for key is still
// inside the TTL window.
func (s *Server) freshnessValid(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.freshnessEntries()[key]
	return ok && time.Since(entry.checkedAt) < s.ttl()
}

// recordFresh stamps a successful staleness check for key.
func (s *Server) recordFresh(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freshnessEntries()[key] = freshnessEntry{checkedAt: time.Now()}
}

func (s *Server) freshnessEntries() map[string]freshnessEntry {
	if s.freshness == nil {
		s.freshness = make(map[string]freshnessEntry)
	}
	return s.freshness
}

// autoIndexMemoized runs autoIndex but remembers failures for the backoff
// window, so a path that fails (missing toolchain, unparseable tree, timeout)
// fails fast on repeat requests instead of re-running the expensive index.
func (s *Server) autoIndexMemoized(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if fi, ok := s.failedIndexEntries()[absPath]; ok && time.Since(fi.at) < s.backoff() {
		retryIn := (s.backoff() - time.Since(fi.at)).Round(time.Second)
		s.mu.Unlock()
		return fmt.Errorf("auto-index for %s previously failed (%s); retrying in %s",
			absPath, fi.err, retryIn)
	}
	s.mu.Unlock()

	if err := s.doIndex(absPath); err != nil {
		s.mu.Lock()
		s.failedIndexEntries()[absPath] = failedIndex{at: time.Now(), err: err.Error()}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	delete(s.failedIndexEntries(), absPath)
	s.mu.Unlock()
	return nil
}

// doIndex is the index entry point, overridable in tests.
func (s *Server) doIndex(absPath string) error {
	if s.indexFn != nil {
		return s.indexFn(absPath)
	}
	return s.autoIndex(absPath)
}

func (s *Server) failedIndexEntries() map[string]failedIndex {
	if s.failedIndexes == nil {
		s.failedIndexes = make(map[string]failedIndex)
	}
	return s.failedIndexes
}

func (s *Server) resolveDBPath() string {
	if s.dbPath != "" {
		return s.dbPath
	}
	return store.DefaultPath()
}

func (s *Server) autoIndex(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		return fmt.Errorf("parse error: %w", err)
	}
	if len(parseResult.Errors) > 0 {
		s.mu.Lock()
		s.warnings = append(s.warnings,
			fmt.Sprintf("%d package(s) failed to load during index; the index may be incomplete. Run 'index' to see details", len(parseResult.Errors)))
		s.mu.Unlock()
	}

	resolveResult := resolve.Run(parseResult)
	newStore, err := store.Create(s.resolveDBPath())
	if err != nil {
		return fmt.Errorf("store error: %w", err)
	}

	if err := newStore.Write(resolveResult, parse.FileContents(parseResult), nil); err != nil {
		_ = newStore.Close()
		return fmt.Errorf("write error: %w", err)
	}

	if err := newStore.SetIndexedAt(time.Now()); err != nil {
		_ = newStore.Close()
		return fmt.Errorf("set indexed_at error: %w", err)
	}

	if err := newStore.SetRepoMeta(absPath, gitHead(absPath), len(resolveResult.Packages), len(resolveResult.Symbols)); err != nil {
		_ = newStore.Close()
		return fmt.Errorf("set repo meta error: %w", err)
	}
	newStore.RecordDirtyFingerprint(absPath, "")

	if s.store != nil {
		_ = s.store.Close()
	}
	s.store = newStore
	return nil
}

func gitHead(path string) string {
	head, _ := vcs.GitHead(path)
	return head
}

func (s *Server) Run() error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		s.runLine(line)
	}
	return scanner.Err()
}

// runLine dispatches one request line, recovering from any panic raised by a
// handler so a single bad request can never take down the server loop.
func (s *Server) runLine(line string) {
	st := &dispatchState{}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "codemap: panic while dispatching request: %v\n%s", r, debug.Stack())
			if st.hasID {
				s.sendError(st.id, -32603, fmt.Sprintf("Internal error: %v", r))
			}
		}
	}()
	s.dispatchLineState(line, st)
}

// dispatchState tracks the request id so a panic mid-dispatch can still be
// reported against the request that triggered it.
type dispatchState struct {
	id    json.RawMessage
	hasID bool
}

func (s *Server) dispatchLine(line string) {
	s.dispatchLineState(line, &dispatchState{})
}

func (s *Server) dispatchLineState(line string, st *dispatchState) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		s.sendError(nil, -32700, "Parse error")
		return
	}

	var methodStr string
	if err := json.Unmarshal(msg["method"], &methodStr); err != nil {
		s.sendError(nil, -32700, "Parse error")
		return
	}

	id, hasID := msg["id"]
	if hasID {
		st.id = id
		st.hasID = true
	}

	switch methodStr {
	case "initialize":
		s.handleInitialize(id)
	case "ping":
		s.handlePing(id)
	case "notifications/initialized":
	case "tools/list":
		s.handleToolsList(id)
	case "tools/call":
		s.handleToolsCall(id, msg)
	default:
		if hasID {
			s.sendError(id, -32601, "Method not found")
		}
	}
}

func (s *Server) handleInitialize(id json.RawMessage) {
	s.sendResponse(id, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{keyName: "codemap", "version": "0.1.0"},
	})
}

func (s *Server) handlePing(id json.RawMessage) {
	s.sendResponse(id, map[string]any{})
}

func (s *Server) handleToolsCall(id json.RawMessage, msg map[string]json.RawMessage) {
	params, ok := msg["params"]
	if !ok {
		s.sendError(id, -32602, "Invalid params")
		return
	}
	var toolCall struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &toolCall); err != nil {
		s.sendError(id, -32602, "Invalid params")
		return
	}

	result, isErr := s.handleTool(toolCall.Name, toolCall.Arguments)
	s.mu.Lock()
	var notices []string
	if len(s.reindexed) > 0 {
		notices = append(notices, "reindexed: ["+strings.Join(s.reindexed, ", ")+"]")
	}
	if len(s.warnings) > 0 {
		notices = append(notices, strings.Join(s.warnings, "\n"))
		s.warnings = nil
	}
	s.mu.Unlock()
	if len(notices) > 0 {
		result = strings.Join(notices, "\n") + "\n\n" + result
	}
	res := map[string]any{
		"content": []map[string]any{{keyType: "text", "text": result}},
	}
	if isErr {
		res["isError"] = true
	}
	s.sendResponse(id, res)
}

type toolHandler func(*store.Store, []query.Option, []render.Option, map[string]any) (string, bool)

func (s *Server) handleTool(name string, args map[string]any) (string, bool) {
	if name == "index" {
		return s.handleIndex(args)
	}
	if name == "schema" {
		return handleSchema(), false
	}
	if name == "changed_symbols" {
		dir := "."
		if v, ok := args["repo_dir"].(string); ok && v != "" {
			dir = v
		}
		if _, err := s.allowedTarget(dir); err != nil {
			return fmt.Sprintf("Error: %v", err), true
		}
	}
	if err := validateToolArgs(name, args); err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}

	st, err := s.getStore()
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}

	opts, renderOpts := s.buildOptions(args)

	handler, ok := s.toolHandlers()[name]
	if !ok {
		return fmt.Sprintf("Error: unknown tool %s", name), true
	}
	result, isErr := handler(st, opts, renderOpts, args)
	if isErr {
		if note := notIndexedNote(st, primaryArg(args)); note != "" {
			result += note
		}
	}
	return result, isErr
}

func (s *Server) toolHandlers() map[string]toolHandler {
	if s.handlers != nil {
		return s.handlers
	}
	return queryHandlers()
}

func queryHandlers() map[string]toolHandler {
	return map[string]toolHandler{
		"overview":                  handleOverview,
		"show":                      handleShow,
		toolCallersOf:               handleCallersOf,
		toolCalleesOf:               handleCalleesOf,
		"search":                    handleSearch,
		keyToolPackage:              handlePackage,
		"methods_of":                handleMethodsOf,
		"importers_of":              handleImportersOf,
		"imports_of":                handleImportsOf,
		"edges_by_type":             handleEdgesByType,
		"all_edges":                 handleAllEdges,
		"list_packages":             handleListPackages,
		"type_usage":                handleTypeUsage,
		"transitive_imports":        handleTransitiveImports,
		"search_prefix":             handleSearchPrefix,
		"find_path":                 handleFindPath,
		"method_search":             handleMethodSearch,
		"interface_impls":           handleInterfaceImpls,
		"unused":                    handleUnused,
		toolCycles:                  handleCycles,
		"symbols_in_file":           handleSymbolsInFile,
		toolBlastRadius:             handleBlastRadius,
		"get_symbol_body":           handleSymbolBody,
		"dependency_layers":         handleDependencyLayers,
		"dependency_flow":           handleDependencyFlow,
		"entry_points":              handleEntryPoints,
		"changed_symbols":           handleChangedSymbols,
		"search_text":               handleSearchText,
		"get_context_bundle":        handleContextBundle,
		"get_hotspots":              handleHotspots,
		"get_symbol_importance":     handleSymbolImportance,
		"health":                    handleHealth,
		"codemap_health":            handleHealth,
		"workspace_changed_symbols": handleWorkspaceChangedSymbols,
		toolContracts:               handleContracts,
		"contract_drift":            handleContractDrift,
		"runtime_contracts":         handleRuntimeContracts,
		"suppress_contract":         handleSuppressContract,
	}
}

// primaryArg guesses the symbol/package name a tool call is about, so a missed
// lookup can be attributed to a missing repo rather than silent absence.
func primaryArg(args map[string]any) string {
	for _, k := range []string{keyQualifiedName, keyPattern, "package_path", keyPath, keyTypeName, "prefix", "method_name", "interface_name", "from", "to"} {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) buildOptions(args map[string]any) ([]query.Option, []render.Option) {
	var opts []query.Option
	var renderOpts []render.Option

	if b, ok := args["include_tests"].(bool); ok && b {
		opts = append(opts, query.WithTests())
	}
	if b, ok := args["full_docs"].(bool); ok && b {
		renderOpts = append(renderOpts, render.WithFullDocs())
	}
	if v, ok := args["repo"].(string); ok && v != "" {
		opts = append(opts, query.WithRepo(v))
	}
	renderOpts = append(renderOpts, render.WithFormat(render.FormatTOON))

	return opts, renderOpts
}

func handleOverview(st *store.Store, opts []query.Option, renderOpts []render.Option, _ map[string]any) (string, bool) {
	result, err := query.Overview(st, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderOverview(result, renderOpts...), false
}

func handleShow(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	qn := requiredString(args, "qualified_name")
	if qn == "" {
		return errQualifiedNameRequired, true
	}
	result, err := query.Show(st, qn, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderShow(result, renderOpts...), false
}

func handleCallersOf(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	qn := requiredString(args, "qualified_name")
	if qn == "" {
		return errQualifiedNameRequired, true
	}
	opts = appendEdgeTypeFilter(args, opts)
	if v, ok := args[keyDepth].(float64); ok && v > 0 {
		opts = append(opts, query.WithDepth(int(v)))
	}
	edges, err := query.CallersOf(st, qn, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderCallers(edges, renderOpts...), false
}

func handleCalleesOf(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	qn := requiredString(args, "qualified_name")
	if qn == "" {
		return errQualifiedNameRequired, true
	}
	opts = appendEdgeTypeFilter(args, opts)
	if v, ok := args[keyDepth].(float64); ok && v > 0 {
		opts = append(opts, query.WithDepth(int(v)))
	}
	edges, err := query.CalleesOf(st, qn, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderCallees(edges, renderOpts...), false
}

func healthNotice(st *store.Store) string {
	h, err := st.Health()
	if err != nil {
		return fmt.Sprintf("No results. Index health: error reading index metadata: %v", err)
	}
	return fmt.Sprintf("No results. Index health: indexed_at=%s repo=%s head=%s %d packages, %d symbols",
		h.IndexedAt, h.RepoPath, h.GitHead, h.PackageCount, h.SymbolCount)
}

func handleSearch(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	pattern := requiredString(args, "pattern")
	if pattern == "" {
		return errPatternRequired, true
	}
	if v, ok := args[keyKind].(string); ok && v != "" {
		opts = append(opts, query.WithKind(v))
	}
	if b, ok := args["exported"].(bool); ok {
		opts = append(opts, query.WithExported(b))
	}
	if v, ok := args["file"].(string); ok && v != "" {
		opts = append(opts, query.WithFile(v))
	}
	results, err := query.Search(st, pattern, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if len(results) == 0 {
		return healthNotice(st), false
	}
	return render.RenderSearch(results, renderOpts...), false
}

func handlePackage(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	pkgPath := requiredString(args, "path")
	if pkgPath == "" {
		return "Error: path is required", true
	}
	if b, ok := args["include_unexported"].(bool); ok && b {
		opts = append(opts, query.WithUnexported())
	}
	result, err := query.Package(st, pkgPath, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderPackage(result, renderOpts...), false
}

func handleMethodsOf(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	typeName := requiredString(args, "type_name")
	if typeName == "" {
		return "Error: type_name is required", true
	}
	methods, err := query.MethodsOf(st, typeName, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if len(methods) == 0 {
		return healthNotice(st), false
	}
	return render.RenderMethodsOf(methods, renderOpts...), false
}

func handleImportersOf(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	pkgPath := requiredString(args, "package_path")
	if pkgPath == "" {
		return errPackagePathRequired, true
	}
	edges, err := query.ImportersOf(st, pkgPath, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderEdges(edges, renderOpts...), false
}

func handleImportsOf(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	pkgPath := requiredString(args, "package_path")
	if pkgPath == "" {
		return errPackagePathRequired, true
	}
	edges, err := query.ImportsOf(st, pkgPath, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderEdges(edges, renderOpts...), false
}

func handleEdgesByType(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	edgeType := requiredString(args, "edge_type")
	if edgeType == "" {
		return errEdgeTypeRequired, true
	}
	edges, err := query.EdgesByType(st, edgeType, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderEdges(edges, renderOpts...), false
}

func handleAllEdges(st *store.Store, opts []query.Option, renderOpts []render.Option, _ map[string]any) (string, bool) {
	edges, err := query.AllEdges(st, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderEdges(edges, renderOpts...), false
}

func handleListPackages(st *store.Store, opts []query.Option, renderOpts []render.Option, _ map[string]any) (string, bool) {
	pkgs, err := query.ListPackages(st, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderListPackages(pkgs, renderOpts...), false
}

func handleTypeUsage(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	typeName := requiredString(args, "type_name")
	if typeName == "" {
		return errTypeNameRequired, true
	}
	results, err := query.TypeUsage(st, typeName, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if len(results) == 0 {
		return healthNotice(st), false
	}
	return render.RenderSearch(results, renderOpts...), false
}

func handleTransitiveImports(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	pkgPath := requiredString(args, "package_path")
	if pkgPath == "" {
		return errPackagePathRequired, true
	}
	edges, err := query.TransitiveImports(st, pkgPath, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderEdges(edges, renderOpts...), false
}

func handleSearchPrefix(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	prefix := requiredString(args, "prefix")
	if prefix == "" {
		return errPrefixRequired, true
	}
	results, err := query.SearchByPrefix(st, prefix, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if len(results) == 0 {
		return healthNotice(st), false
	}
	return render.RenderSearch(results, renderOpts...), false
}

func handleFindPath(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	from := requiredString(args, "from")
	if from == "" {
		return "Error: from is required", true
	}
	to := requiredString(args, "to")
	if to == "" {
		return "Error: to is required", true
	}
	maxDepth := 10
	if v, ok := args["max_depth"].(float64); ok && v > 0 {
		maxDepth = int(v)
	}
	path, err := query.FindPath(st, from, to, maxDepth, opts...)
	if err != nil {
		// Absence is a result, not a failure: report it without the isError
		// flag so agents do not mistake "no path exists" for a query error.
		if errors.Is(err, query.ErrNoPath) {
			return fmt.Sprintf("No path found from %s to %s", from, to), false
		}
		return fmt.Sprintf("Error: %v", err), true
	}
	data, err := json.Marshal(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return string(data), false
}

func handleMethodSearch(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	methodName := requiredString(args, "method_name")
	if methodName == "" {
		return "Error: method_name is required", true
	}
	results, err := query.MethodSearch(st, methodName, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if len(results) == 0 {
		return healthNotice(st), false
	}
	return render.RenderSearch(results, renderOpts...), false
}

func handleInterfaceImpls(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	interfaceName := requiredString(args, "interface_name")
	if interfaceName == "" {
		return "Error: interface_name is required", true
	}
	results, err := query.InterfaceImplementations(st, interfaceName, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	data, err := json.Marshal(results)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return string(data), false
}

func handleUnused(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	results, err := query.UnusedSymbols(st, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	data, err := json.Marshal(results)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return string(data), false
}

func handleCycles(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	edgeType := requiredString(args, "edge_type")
	if edgeType == "" {
		return errEdgeTypeRequired, true
	}
	cycles, err := query.DetectCycles(st, edgeType)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	data, err := json.Marshal(cycles)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return string(data), false
}

func handleSymbolsInFile(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	filePath := requiredString(args, "file_path")
	if filePath == "" {
		return "Error: file_path is required", true
	}
	results, err := query.SymbolsInFile(st, filePath, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderSearch(results, renderOpts...), false
}

func handleBlastRadius(st *store.Store, opts []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	qn := requiredString(args, "qualified_name")
	if qn == "" {
		return errQualifiedNameRequired, true
	}
	depth := 3
	if v, ok := args[keyDepth].(float64); ok && v > 0 {
		depth = int(v)
	}
	result, err := query.BlastRadiusWithContracts(st, qn, depth, opts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return string(data), false
}

func handleSymbolBody(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	qn := requiredString(args, "qualified_name")
	if qn == "" {
		return errQualifiedNameRequired, true
	}
	contextLines := 0
	if v, ok := args[keyContextLines].(float64); ok && v > 0 {
		contextLines = int(v)
	}
	includeDoc := true
	if b, ok := args["include_doc"].(bool); ok {
		includeDoc = b
	}
	result, err := query.GetSymbolBody(st, qn, contextLines, includeDoc)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderSymbolBody(result, renderOpts...), false
}

func handleDependencyLayers(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	topHubs := 5
	if v, ok := args["top_hubs"].(float64); ok && v > 0 {
		topHubs = int(v)
	}
	includeTests := false
	if b, ok := args["include_tests"].(bool); ok {
		includeTests = b
	}
	result, err := query.DependencyLayers(st, includeTests, topHubs)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderDependencyLayers(result, renderOpts...), false
}

func handleDependencyFlow(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	pkgPath := requiredString(args, "package_path")
	if pkgPath == "" {
		return errPackagePathRequired, true
	}
	result, err := query.DependencyFlow(st, pkgPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderDependencyFlow(result, renderOpts...), false
}

func handleEntryPoints(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	var heuristics []string
	if v, ok := args["heuristics"].([]any); ok {
		for _, h := range v {
			if s, ok := h.(string); ok {
				heuristics = append(heuristics, s)
			}
		}
	}
	includeTests := false
	if b, ok := args["include_tests"].(bool); ok {
		includeTests = b
	}
	entries, err := query.EntryPoints(st, heuristics, includeTests)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderEntryPoints(entries, renderOpts...), false
}

func handleChangedSymbols(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	ref := ""
	if v, ok := args["ref"].(string); ok && v != "" {
		ref = v
	}
	repoDir := "."
	if v, ok := args["repo_dir"].(string); ok && v != "" {
		repoDir = v
	}
	withBlast := true
	if b, ok := args["with_blast_radius"].(bool); ok {
		withBlast = b
	}
	includeBodies := false
	if b, ok := args["include_bodies"].(bool); ok {
		includeBodies = b
	}
	includeTests := false
	if b, ok := args["include_tests"].(bool); ok {
		includeTests = b
	}
	result, err := query.ChangedSymbols(st, repoDir, ref, withBlast, includeBodies, includeTests)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderChangedSymbols(result, renderOpts...), false
}

func handleWorkspaceChangedSymbols(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, bool) {
	withBlast := true
	if b, ok := args["with_blast_radius"].(bool); ok {
		withBlast = b
	}
	includeBodies := false
	if b, ok := args["include_bodies"].(bool); ok {
		includeBodies = b
	}
	includeTests := false
	if b, ok := args["include_tests"].(bool); ok {
		includeTests = b
	}
	result, err := query.WorkspaceChangedSymbols(st, withBlast, includeBodies, includeTests)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderChangedSymbols(result, renderOpts...), false
}

func handleHealth(st *store.Store, _ []query.Option, _ []render.Option, _ map[string]any) (string, bool) {
	repos, err := st.ListRepos()
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if len(repos) == 0 {
		h, herr := st.Health()
		if herr != nil {
			return fmt.Sprintf("Error: health check failed: %v", herr), true
		}
		out := fmt.Sprintf("indexed_at=%s repo=%s head=%s %d packages, %d symbols",
			h.IndexedAt, h.RepoPath, h.GitHead, h.PackageCount, h.SymbolCount)
		return out, false
	}
	var b strings.Builder
	b.WriteString("repos:\n")
	for _, r := range repos {
		state := st.RepoState(r)
		fmt.Fprintf(&b, "  %s: %s", r.ModulePath, state)
		if r.IndexedAt != "" {
			fmt.Fprintf(&b, "  indexed_at=%s", r.IndexedAt)
		}
		if state == "stale" {
			b.WriteString("  (auto-reindex on next query)")
		}
		if state == "missing" {
			b.WriteString("  (queries for this repo return 'repo not indexed')")
		}
		b.WriteString("\n")
	}
	return b.String(), false
}

// notIndexedNote reports when a queried name belongs to a workspace member
// that is missing/not indexed, so agents never mistake absence for fact.
func notIndexedNote(st *store.Store, name string) string {
	repos, err := st.ListRepos()
	if err != nil {
		return ""
	}
	for _, r := range repos {
		if !r.Missing {
			continue
		}
		if name == r.ModulePath || strings.HasPrefix(name, r.ModulePath+"/") || strings.HasPrefix(name, r.ModulePath+".") {
			return fmt.Sprintf("; note: %s is not indexed (missing checkout)", r.ModulePath)
		}
	}
	return ""
}

// handleWorkspaceIndex indexes every member of the workspace declared by the
// codemap.yaml nearest to absPath into that directory's database.
func (s *Server) handleWorkspaceIndex(absPath string) (string, bool) {
	cfg, err := workspace.Load(workspace.DefaultPath(absPath))
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	wsDBPath := s.dbHomeFor(absPath)
	summaries, err := workspace.IndexAll(cfg, wsDBPath)
	if err != nil {
		return fmt.Sprintf("Error: workspace index: %v", err), true
	}
	var b strings.Builder
	for _, sm := range summaries {
		fmt.Fprintf(&b, "%s: %d packages, %d symbols, %d edges (%s)\n", sm.Repo, sm.Packages, sm.Symbols, sm.Edges, sm.State)
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	st, err := store.Open(wsDBPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	s.store = st
	s.dbPath = wsDBPath
	s.repoPath = absPath

	ccfg, ccfgErr := contractConfigFromWorkspace(cfg)
	if ccfgErr != nil {
		return fmt.Sprintf("Error: contract config: %v", ccfgErr), true
	}
	s.contractCfg = ccfg
	s.hasContractCfg = true
	suppressPairs, pairsErr := cfg.ContractSuppressionPairs()
	if pairsErr != nil {
		return fmt.Sprintf("Error: contract suppression: %v", pairsErr), true
	}
	for _, p := range suppressPairs {
		if err := st.SuppressContracts(p[0], p[1]); err != nil {
			return fmt.Sprintf("Error: recording contract suppression: %v", err), true
		}
	}
	reanalyzed, cerr := contract.ReanalyzeStaleContracts(st, s.contractCfg)
	if cerr != nil {
		return fmt.Sprintf("Error: contract analysis: %v", cerr), true
	}
	for _, repo := range reanalyzed {
		b.WriteString(repo + ": contract analysis refreshed\n")
	}

	return strings.TrimSpace(b.String()), false
}

// finalizeIndexMeta records the working-tree fingerprint and any churn
// degradation on a freshly written index so staleness and hotspot checks have
// honest metadata.
func (s *Server) finalizeIndexMeta(newStore *store.Store, absPath string, churnErr error) error {
	newStore.RecordDirtyFingerprint(absPath, "")
	if err := newStore.RecordChurnDegradation(churnErr); err != nil {
		_ = newStore.Close()
		return fmt.Errorf("set churn degradation: %w", err)
	}
	return nil
}

func (s *Server) handleIndex(args map[string]any) (string, bool) {
	path := "."
	if v, ok := args["path"].(string); ok && v != "" {
		path = v
	}

	absPath, err := s.allowedTarget(path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}

	// Workspace mode: index every member declared by the nearest codemap.yaml.
	if configPath := workspace.DefaultPath(absPath); configPath != "" {
		return s.handleWorkspaceIndex(absPath)
	}

	msg, err := s.indexSingleRepo(absPath)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return msg, false
}

// indexSingleRepo parses, resolves, and writes a single-repo index, swapping it
// into the served store and reporting the resulting package/symbol/edge counts.
func (s *Server) indexSingleRepo(absPath string) (string, error) {
	parseResult, err := parse.Run(absPath)
	if err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}
	resolveResult := resolve.Run(parseResult)

	dbPath := s.dbHomeFor(absPath)
	newStore, err := store.Create(dbPath)
	if err != nil {
		return "", fmt.Errorf("store error: %w", err)
	}

	churn, churnErr := vcs.GitFileChurn(absPath, "HEAD")

	if err := newStore.Write(resolveResult, parse.FileContents(parseResult), churn); err != nil {
		_ = newStore.Close()
		return "", fmt.Errorf("write error: %w", err)
	}
	if err := newStore.SetIndexedAt(time.Now()); err != nil {
		_ = newStore.Close()
		return "", fmt.Errorf("set indexed_at error: %w", err)
	}
	if err := newStore.SetRepoMeta(absPath, gitHead(absPath), len(resolveResult.Packages), len(resolveResult.Symbols)); err != nil {
		_ = newStore.Close()
		return "", fmt.Errorf("set repo meta error: %w", err)
	}
	if err := s.finalizeIndexMeta(newStore, absPath, churnErr); err != nil {
		return "", err
	}

	if s.store != nil {
		_ = s.store.Close()
	}
	s.store = newStore
	s.dbPath = dbPath
	s.repoPath = absPath

	msg := fmt.Sprintf("Indexed %d packages, %d symbols, %d edges",
		len(resolveResult.Packages),
		len(resolveResult.Symbols),
		len(resolveResult.Edges))
	if len(parseResult.Errors) > 0 {
		msg += fmt.Sprintf("; %d package(s) failed to load", len(parseResult.Errors))
	}
	return msg, nil
}

func requiredString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// allowedTarget resolves path to an absolute, symlink-real path and requires it
// to be a known workspace member root or the served repo root. Anything else is
// rejected so a client cannot read, parse, or write to arbitrary directories.
func (s *Server) allowedTarget(path string) (string, error) {
	realPath := evalRoot(path)
	for _, root := range s.allowedRoots() {
		if filepath.Clean(realPath) == filepath.Clean(root) {
			return realPath, nil
		}
	}
	return "", fmt.Errorf("path %s is outside the allowed index roots (%s)",
		path, strings.Join(s.allowedRoots(), ", "))
}

// allowedRoots returns the directories the index/changed_symbols tools may
// target: explicitly configured roots (allowedPaths), the served repo root,
// and the workspace root plus member roots from the nearest codemap.yaml.
func (s *Server) allowedRoots() []string {
	roots := make([]string, 0, 4)
	seen := make(map[string]bool)
	add := func(p string) {
		if p == "" {
			return
		}
		if r := evalRoot(p); r != "" && !seen[r] {
			seen[r] = true
			roots = append(roots, r)
		}
	}
	for _, p := range s.allowedPaths {
		add(p)
	}
	if s.repoPath != "" {
		add(s.repoPath)
	}
	if cfgPath := workspace.DefaultPath(s.repoPath); cfgPath != "" {
		if cfg, err := workspace.Load(cfgPath); err == nil {
			add(cfg.Root)
			for _, m := range cfg.Members {
				add(m.Dir)
			}
		}
	}
	return roots
}

// dbHomeFor derives the DB write location for an allowed index target: the
// configured DB home when one is active, otherwise the target's .codemap dir.
func (s *Server) dbHomeFor(absPath string) string {
	if s.dbPath != "" {
		return s.dbPath
	}
	return filepath.Join(absPath, ".codemap", "codemap.db")
}

// evalRoot absolutizes p and resolves symlinks so path comparisons happen on
// the real filesystem paths.
func evalRoot(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if realPath, err := filepath.EvalSymlinks(abs); err == nil {
		return realPath
	}
	return filepath.Clean(abs)
}

// --- input validation ---

const edgeTypeImports = "imports"

const (
	toolBlastRadius = "blast_radius"
	toolCallersOf   = "callers_of"
	toolCalleesOf   = "callees_of"
	toolContracts   = "contracts"
	toolCycles      = "cycles"
)

var (
	validEdgeTypes    = []string{"calls", "references", "satisfies", "embeds", edgeTypeImports}
	validKinds        = []string{"function", "method", "type", "interface", "const", "var"}
	validSeverities   = []string{"compatible", "breaking", "unknown"}
	validDirections   = []string{"producer", "consumer", "shared"}
	validRuntimeKinds = []string{"redis", "jetstream", "ws_type"}
	validHeuristics   = []string{"main", "test", "uncalled_exported", "handler_sig", "handler_name"}
)

// intArgSpec documents one validated integer argument for a tool.
type intArgSpec struct {
	key string
	min int
	max int
}

// floatArgSpec documents one validated float argument for a tool.
type floatArgSpec struct {
	key  string
	minF float64
	maxF float64
}

// enumArgSpec documents one validated enum argument; slice=true applies the
// check to each element of a []string argument.
type enumArgSpec struct {
	key     string
	allowed []string
	slice   bool
}

var toolIntArgs = map[string][]intArgSpec{
	toolCallersOf:           {{key: keyDepth, min: 1, max: 100}},
	toolCalleesOf:           {{key: keyDepth, min: 1, max: 100}},
	toolBlastRadius:         {{key: keyDepth, min: 1, max: 100}},
	"find_path":             {{key: "max_depth", min: 1, max: 100}},
	"get_hotspots":          {{key: keyTopN, min: 1, max: 500}, {key: "min_complexity", min: 0, max: 1_000_000}, {key: "min_churn", min: 0, max: 1_000_000}},
	"get_symbol_importance": {{key: keyTopN, min: 1, max: 500}, {key: "scope", min: 0, max: 1_000_000}},
	"dependency_layers":     {{key: "top_hubs", min: 1, max: 500}},
	"search_text":           {{key: keyContextLines, min: 0, max: 500}},
	"get_symbol_body":       {{key: keyContextLines, min: 0, max: 500}},
	"get_context_bundle":    {{key: "token_budget", min: 1, max: 1_000_000}},
}

var toolFloatArgs = map[string][]floatArgSpec{
	toolContracts: {{key: "min_confidence", minF: 0, maxF: 1}},
}

var toolEnumArgs = map[string][]enumArgSpec{
	toolCallersOf:       {{key: keyEdgeTypes, allowed: validEdgeTypes, slice: true}},
	toolCalleesOf:       {{key: keyEdgeTypes, allowed: validEdgeTypes, slice: true}},
	"search":            {{key: keyKind, allowed: validKinds}},
	"edges_by_type":     {{key: "edge_type", allowed: validEdgeTypes}},
	toolCycles:          {{key: "edge_type", allowed: validEdgeTypes}},
	toolContracts:       {{key: keyDirection, allowed: validDirections}, {key: keySeverity, allowed: validSeverities}},
	"contract_drift":    {{key: keySeverity, allowed: validSeverities}},
	"runtime_contracts": {{key: keyKind, allowed: validRuntimeKinds}},
	"entry_points":      {{key: "heuristics", allowed: validHeuristics, slice: true}},
}

// validateToolArgs rejects out-of-range numerics and unknown enum values before
// any traversal or search work runs.
func validateToolArgs(tool string, args map[string]any) error {
	for _, spec := range toolIntArgs[tool] {
		if err := validateIntArg(tool, spec.key, args, spec.min, spec.max); err != nil {
			return err
		}
	}
	for _, spec := range toolFloatArgs[tool] {
		if err := validateFloatArg(tool, spec.key, args, spec.minF, spec.maxF); err != nil {
			return err
		}
	}
	for _, spec := range toolEnumArgs[tool] {
		if spec.slice {
			if err := validateStringEnumSlice(tool, spec.key, args, spec.allowed); err != nil {
				return err
			}
			continue
		}
		if err := validateStringArg(tool, spec.key, args, spec.allowed); err != nil {
			return err
		}
	}
	return nil
}

// validateInt is the shared integer-range validator.
func validateInt(tool, name string, v float64, lo, hi int) (int, error) {
	iv := int(v)
	if float64(iv) != v {
		return 0, fmt.Errorf("%s %s must be an integer, got %v", tool, name, v)
	}
	if iv < lo || iv > hi {
		return 0, fmt.Errorf("%s %s must be between %d and %d, got %d", tool, name, lo, hi, iv)
	}
	return iv, nil
}

// validateIntArg validates an optional integer argument against a range.
func validateIntArg(tool, name string, args map[string]any, lo, hi int) error {
	raw, ok := args[name]
	if !ok {
		return nil
	}
	v, ok := raw.(float64)
	if !ok {
		return fmt.Errorf("%s %s must be a number, got %v", tool, name, raw)
	}
	_, err := validateInt(tool, name, v, lo, hi)
	return err
}

// validateFloatArg validates an optional float argument against a range.
func validateFloatArg(tool, name string, args map[string]any, lo, hi float64) error {
	raw, ok := args[name]
	if !ok {
		return nil
	}
	v, ok := raw.(float64)
	if !ok {
		return fmt.Errorf("%s %s must be a number, got %v", tool, name, raw)
	}
	if v < lo || v > hi {
		return fmt.Errorf("%s %s must be between %v and %v, got %v", tool, name, lo, hi, v)
	}
	return nil
}

// validateEnum is the shared enum validator; it names the allowed values so the
// caller can fix the argument without a second round trip.
func validateEnum(tool, name, value string, allowed []string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("%s %s must be one of [%s], got %q", tool, name, strings.Join(allowed, ", "), value)
}

// validateStringArg validates an optional string enum argument.
func validateStringArg(tool, name string, args map[string]any, allowed []string) error {
	v, ok := args[name].(string)
	if !ok || v == "" {
		return nil
	}
	return validateEnum(tool, name, v, allowed)
}

// validateStringEnumSlice validates each string in an optional []string arg.
func validateStringEnumSlice(tool, name string, args map[string]any, allowed []string) error {
	arr, ok := args[name].([]any)
	if !ok {
		return nil
	}
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			continue
		}
		if err := validateEnum(tool, name, s, allowed); err != nil {
			return err
		}
	}
	return nil
}

func appendEdgeTypeFilter(args map[string]any, opts []query.Option) []query.Option {
	v, ok := args[keyEdgeTypes]
	if !ok {
		return opts
	}
	arr, ok := v.([]any)
	if !ok {
		return opts
	}
	types := make([]string, 0, len(arr))
	for _, t := range arr {
		if s, ok := t.(string); ok {
			types = append(types, s)
		}
	}
	if len(types) > 0 {
		opts = append(opts, query.WithEdgeTypes(types...))
	}
	return opts
}

const (
	errQualifiedNameRequired = "Error: qualified_name is required"
	errPackagePathRequired   = "Error: package_path is required"
	errEdgeTypeRequired      = "Error: edge_type is required"
	errTypeNameRequired      = "Error: type_name is required"
	errPrefixRequired        = "Error: prefix is required"
	errPatternRequired       = "Error: pattern is required"
)

const (
	keyQualifiedName = "qualified_name"
	keyKind          = "kind"
	keySignature     = "signature"
	keyPosFile       = "pos_file"
	keyPosLine       = "pos_line"
	keyExported      = "exported"
	keyEdgeType      = "edge_type"
	keyPath          = "path"
	keyRepo          = "repo"
	keyPattern       = "pattern"
	keyIncludeTests  = "include_tests"
	keyDepth         = "depth"
	keyPackagePath   = "package_path"
	keyName          = "name"
	keyTypeName      = "type_name"
	keyType          = "type"
	keyDescription   = "description"
	keyBody          = "body"
	keyToolPackage   = "package"
	keyEvidence      = "evidence"
	keyIndexedAt     = "indexed_at"
	keyTopN          = "top_n"
	keyContextLines  = "context_lines"
	keyEdgeTypes     = "edge_types"
)

const (
	descFullyQualifiedName = "string — fully qualified symbol name"
	descSymbolKind         = "string — symbol kind"
	descSourceFilePath     = "string — source file path"
	descLineNumber         = "int — line number"
	descSourceSymbolQN     = "string — source symbol qualified name"
	descTargetSymbolQN     = "string — target symbol qualified name"
	descEvidence           = "string — evidence for this contract edge"
	descIndexedAt          = "string — ISO 8601 timestamp of analysis"
	descRepoModulePath     = "string — repo module path"
)

var returnTypeSchemas = map[string]any{
	"SymbolDetail": map[string]any{
		keyQualifiedName: "string — fully qualified name (e.g., cli/codemap/extract.Run)",
		keyKind:          "string — symbol kind: function, method, type, interface, const, var",
		"receiver":       "string — receiver type for methods (empty for functions)",
		keySignature:     "string — type signature (e.g., 'func(string) error', 'struct { Name string }')",
		"doc":            "string — documentation comment",
		keyPosFile:       descSourceFilePath,
		keyPosLine:       "int — line number in source file",
		keyExported:      "bool — whether the symbol is exported (capitalized)",
	},
	"EdgeDetail": map[string]any{
		keyFromRef:   descSourceSymbolQN,
		keyToRef:     descTargetSymbolQN,
		keyEdgeType:  "string — relationship type: calls, references, satisfies, embeds, imports",
		keyPosFile:   "string — primary occurrence file (first site)",
		keyPosLine:   "int — primary occurrence line (first site)",
		"sites":      "[]Site — aggregated distinct occurrence locations of this edge ({file, line} objects); primary position is sites[0]",
		"site_count": "int — number of distinct occurrence locations (equals len(sites))",
	},
	"Site": map[string]any{
		"file": "string — repo-relative file path where the edge occurs",
		"line": "int — line number where the edge occurs",
	},
	"SearchResult": map[string]any{
		keyQualifiedName: descFullyQualifiedName,
		keyKind:          descSymbolKind,
		keySignature:     "string — type signature",
		"doc":            "string — documentation comment",
		"receiver":       "string — receiver type for methods",
		keyPosFile:       descSourceFilePath,
		keyPosLine:       descLineNumber,
		keyExported:      "bool — whether exported",
	},
	"ShowResult": map[string]any{
		"symbol":         "SymbolDetail — the symbol being shown",
		"incoming_edges": "[]EdgeDetail — edges pointing TO this symbol",
		"outgoing_edges": "[]EdgeDetail — edges pointing FROM this symbol",
	},
	"PackageSummary": map[string]any{
		keyPath:            "string — package import path",
		keyName:            "string — package name",
		"exported_symbols": "[]SymbolDetail — exported symbols in this package",
		"import_count":     "int — number of imports from this package",
	},
	"PackageResult": map[string]any{
		keyPath:            "string — package import path",
		keyName:            "string — package name",
		"exported_symbols": "[]SymbolDetail — exported symbols",
		"import_count":     "int — number of imports",
	},
	"EdgeTypes": map[string]any{
		"calls":         "A calls B — function/method A invokes function/method B",
		"references":    "A references B — symbol A uses symbol B in a non-call context (field access, variable read, type usage)",
		"satisfies":     "A satisfies B — type A implements interface B (found via callers_of on interfaces)",
		"embeds":        "A embeds B — struct A embeds struct/interface B",
		edgeTypeImports: "A imports B — package A imports package B",
	},
	"SymbolBodyResult": map[string]any{
		keyQualifiedName: "string — fully qualified symbol name",
		keyKind:          descSymbolKind + " (function, method, type, const, var, interface, struct)",
		keyPosFile:       descSourceFilePath,
		keyPosLine:       "int — start line of the declaration",
		"pos_end_line":   "int — end line of the declaration",
		keyBody:          "string — source text of the declaration (and optional doc comment when include_doc=true)",
		"part_of_group":  "bool — true if this is a member of a grouped var/const/type block",
		"group_members":  "int — number of members in the group (when part_of_group=true)",
		"context_before": "string — N file lines before the declaration (when context_lines > 0)",
		"context_after":  "string — N file lines after the declaration (when context_lines > 0)",
	},
	"ChangedSymbol": map[string]any{
		keyQualifiedName: "string — fully qualified symbol name",
		keyKind:          "string — symbol kind",
		"change_type":    "string — modified | added | removed",
		keyPosFile:       descSourceFilePath,
		keyPosLine:       "int — start line",
		toolBlastRadius:  "*BlastRadius — optional blast radius (present when with_blast_radius=true)",
		keyBody:          "string — optional source body (present when include_bodies=true)",
	},
	"ChangedSymbolsSummary": map[string]any{
		"modified":      "int — number of modified symbols",
		"added":         "int — number of added symbols",
		"removed":       "int — number of removed symbols",
		"files_changed": "int — number of .go files in the diff",
	},
	"ChangedSymbolsResult": map[string]any{
		"summary": "ChangedSymbolsSummary — aggregate counts",
		"symbols": "[]ChangedSymbol — per-symbol change details",
	},
	"Layer": map[string]any{
		"level":    "int — Kahn topological level (0 = foundation)",
		"packages": "[]string — package paths at this level",
	},
	"Hub": map[string]any{
		keyToolPackage: "string — package path",
		"fan_in":       "int — number of intra-project importers",
		"fan_out":      "int — number of intra-project imports",
	},
	"LayersResult": map[string]any{
		"layers": "[]Layer — topological layers (Kahn)",
		"hubs":   "[]Hub — top packages ranked by fan-in",
	},
	"FlowResult": map[string]any{
		keyToolPackage:       "string — package path",
		edgeTypeImports:      "[]EdgeDetail — immediate intra-project imports",
		"importers":          "[]EdgeDetail — immediate intra-project importers",
		"transitive_imports": "[]EdgeDetail — transitive intra-project imports",
	},
	"EntryPoint": map[string]any{
		keyQualifiedName: descFullyQualifiedName,
		keyKind:          descSymbolKind,
		keySignature:     "string — symbol signature",
		keyPosFile:       descSourceFilePath,
		keyPosLine:       "int — start line",
		"reason":         "string — heuristic that matched: main | test | uncalled_exported | handler_sig | handler_name",
	},
	"FileMatch": map[string]any{
		"file_path":      "string — path to the matching file",
		"line_number":    "int — line number of the match",
		"line":           "string — the matching line content",
		"context_before": "string — lines before the match",
		"context_after":  "string — lines after the match",
	},
	"Bundle": map[string]any{
		keyQualifiedName: descFullyQualifiedName,
		"symbol":         "SymbolDetail — the symbol details",
		keyBody:          "string — symbol source body",
		"callees":        "[]EdgeDetail — direct callees (depth=1)",
		"callers":        "[]EdgeDetail — direct callers (depth=1)",
		"same_file":      "[]SearchResult — other symbols in the same file",
		"token_estimate": "int — estimated token count",
	},
	"Hotspot": map[string]any{
		"qualified_name": descFullyQualifiedName,
		keyKind:          descSymbolKind,
		"pos_file":       descSourceFilePath,
		"pos_line":       descLineNumber,
		"complexity":     "int — cyclomatic complexity",
		"churn_count":    "int — git commit count for the file",
		"risk_score":     "float64 — complexity * log(churn+1)",
	},
	"ImportanceEntry": map[string]any{
		"qualified_name": descFullyQualifiedName,
		keyKind:          descSymbolKind,
		"pos_file":       descSourceFilePath,
		"pos_line":       descLineNumber,
		"importance":     "float64 — PageRank importance score",
	},
	"Contract": map[string]any{
		"id":         "int — contract ID",
		keyFromRef:   descSourceSymbolQN,
		keyToRef:     descTargetSymbolQN,
		keyDirection: "string — producer, consumer, or shared",
		"confidence": "float64 — confidence score (0-1)",
		keySeverity:  "string — compatible, breaking, or unknown",
		"suggested":  "bool — true if below confidence threshold",
		keyEvidence:  descEvidence,
		keyIndexedAt: descIndexedAt,
		keyRepo:      descRepoModulePath,
	},
	"DriftReport": map[string]any{
		"id":         "int — drift report ID",
		keyFromRef:   descSourceSymbolQN,
		keyToRef:     descTargetSymbolQN,
		keySeverity:  "string — compatible, breaking, or unknown",
		"fields":     "[]DriftField — field-level divergence details",
		keyEvidence:  descEvidence,
		keyIndexedAt: descIndexedAt,
		keyRepo:      descRepoModulePath,
	},
	"DriftField": map[string]any{
		"wire_name": "string — effective wire field name (cbor/json/yaml/toml/bson/db tag, else Go name)",
		"type_a":    "string — type in source repo",
		"type_b":    "string — type in target repo",
		"repo_a":    "string — source repo",
		"repo_b":    "string — target repo",
		"status":    "string — removed, renamed, type_changed, added, same_value_different_name, value_changed",
	},
	"RuntimeContract": map[string]any{
		"id":         "int — runtime contract ID",
		keyKind:      "string — redis, jetstream, or ws_type",
		"pattern":    "string — normalized pattern",
		keyFromRef:   descSourceSymbolQN,
		keyToRef:     descTargetSymbolQN,
		keyDirection: "string — producer, consumer, or shared",
		keyEvidence:  descEvidence,
		keyIndexedAt: descIndexedAt,
		keyRepo:      descRepoModulePath,
	},
}

func handleSchema() string {
	data, err := json.MarshalIndent(returnTypeSchemas, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": "schema marshal failed: %s"}`, err.Error())
	}
	return string(data)
}

func (s *Server) sendResponse(id json.RawMessage, result any) {
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	if err != nil {
		return
	}
	fmt.Println(string(data))
}

func (s *Server) sendError(id json.RawMessage, code int, message string) {
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return
	}
	fmt.Println(string(data))
}

func (s *Server) handleToolsList(id json.RawMessage) {
	s.sendResponse(id, map[string]any{
		"tools": buildToolsList(),
	})
}

func handleSearchText(st *store.Store, qOpts []query.Option, rOpts []render.Option, args map[string]any) (string, bool) {
	pattern := requiredString(args, "pattern")
	if strings.TrimSpace(pattern) == "" {
		return "Error: pattern is required", true
	}
	filePattern := ""
	if v, ok := args["file_pattern"].(string); ok {
		filePattern = v
	}
	isRegex := false
	if v, ok := args["is_regex"].(bool); ok {
		isRegex = v
	}
	contextLines := 0
	if v, ok := args[keyContextLines].(float64); ok {
		contextLines = int(v)
	}
	matches, err := query.SearchText(st, pattern, filePattern, isRegex, contextLines)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	if len(matches) == 0 {
		return healthNotice(st), false
	}
	return render.RenderTextMatches(matches, rOpts...), false
}

func handleContextBundle(st *store.Store, qOpts []query.Option, rOpts []render.Option, args map[string]any) (string, bool) {
	qn := requiredString(args, "qualified_name")
	tokenBudget := 8000
	if v, ok := args["token_budget"].(float64); ok {
		tokenBudget = int(v)
	}
	bundle, err := query.ContextBundle(st, qn, tokenBudget)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderBundle(bundle, rOpts...), false
}

func handleHotspots(st *store.Store, qOpts []query.Option, rOpts []render.Option, args map[string]any) (string, bool) {
	topN := 10
	if v, ok := args[keyTopN].(float64); ok {
		topN = int(v)
	}
	minComplexity := 0
	if v, ok := args["min_complexity"].(float64); ok {
		minComplexity = int(v)
	}
	minChurn := 0
	if v, ok := args["min_churn"].(float64); ok {
		minChurn = int(v)
	}
	hotspots, err := query.Hotspots(st, topN, minComplexity, minChurn)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderHotspots(hotspots, rOpts...), false
}

func handleSymbolImportance(st *store.Store, qOpts []query.Option, rOpts []render.Option, args map[string]any) (string, bool) {
	topN := 10
	if v, ok := args[keyTopN].(float64); ok {
		topN = int(v)
	}
	scope := 0
	if v, ok := args["scope"].(float64); ok {
		scope = int(v)
	}
	entries, err := query.SymbolImportance(st, topN, scope, qOpts...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), true
	}
	return render.RenderImportance(entries, rOpts...), false
}

func buildToolsList() []map[string]any {
	tools := make([]map[string]any, 0, 14)
	tools = append(tools, indexTools()...)
	tools = append(tools, queryTools()...)
	tools = append(tools, sourceTools()...)
	tools = append(tools, shapeTools()...)
	return tools
}

func indexTools() []map[string]any {
	return []map[string]any{
		toolDef("index",
			"Index the Go repository at the given path (or the workspace declared by its codemap.yaml). Indexing happens automatically on first tool call, so this is only needed to force a full re-index. Returns the number of packages, symbols, and edges indexed.",
			map[string]any{
				"path": stringProp("Absolute or relative path to the Go repository root. Defaults to current directory."),
			}),
		toolDef("health",
			"Source of truth for index freshness: per-repo state (indexed/stale/missing) and indexed_at. Use when results look stale or a query misses a symbol you expect to exist.",
			map[string]any{}),
		toolDef("overview",
			"Get a bird's-eye architecture summary of all indexed Go packages: each package's path, exported symbols with signatures, and import counts. Use instead of reading multiple files to understand project structure.",
			map[string]any{
				keyIncludeTests: boolProp("Include test packages and symbols"),
				"full_docs":     boolProp("Show full documentation instead of first sentence"),
			}),
		toolDef("show",
			"Get full details for a Go symbol: file path, line number, type signature, documentation, and all call relationships (who calls it and what it calls). Incoming and outgoing edges are deduplicated per (from, to, type) pair and include aggregated sites and site counts. Use instead of reading source files when you need symbol context and relationships. Requires a fully qualified name like 'fmt.Println' or 'encoding/json.Decoder.Decode'.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name (e.g., cli/codemap/extract.Run)"),
				"full_docs":      boolProp("Show full documentation instead of first sentence"),
				keyRepo:          repoProp(),
			},
			"qualified_name"),
		toolDef(toolCallersOf,
			"Find all callers of a Go function, method, or type: returns caller names, file paths, line numbers, edge types (calls, references, satisfies, embeds, imports), aggregated sites, and site counts. Edges are deduplicated per (from, to, type) pair; each edge carries an aggregated list of distinct call sites. For interfaces, this returns types that implement the interface (satisfies edges). Use instead of grep for tracing call sites and understanding where a symbol is used.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name"),
				keyEdgeTypes:     stringArrayProp("Optional filter for specific edge types (e.g., calls, references, satisfies, embeds, imports)"),
				keyDepth:         intProp("Traversal depth for transitive callers (default: 1 = direct only)"),
				keyRepo:          repoProp(),
			},
			"qualified_name"),
		toolDef(toolCalleesOf,
			"Find all functions and methods called by a Go symbol: returns callee names, file paths, line numbers, edge types (calls, references, satisfies, embeds, imports), aggregated sites, and site counts. Edges are deduplicated per (from, to, type) pair; each edge carries an aggregated list of distinct call sites. For types, this returns embedded types and implemented interfaces. Use to trace dependencies and understand what a function relies on.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name"),
				keyEdgeTypes:     stringArrayProp("Optional filter for specific edge types (e.g., calls, references, satisfies, embeds, imports)"),
				keyDepth:         intProp("Traversal depth for transitive callees (default: 1 = direct only)"),
				keyRepo:          repoProp(),
			},
			"qualified_name"),
	}
}

func queryTools() []map[string]any {
	tools := make([]map[string]any, 0, 12)
	tools = append(tools, schemaTools()...)
	tools = append(tools, symbolTools()...)
	tools = append(tools, graphTools()...)
	tools = append(tools, advancedTools()...)
	tools = append(tools, contractTools()...)
	return tools
}

func schemaTools() []map[string]any {
	return []map[string]any{
		toolDef("schema",
			"Returns the schema of all result types returned by codemap tools. Call this first to understand the structure of responses before interpreting results.",
			map[string]any{}),
	}
}

func symbolTools() []map[string]any {
	return []map[string]any{
		toolDef("package",
			"Get all exported (or unexported) symbols for a single Go package: names, kinds, signatures, receivers, file paths, line numbers, and documentation. Use instead of reading package files one by one to understand a package's API.",
			map[string]any{
				"path":               stringProp("Package import path"),
				"include_unexported": boolProp("Include unexported symbols"),
				keyRepo:              repoProp(),
			},
			"path"),
		toolDef("methods_of",
			"Find all methods on a Go type by its short name (e.g., 'Server', 'Store'). Returns each method's qualified name, signature, file path, line number, and documentation. Use instead of grep when you need a type's full method set.",
			map[string]any{
				keyTypeName: stringProp("Short type name (e.g., Store)"),
				keyRepo:     repoProp(),
			},
			"type_name"),
		toolDef("search",
			"Search Go symbols (functions, types, methods, interfaces, constants, variables) by name. Returns qualified names, file paths, line numbers, signatures, and documentation. More precise than grep for finding Go symbol definitions and declarations. Use this when you know a symbol name but not its location.",
			map[string]any{
				keyPattern:      stringProp("Search pattern (case-insensitive substring match)"),
				keyIncludeTests: boolProp("Include test packages and symbols"),
				keyKind:         stringProp("Filter by symbol kind (e.g., function, method, type, const, var, interface)"),
				"exported":      boolProp("Filter by exported status"),
				"file":          stringProp("Filter by file path (substring match on pos_file, e.g., 'server.go' or 'mcp/')"),
				keyRepo:         repoProp(),
			},
			"pattern"),
		toolDef("list_packages",
			"List all indexed Go packages with their import paths, names, and symbol counts. Use to discover what packages exist in the codebase before diving into specific symbols.",
			map[string]any{
				keyIncludeTests: boolProp("Include test packages"),
			}),
	}
}

func graphTools() []map[string]any {
	return []map[string]any{
		toolDef("importers_of",
			"Find all Go packages that import the given package. Returns package paths and line numbers. Use to understand a package's downstream dependents and blast radius of changes.",
			map[string]any{
				keyPackagePath: stringProp("Package import path"),
				"repo":         repoProp(),
			},
			"package_path"),
		toolDef("imports_of",
			"Find all packages imported by the given Go package. Returns import paths and line numbers. Use to understand a package's upstream dependencies.",
			map[string]any{
				keyPackagePath: stringProp("Package import path"),
				"repo":         repoProp(),
			},
			"package_path"),
		toolDef("edges_by_type",
			"List all edges of a specific relationship type (calls, references, satisfies, embeds, imports) across the entire indexed codebase. Edges are deduplicated per (from, to, type) pair and include aggregated sites and site counts. Use to trace a specific category of relationships globally.",
			map[string]any{
				keyEdgeType: stringProp("Edge type to filter by (e.g., calls, references, satisfies, embeds, imports)"),
			},
			"edge_type"),
		toolDef("all_edges",
			"List every relationship edge in the indexed codebase. Returns from/to symbol references, edge types, and source locations. Intended for bulk analysis; prefer callers_of or callees_of for targeted queries.",
			map[string]any{}),
		toolDef("search_text",
			"Search file contents using FTS5 full-text search or regex. Returns matching file paths, line numbers, and context lines. Use for finding text patterns in source code.",
			map[string]any{
				keyPattern:      stringProp("Search pattern (FTS5 query or regex)"),
				"file_pattern":  stringProp("File path filter pattern (substring match, optional)"),
				"is_regex":      boolProp("Use regex mode instead of FTS5 (default false)"),
				keyContextLines: intProp("Number of context lines around each match (default 0)"),
			},
			"pattern"),
		toolDef("get_context_bundle",
			"Bundle a symbol with its body, callees, callers, and same-file symbols into a single context window. Estimates token count and trims to budget. Use for LLM context preparation.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name"),
				"token_budget":   intProp("Maximum token budget for the bundle (default 8000)"),
			},
			"qualified_name"),
		toolDef("get_hotspots",
			"Find code hotspots by combining cyclomatic complexity with git churn. Returns symbols ranked by risk score (complexity * log(churn+1)). Use to identify risky code for refactoring.",
			map[string]any{
				keyTopN:          intProp("Number of hotspots to return (default 10)"),
				"min_complexity": intProp("Minimum complexity threshold (default 0)"),
				"min_churn":      intProp("Minimum churn count threshold (default 0)"),
			}),
		toolDef("get_symbol_importance",
			"Compute symbol importance using PageRank over symbol-to-symbol edges (calls + references + satisfies). Ranks symbols by real call-graph centrality; hub symbols outrank leaves. The `scope` parameter is reserved and has no effect.",
			map[string]any{
				keyTopN: intProp("Number of results to return (default 10)"),
				"scope": intProp("Reserved; accepted for compatibility, has no effect"),
			}),
	}
}

func advancedTools() []map[string]any {
	tools := make([]map[string]any, 0, 10)
	tools = append(tools, typeAndImportTools()...)
	tools = append(tools, analysisTools()...)
	tools = append(tools, pathAndSearchTools()...)
	return tools
}

func typeAndImportTools() []map[string]any {
	return []map[string]any{
		toolDef("type_usage",
			"Find all symbols that use a given type in their signatures (parameters, return types, fields). Returns qualified names, kinds, signatures, and file locations. Use to find where a type is used across the codebase.",
			map[string]any{
				keyTypeName:     stringProp("Type name to search for (e.g., 'error', 'string', 'MyStruct')"),
				keyIncludeTests: boolProp("Include test packages and symbols"),
			},
			"type_name"),
		toolDef("transitive_imports",
			"Find all packages transitively imported by a given package (direct and indirect dependencies). Returns import edges with file paths and line numbers. Use to understand the full dependency tree.",
			map[string]any{
				keyPackagePath: stringProp("Package import path"),
			},
			"package_path"),
		toolDef("search_prefix",
			"Search Go symbols by qualified name prefix. Returns all symbols whose qualified name starts with the given prefix. Use to find all symbols in a package or subpackage.",
			map[string]any{
				"prefix":        stringProp("Qualified name prefix (e.g., 'cli/codemap/' or 'encoding/json.Decoder')"),
				keyIncludeTests: boolProp("Include test packages and symbols"),
			},
			"prefix"),
	}
}

func analysisTools() []map[string]any {
	return []map[string]any{
		toolDef("interface_impls",
			"Find all types that implement a given interface and their implementing methods. Returns interface name, implementing type, and method qualified names. Use to find all implementations of an interface.",
			map[string]any{
				"interface_name": stringProp("Interface qualified name"),
			},
			"interface_name"),
		toolDef("unused",
			"Find all unexported symbols that have zero incoming callers/references. Returns symbol names, kinds, signatures, and file locations. Use to find dead code.",
			map[string]any{
				keyIncludeTests: boolProp("Include test packages and symbols"),
			}),
		toolDef(toolCycles,
			"Detect cycles in a specific edge type graph (e.g., imports, calls). Returns every distinct cycle as a canonicalized path, rotated to its smallest node, in deterministic sorted order. Results are capped at 1000 distinct cycles; when the cap is hit the last returned cycle carries a `truncated` flag. Use to find circular dependencies or call cycles.",
			map[string]any{
				keyEdgeType: stringProp("Edge type to check for cycles (e.g., 'imports', 'calls')"),
			},
			"edge_type"),
		toolDef(toolBlastRadius,
			"Analyze the impact of changing a symbol: direct callers, transitive callers, interface implementations, embedders, and type users. Returns a summary of impact metrics. Use before refactoring to understand blast radius.",
			map[string]any{
				keyQualifiedName: stringProp("Symbol qualified name"),
				keyDepth:         intProp("Transitive caller depth (default: 3)"),
				keyRepo:          repoProp(),
			},
			"qualified_name"),
	}
}

func pathAndSearchTools() []map[string]any {
	return []map[string]any{
		toolDef("find_path",
			"Find a call/reference path between two symbols using BFS. Returns the path as a sequence of edges. Use to understand how two symbols are connected in the code graph.",
			map[string]any{
				"from":      stringProp("Starting symbol qualified name"),
				"to":        stringProp("Target symbol qualified name"),
				"max_depth": intProp("Maximum traversal depth (default: 10)"),
				keyRepo:     repoProp(),
			},
			"from", "to"),
		toolDef("method_search",
			"Search for all methods with a given name across all types. Returns each method's qualified name, receiver type, signature, and file location. Use to find all implementations of a method name.",
			map[string]any{
				"method_name":   stringProp("Method name to search for (e.g., 'Read', 'GetName')"),
				keyIncludeTests: boolProp("Include test packages and symbols"),
			},
			"method_name"),
		toolDef("symbols_in_file",
			"Find all symbols defined in a specific file. Returns symbol names, kinds, signatures, and line numbers. Use to understand what a file defines.",
			map[string]any{
				"file_path": stringProp("File path (substring match, e.g., 'server.go' or 'mcp/server.go')"),
			},
			"file_path"),
	}
}

func toolDef(name, description string, properties map[string]any, required ...string) map[string]any {
	t := map[string]any{
		keyName:        name,
		keyDescription: description + staleGuidance,
		"inputSchema": map[string]any{
			keyType:      "object",
			"properties": properties,
		},
	}
	if len(required) > 0 {
		t["inputSchema"].(map[string]any)["required"] = required
	}
	return t
}

// staleGuidance is appended to every tool description so agents know results
// reflect a fresh index and how to force a manual one.
const staleGuidance = " Results reflect a fresh index — codemap re-indexes any stale repo before answering. If you still suspect stale data, run `codemap index` (or `codemap index --workspace`)."

// repoProp is the optional repo scope property shared by query tools.
func repoProp() map[string]any {
	return stringProp("Optional workspace member (module path) to scope results to (e.g. 'krucil'). Workspace mode only.")
}

func stringProp(desc string) map[string]any {
	return map[string]any{keyType: "string", keyDescription: desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{keyType: "boolean", keyDescription: desc}
}

func stringArrayProp(desc string) map[string]any {
	return map[string]any{
		keyType:        "array",
		"items":        map[string]any{keyType: "string"},
		keyDescription: desc,
	}
}

func intProp(desc string) map[string]any {
	return map[string]any{keyType: "integer", keyDescription: desc}
}

func sourceTools() []map[string]any {
	return []map[string]any{
		toolDef("get_symbol_body",
			"Get the source text of a single Go symbol (function, method, type, const, or var) by qualified name. Re-parses the file at query time to extract the exact declaration span, with optional doc comment and context-line padding. Replaces read(whole_file) for 'show me this one symbol' lookups with ~20x token reduction.",
			map[string]any{
				keyQualifiedName: stringProp("Fully qualified symbol name (e.g., cli/codemap/extract.Run)"),
				keyContextLines:  intProp("Number of file lines to include before and after the declaration as context (default 0)"),
				"include_doc":    boolProp("Prepend the leading doc comment to the body (default true)"),
			},
			"qualified_name"),
		toolDef("changed_symbols",
			"Get the symbols changed in the working tree relative to a git ref (default 'main'), classified by change_type (modified/added/removed). Each changed symbol can carry an optional blast_radius and source body. Replaces 'git diff' + manual hunk-to-symbol mapping + per-symbol blast_radius lookups.",
			map[string]any{
				"ref":               stringProp("Git ref to diff against (default: repository's default branch, e.g. main or master)"),
				"with_blast_radius": boolProp("Attach blast_radius to each changed symbol (default true)"),
				"include_bodies":    boolProp("Attach source body to each changed symbol (default false)"),
				"include_tests":     boolProp("Include test packages/symbols (default false)"),
			}),
		toolDef("workspace_changed_symbols",
			"Get a workspace-wide diff of changed symbols: every member repo's changed symbols, each computed against that repo's own reference and tagged with its repo. Replaces running changed_symbols per repo manually.",
			map[string]any{
				"with_blast_radius": boolProp("Attach blast_radius to each changed symbol (default true)"),
				"include_bodies":    boolProp("Attach source body to each changed symbol (default false)"),
				"include_tests":     boolProp("Include test packages/symbols (default false)"),
			}),
	}
}

func shapeTools() []map[string]any {
	return []map[string]any{
		toolDef("dependency_layers",
			"Get a topological layering of the project's packages based on the stored 'imports' edges (Kahn's algorithm), plus the top packages ranked by fan-in. Scoped to intra-project packages; standard library and third-party imports are excluded. Replaces N calls to importers_of to judge 'is this a hub' or 'what's the layer order'.",
			map[string]any{
				keyIncludeTests: boolProp("Include _test packages in the layering (default false)"),
				"top_hubs":      intProp("Maximum number of hub entries to return (default 5)"),
			}),
		toolDef("dependency_flow",
			"Get the immediate imports, immediate importers, and transitive imports of a single package in one call. Composes imports_of, importers_of, and transitive_imports.",
			map[string]any{
				keyPackagePath: stringProp("Package import path"),
			},
			"package_path"),
		toolDef("entry_points",
			"Get the entry points of the codebase: main() functions, test entry symbols, exported funcs/methods with no incoming callers, and optionally HTTP handlers. Use to surface 'where do I start reading' without grep.",
			map[string]any{
				"heuristics":    stringArrayProp("Heuristic set to apply: main, test, uncalled_exported, handler_sig, handler_name (default [\"main\",\"test\",\"uncalled_exported\"])"),
				keyIncludeTests: boolProp("Include _test packages; required to surface test-entry symbols (default false)"),
			}),
	}
}
