package initcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const agentsContent = `# AGENTS.md

This project uses ` + "`codemap`" + ` MCP tools for Go codebase analysis. These tools are faster and more precise than grep/glob for understanding Go code structure, call relationships, and symbol locations.

## When to Use Codemap vs grep/glob/read

| You want... | Use this | Not this |
|---|---|---|
| Find a function/type by name | ` + "`codemap_search`" + ` | grep |
| Understand a symbol's signature, docs, callers, callees | ` + "`codemap_show`" + ` | reading source files |
| Find who calls a function | ` + "`codemap_callers_of`" + ` | grep for function name |
| Find what a function depends on | ` + "`codemap_callees_of`" + ` | reading imports |
| Get a package's full API | ` + "`codemap_package`" + ` | listing .go files |
| Find all methods on a type | ` + "`codemap_methods_of`" + ` | grep for receiver patterns |
| See project architecture | ` + "`codemap_overview`" + ` | guessing from directory structure |
| Find where a package is imported | ` + "`codemap_importers_of`" + ` | grep for import path |
| Find what a package depends on | ` + "`codemap_imports_of`" + ` | reading import blocks |
| List all packages | ` + "`codemap_list_packages`" + ` | ls and guess |

## Available Tools

- ` + "`codemap_index`" + ` — Force a full re-index. Indexing happens automatically on first tool call.
- ` + "`codemap_overview`" + ` — Architecture summary: packages, exported symbols, import counts.
- ` + "`codemap_search`" + ` — Search symbols by name. Returns qualified names, file paths, line numbers, signatures.
- ` + "`codemap_show`" + ` — Full symbol detail: signature, docs, file:line, incoming/outgoing edges.
- ` + "`codemap_callers_of`" + ` — Who calls this symbol? Returns caller names, file paths, line numbers.
- ` + "`codemap_callees_of`" + ` — What does this symbol call? Returns callee names, file paths, line numbers.
- ` + "`codemap_package`" + ` — All symbols in a package with signatures and docs.
- ` + "`codemap_methods_of`" + ` — All methods on a type (short name like ` + "`Store`" + `).
- ` + "`codemap_list_packages`" + ` — All indexed packages with paths and symbol counts.
- ` + "`codemap_importers_of`" + ` — Packages that import the given package.
- ` + "`codemap_imports_of`" + ` — Packages imported by the given package.
- ` + "`codemap_edges_by_type`" + ` — All edges of a specific type (calls, references, satisfies, embeds, imports).
- ` + "`codemap_all_edges`" + ` — Every relationship edge in the index.

## Tips

- Qualified names use ` + "`package/path.SymbolName`" + ` format
- Set ` + "`include_tests: true`" + ` to include test packages/symbols
- Set ` + "`full_docs: true`" + ` to see complete documentation
- Search is case-insensitive substring match
- Indexing is automatic and fast (~50ms). Re-indexes when Go files change.
`

