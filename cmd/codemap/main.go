package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"codemap/cmd/codemap/initcmd"
	"codemap/mcp"
	"codemap/parse"
	"codemap/query"
	"codemap/render"
	"codemap/resolve"
	"codemap/store"
	"codemap/vcs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type parsedFlags struct {
	exportedFilter    *bool
	kindFilter        string
	packageFilter     string
	dbPath            string
	edgeTypes         []string
	format            render.Format
	includeTests      bool
	fullDocs          bool
	includeUnexported bool
	regex             bool
	contextLines      int
	topN              int
	minComplexity     int
	minChurn          int
}

func parseArgs(args []string, defaultDBPath string) (parsedFlags, []string) {
	flags := parsedFlags{format: render.FormatTOON, dbPath: defaultDBPath}
	var filteredArgs []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if handled, newI := handleFlag(arg, args, i, &flags); handled {
			i = newI
		} else if arg != "" {
			filteredArgs = append(filteredArgs, arg)
		}
	}

	return flags, filteredArgs
}

func handleFlag(arg string, args []string, i int, flags *parsedFlags) (bool, int) {
	switch arg {
	case "--include-tests":
		flags.includeTests = true
	case "--include-unexported":
		flags.includeUnexported = true
	case "--full-docs":
		flags.fullDocs = true
	case "--json":
		flags.format = render.FormatJSON
	case "--toon":
		flags.format = render.FormatTOON
	case "--compact":
		flags.format = render.FormatCompact
	case "--db":
		if i+1 < len(args) {
			flags.dbPath = args[i+1]
			return true, i + 1
		}
	case "--edge-types":
		if i+1 < len(args) {
			flags.edgeTypes = splitCSV(args[i+1])
			return true, i + 1
		}
	case "--kind":
		return consumeStringArg(args, i, &flags.kindFilter)
	case "--exported":
		return handleExportedFlag(args, i, flags)
	case "--package":
		return consumeStringArg(args, i, &flags.packageFilter)
	case "--regex":
		flags.regex = true
	case "--context-lines":
		return consumeIntArg(args, i, &flags.contextLines)
	case "--top-n":
		return consumeIntArg(args, i, &flags.topN)
	case "--min-complexity":
		return consumeIntArg(args, i, &flags.minComplexity)
	case "--min-churn":
		return consumeIntArg(args, i, &flags.minChurn)
	default:
		return false, i
	}
	return false, i
}

func consumeStringArg(args []string, i int, target *string) (bool, int) {
	if i+1 < len(args) {
		*target = args[i+1]
		return true, i + 1
	}
	return false, i
}

func consumeIntArg(args []string, i int, target *int) (bool, int) {
	if i+1 < len(args) {
		if v, err := strconv.Atoi(args[i+1]); err == nil {
			*target = v
		}
		return true, i + 1
	}
	return false, i
}

func handleExportedFlag(args []string, i int, flags *parsedFlags) (bool, int) {
	if i+1 < len(args) {
		flags.exportedFilter = parseBool(args[i+1])
		return true, i + 1
	}
	flags.exportedFilter = new(bool)
	*flags.exportedFilter = true
	return false, i
}