const opencodeNavContent = `<!-- Context: codemap/navigation | Priority: high | Version: 1.2 -->

# Codemap MCP Tools

Codemap provides Go code analysis via MCP tools. **Use codemap tools instead of grep/glob/read for any Go codebase navigation** — they return file paths, line numbers, signatures, and call relationships that grep cannot.

## How It Works

Indexing is **automatic** — the first tool call triggers indexing if needed (~50ms). If Go files change, the index refreshes automatically. No manual ` + "`codemap_index`" + ` call required.

## Decision Guide

| You want... | Use | Instead of |
|---|---|---|
| Find a function/type by name | ` + "`codemap_search`" + ` | grep |
| See a symbol's signature and docs | ` + "`codemap_show`" + ` | reading source files |
| Who calls this function? | ` + "`codemap_callers_of`" + ` | grep for function name |
| What does this function call? | ` + "`codemap_callees_of`" + ` | reading code |
| Get a package's full API | ` + "`codemap_package`" + ` | listing .go files |
| All methods on a type | ` + "`codemap_methods_of`" + ` | grep for receiver |
| Project architecture overview | ` + "`codemap_overview`" + ` | guessing from dirs |
| Where is this package imported? | ` + "`codemap_importers_of`" + ` | grep for import path |
| What does this package depend on? | ` + "`codemap_imports_of`" + ` | reading import blocks |

## Available Tools

| Tool | Returns | When to use |
|---|---|---|
| ` + "`codemap_index`" + ` | Packages, symbols, edges count | Force re-index (automatic otherwise) |
| ` + "`codemap_overview`" + ` | Each package's exports + signatures | Understanding project structure |
| ` + "`codemap_search`" + ` | Qualified names, file:line, signatures, docs | Finding symbols by name |
| ` + "`codemap_show`" + ` | Signature, docs, file:line, incoming+outgoing edges | Understanding a single symbol |
| ` + "`codemap_callers_of`" + ` | Caller names, file:line, edge types | Tracing who uses a function |
| ` + "`codemap_callees_of`" + ` | Callee names, file:line, edge types | Tracing dependencies |
| ` + "`codemap_package`" + ` | All symbols with signatures and docs | Inspecting a package's API |
| ` + "`codemap_methods_of`" + ` | Method names, signatures, file:line | Finding a type's method set |
| ` + "`codemap_list_packages`" + ` | Import paths, names, symbol counts | Discovering packages |
| ` + "`codemap_importers_of`" + ` | Importing packages with file:line | Finding downstream dependents |
| ` + "`codemap_imports_of`" + ` | Imported packages with file:line | Finding upstream dependencies |
| ` + "`codemap_edges_by_type`" + ` | All edges of a given type | Bulk relationship analysis |
| ` + "`codemap_all_edges`" + ` | Every edge in the codebase | Full graph export |

## Tips

- Qualified names: ` + "`package/path.SymbolName`" + ` (e.g., ` + "`encoding/json.Decoder.Decode`" + `)
- ` + "`include_tests: true`" + ` — include test packages/symbols
- ` + "`full_docs: true`" + ` — full doc comments instead of first sentence only
- ` + "`include_unexported: true`" + ` — include private symbols (package tool)
- Search is case-insensitive substring match
- ` + "`methods_of`" + ` takes a short type name (e.g., ` + "`Store`" + `, not the full path)
`

func Run() error {
	errors := false

	if err := injectCrush(); err != nil {
		fmt.Fprintf(os.Stderr, "crush: %v\n", err)
		errors = true
	}

	if err := injectOpencode(); err != nil {
		fmt.Fprintf(os.Stderr, "opencode: %v\n", err)
		errors = true
	}

	if err := injectAgents(); err != nil {
		fmt.Fprintf(os.Stderr, "agents: %v\n", err)
		errors = true
	}

	if errors {
		return fmt.Errorf("one or more injections failed")
	}
	return nil
}

func injectCrush() error {
	cfgPath := filepath.Join(os.Getenv("HOME"), ".config", "crush", "crush.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("crush config not found at %s: %w", cfgPath, err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing crush config: %w", err)
	}

	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		mcp = make(map[string]any)
		cfg["mcp"] = mcp
	}

	if _, exists := mcp["codemap"]; exists {
		fmt.Println("crush: codemap MCP entry already exists, skipping")
		return nil
	}

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding codemap binary: %w", err)
	}

	mcp["codemap"] = map[string]any{
		"type":    "stdio",
		"command": selfPath,
		"args":    []any{"serve"},
	}

	updated, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling crush config: %w", err)
	}

	if err := os.WriteFile(cfgPath, updated, 0644); err != nil {
		return fmt.Errorf("writing crush config: %w", err)
	}

	fmt.Printf("crush: injected codemap MCP entry into %s\n", cfgPath)
	return nil
}

func injectOpencode() error {
	navDir := filepath.Join(os.Getenv("HOME"), ".config", "opencode", "context", "codemap")
	if err := os.MkdirAll(navDir, 0755); err != nil {
		return fmt.Errorf("creating opencode context directory: %w", err)
	}

	navPath := filepath.Join(navDir, "navigation.md")
	if err := os.WriteFile(navPath, []byte(opencodeNavContent), 0644); err != nil {
		return fmt.Errorf("writing navigation.md: %w", err)
	}

	fmt.Printf("opencode: updated %s\n", navPath)
	return nil
}

func injectAgents() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	agentsPath := filepath.Join(cwd, "AGENTS.md")
	if _, err := os.Stat(agentsPath); err == nil {
		fmt.Println("agents: AGENTS.md already exists, skipping")
		return nil
	}

	if err := os.WriteFile(agentsPath, []byte(agentsContent), 0644); err != nil {
		return fmt.Errorf("writing AGENTS.md: %w", err)
	}

	fmt.Printf("agents: created %s\n", agentsPath)
	return nil
}