func splitCSV(s string) []string {
	var result []string
	for t := range strings.SplitSeq(s, ",") {
		trimmed := strings.TrimSpace(t)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func run() error {
	if len(os.Args) < 2 {
		printUsage()
		return fmt.Errorf("no command provided")
	}

	cmd := os.Args[1]
	flags, filteredArgs := parseArgs(os.Args[2:], store.DefaultPath())

	queryOpts := buildQueryOpts(flags)
	renderOpts := buildRenderOpts(flags)

	return dispatchCommand(cmd, filteredArgs, flags.dbPath, queryOpts, renderOpts)
}

func buildQueryOpts(flags parsedFlags) []query.Option {
	var opts []query.Option
	if flags.includeTests {
		opts = append(opts, query.WithTests())
	}
	if len(flags.edgeTypes) > 0 {
		opts = append(opts, query.WithEdgeTypes(flags.edgeTypes...))
	}
	if flags.kindFilter != "" {
		opts = append(opts, query.WithKind(flags.kindFilter))
	}
	if flags.exportedFilter != nil {
		opts = append(opts, query.WithExported(*flags.exportedFilter))
	}
	if flags.packageFilter != "" {
		opts = append(opts, query.WithPackage(flags.packageFilter))
	}
	if flags.includeUnexported {
		opts = append(opts, query.WithUnexported())
	}
	return opts
}

func buildRenderOpts(flags parsedFlags) []render.Option {
	opts := []render.Option{render.WithFormat(flags.format)}
	if flags.fullDocs {
		opts = append(opts, render.WithFullDocs())
	}
	return opts
}

func dispatchCommand(cmd string, args []string, dbPath string, queryOpts []query.Option, renderOpts []render.Option) error {
	switch cmd {
	case "index":
		path := "."
		if len(args) > 0 {
			path = args[0]
		}
		cmdIndex(path, dbPath)
	case "init", "inject":
		cmdInit()
	case "serve":
		cmdServe(dbPath)
	default:
		return dispatchQueryCommand(cmd, args, dbPath, queryOpts, renderOpts)
	}
	return nil
}

func dispatchQueryCommand(cmd string, args []string, dbPath string, queryOpts []query.Option, renderOpts []render.Option) error {
	switch cmd {
	case "overview":
		cmdOverview(dbPath, queryOpts, renderOpts)
	case "show":
		return withRequiredArg(args, "show", func(arg string) { cmdShow(arg, dbPath, queryOpts, renderOpts) })
	case "callers-of":
		return withRequiredArg(args, "callers-of", func(arg string) { cmdCallersOf(arg, dbPath, queryOpts, renderOpts) })
	case "callees-of":
		return withRequiredArg(args, "callees-of", func(arg string) { cmdCalleesOf(arg, dbPath, queryOpts, renderOpts) })
	case "package":
		return withRequiredArg(args, "package", func(arg string) { cmdPackage(arg, dbPath, queryOpts, renderOpts) })
	case "methods-of":
		return withRequiredArg(args, "methods-of", func(arg string) { cmdMethodsOf(arg, dbPath, queryOpts, renderOpts) })
	case "search":
		return withRequiredArg(args, "search", func(arg string) { cmdSearch(arg, dbPath, queryOpts, renderOpts) })
	case "importers-of":
		return withRequiredArg(args, "importers-of", func(arg string) { cmdImportersOf(arg, dbPath, queryOpts, renderOpts) })
	case "imports-of":
		return withRequiredArg(args, "imports-of", func(arg string) { cmdImportsOf(arg, dbPath, queryOpts, renderOpts) })
	case "edges-by-type":
		return withRequiredArg(args, "edges-by-type", func(arg string) { cmdEdgesByType(arg, dbPath, queryOpts, renderOpts) })
	case "all-edges":
		cmdAllEdges(dbPath, queryOpts, renderOpts)
	case "list-packages":
		cmdListPackages(dbPath, queryOpts, renderOpts)
	case "search-text":
		return withRequiredArg(args, "search-text", func(arg string) { cmdSearchText(arg, dbPath, renderOpts) })
	case "hotspots":
		cmdHotspots(dbPath, renderOpts)
	case "importance":
		cmdImportance(dbPath, renderOpts)
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", cmd)
	}
	return nil
}

func withRequiredArg(args []string, name string, fn func(string)) error {
	if len(args) < 1 {
		return fmt.Errorf("%s requires an argument", name)
	}
	fn(args[0])
	return nil
}

func cmdIndex(path, dbPath string) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Parse error: %v\n", err)
		return
	}

	if len(parseResult.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "Parse warnings:\n")
		for _, e := range parseResult.Errors {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", e.File, e.Err)
		}
	}

	resolveResult := resolve.Run(parseResult)

	s, err := store.Create(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Store error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	churn, _ := vcs.GitFileChurn(absPath, "HEAD")

	if err := s.Write(resolveResult, parse.FileContents(parseResult), churn); err != nil {
		fmt.Fprintf(os.Stderr, "Write error: %v\n", err)
		return
	}

	if err := s.SetIndexedAt(time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "Set indexed_at error: %v\n", err)
		return
	}

	head, _ := vcs.GitHead(absPath)
	if err := s.SetRepoMeta(absPath, head, len(resolveResult.Packages), len(resolveResult.Symbols)); err != nil {
		fmt.Fprintf(os.Stderr, "Set repo meta error: %v\n", err)
		return
	}

	fmt.Printf("Indexed %d packages, %d symbols, %d edges\n",
		len(resolveResult.Packages),
		len(resolveResult.Symbols),
		len(resolveResult.Edges))
}

func cmdOverview(dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	result, err := query.Overview(s, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderOverview(result, rOpts...))
}

func cmdShow(qn, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	result, err := query.Show(s, qn, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderShow(result, rOpts...))
}

func cmdCallersOf(qn, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	edges, err := query.CallersOf(s, qn, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderCallers(edges, rOpts...))
}

func cmdCalleesOf(qn, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	edges, err := query.CalleesOf(s, qn, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderCallees(edges, rOpts...))
}

func cmdPackage(pkgPath, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	result, err := query.Package(s, pkgPath, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderPackage(result, rOpts...))
}

func cmdMethodsOf(typeName, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	methods, err := query.MethodsOf(s, typeName, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderMethodsOf(methods, rOpts...))
}

func cmdImportersOf(pkgPath, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	edges, err := query.ImportersOf(s, pkgPath, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderEdges(edges, rOpts...))
}

func cmdImportsOf(pkgPath, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	edges, err := query.ImportsOf(s, pkgPath, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderEdges(edges, rOpts...))
}

func cmdEdgesByType(edgeType, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	edges, err := query.EdgesByType(s, edgeType, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderEdges(edges, rOpts...))
}

func cmdAllEdges(dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	edges, err := query.AllEdges(s, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderEdges(edges, rOpts...))
}

func cmdSearch(pattern, dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	results, err := query.Search(s, pattern, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderSearch(results, rOpts...))
}

func cmdListPackages(dbPath string, qOpts []query.Option, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	pkgs, err := query.ListPackages(s, qOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	fmt.Print(render.RenderListPackages(pkgs, rOpts...))
}

func cmdInit() {
	if err := initcmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
}

func cmdServe(dbPath string) {
	server := mcp.NewLazy(dbPath)
	if err := server.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		return
	}
}

func cmdSearchText(pattern, dbPath string, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	flags, _ := parseArgs(os.Args[2:], store.DefaultPath())
	filePattern := ""
	if len(os.Args) > 3 {
		filePattern = os.Args[3]
	}
	matches, err := query.SearchText(s, pattern, filePattern, flags.regex, flags.contextLines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	fmt.Print(render.RenderTextMatches(matches, rOpts...))
}

func cmdHotspots(dbPath string, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	flags, _ := parseArgs(os.Args[2:], store.DefaultPath())
	topN := flags.topN
	if topN <= 0 {
		topN = 10
	}
	hotspots, err := query.Hotspots(s, topN, flags.minComplexity, flags.minChurn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	fmt.Print(render.RenderHotspots(hotspots, rOpts...))
}

func cmdImportance(dbPath string, rOpts []render.Option) {
	s, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer func() { _ = s.Close() }()

	flags, _ := parseArgs(os.Args[2:], store.DefaultPath())
	topN := flags.topN
	if topN <= 0 {
		topN = 10
	}
	entries, err := query.SymbolImportance(s, topN, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	fmt.Print(render.RenderImportance(entries, rOpts...))
}

func printUsage() {
	fmt.Println("Usage: codemap <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  inject (or init)          Install opencode plugin + context files for codemap integration")
	fmt.Println("  index [path]              Index a Go repository")
	fmt.Println("  overview                  Show package overview")
	fmt.Println("  show <qualified_name>     Show symbol details")
	fmt.Println("  callers-of <name>         Find callers of a symbol")
	fmt.Println("  callees-of <name>         Find callees of a symbol")
	fmt.Println("  search <pattern>          Search symbols")
	fmt.Println("  importers-of <pkg>        List packages that import the given package")
	fmt.Println("  imports-of <pkg>          List packages imported by the given package")
	fmt.Println("  edges-by-type <type>      List all edges of a given type")
	fmt.Println("  all-edges                 List all edges")
	fmt.Println("  list-packages             List all packages")
	fmt.Println("  package <path>            Show package details")
	fmt.Println("  methods-of <type_name>    Show methods of a type")
	fmt.Println("  search-text <pattern>     Search file contents (FTS5 or regex)")
	fmt.Println("  hotspots                  Show code hotspots (complexity x churn)")
	fmt.Println("  importance                Show symbol importance (PageRank)")
	fmt.Println("  serve                     Start MCP server")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --include-tests           Include test packages and symbols")
	fmt.Println("  --include-unexported      Include unexported symbols (for package)")
	fmt.Println("  --full-docs               Show full documentation")
	fmt.Println("  --toon                    Output as TOON (default)")
	fmt.Println("  --json                    Output as JSON")
	fmt.Println("  --compact                 Compact output")
	fmt.Println("  --db <path>               Database path (default: .codemap/codemap.db)")
	fmt.Println("  --edge-types <types>      Filter edge types (comma-separated, for callers-of/callees-of)")
	fmt.Println("  --kind <kind>             Filter by symbol kind (for search)")
	fmt.Println("  --exported [bool]         Filter by exported status (for search)")
	fmt.Println("  --package <pkg>           Filter by package path (for search)")
	fmt.Println("  --regex                   Use regex mode (for search-text)")
	fmt.Println("  --context-lines <n>       Context lines around matches (for search-text)")
	fmt.Println("  --top-n <n>               Number of results (for hotspots/importance)")
	fmt.Println("  --min-complexity <n>      Minimum complexity (for hotspots)")
	fmt.Println("  --min-churn <n>           Minimum churn (for hotspots)")
}

func parseBool(s string) *bool {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return nil
	}
	return &v
}
