package initcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ToolsAndTipsBlock is the single source of truth for the codemap tool list and
// usage tips shared by AGENTS.md and the opencode navigation doc. Both documents
// embed this one block so a tool/tip edit updates both at once. Kept exported so
// it is the canonical reference text.
const ToolsAndTipsBlock = `## Available Tools

| Tool | Returns / Does | When to use |
|---|---|---|
| ` + "`index`" + ` | Force a full re-index | Indexing is automatic otherwise |
| ` + "`overview`" + ` | Each package's exports + signatures | Understanding project structure |
| ` + "`search`" + ` | Qualified names, file:line, signatures, docs; mode: substring/prefix/method | Finding symbols by name |
| ` + "`show`" + ` | Signature, docs, file:line, incoming+outgoing edges; source: true returns raw source | Understanding a single symbol |
| ` + "`callers_of`" + ` | Caller names, file:line, edge types | Tracing who calls a function |
| ` + "`callees_of`" + ` | Callee names, file:line, edge types | Tracing a symbol's dependencies |
| ` + "`package`" + ` | All symbols with signatures and docs; without path lists all packages | Inspecting a package's API or discovering packages |
| ` + "`methods_of`" + ` | Method names, signatures, file:line | Finding a type's method set |
| ` + "`imports_of`" + ` | Imports (direction out) or importers (direction in); transitive: true walks the tree | Import relationships |
| ` + "`all_edges`" + ` | Every edge in the codebase; optional edge_type filter | Bulk relationship analysis |
| ` + "`search_text`" + ` | Matching file paths, line numbers, context | Full-text search across indexed files |
| ` + "`get_context_bundle`" + ` | Symbol body, callees, callers, same-file symbols | LLM context preparation |
| ` + "`get_hotspots`" + ` | Symbols ranked by complexity x churn (mode churn) or PageRank centrality (mode pagerank) | Finding risky or critical code |
| ` + "`changed_symbols`" + ` | Symbols changed in the working tree; workspace: true covers every member repo | One line per symbol with ` + "`compact: true`" + ` |
| ` + "`read_response_section`" + ` | Stored sections of an oversized response, by index or keyword | Reading back an oversized-response manifest |
| ` + "`stats`" + ` | Per-tool calls, errors, response bytes, estimated raw-read bytes, reduction % | Verifying context savings and tuning output formats |

## Tips

- Qualified names use ` + "`package/path.SymbolName`" + ` format
- ` + "`include_tests: true`" + ` includes test packages/symbols
- ` + "`full_docs: true`" + ` shows complete documentation instead of the first sentence
- ` + "`include_unexported: true`" + ` includes private symbols (package tool)
- ` + "`methods_of`" + ` takes a short type name (e.g., ` + "`Store`" + `, not the full path)
- ` + "`search`" + ` is case-insensitive substring match by default; ` + "`mode: \"prefix\"`" + ` matches qualified-name starts, ` + "`mode: \"method\"`" + ` finds methods by name across types
- ` + "`search`" + ` returns exported and unexported symbols by default; ` + "`exported: true`" + ` filters to exported-only, ` + "`exported: false`" + ` to unexported-only
- ` + "`imports_of`" + ` defaults to direction ` + "`out`" + ` (imports); use ` + "`direction: \"in\"`" + ` for importers and ` + "`transitive: true`" + ` for the full dependency tree
- Indexing is automatic and fast (~50ms). Re-indexes when Go files change.
`

const agentsContent = `# AGENTS.md

This project uses ` + "`codemap`" + ` MCP tools for Go codebase analysis. These tools are faster and more precise than grep/glob for understanding Go code structure, call relationships, and symbol locations.

## When to Use Codemap vs grep/glob/read

| You want... | Use this | Not this |
|---|---|---|
| Find a function/type by name | ` + "`search`" + ` | grep |
| Understand a symbol's signature, docs, callers, callees | ` + "`show`" + ` | reading source files |
| Find who calls a function | ` + "`callers_of`" + ` | grep for function name |
| Find what a function depends on | ` + "`callees_of`" + ` | reading imports |
| Get a package's full API | ` + "`package`" + ` | listing .go files |
| Find all methods on a type | ` + "`methods_of`" + ` | grep for receiver patterns |
| See project architecture | ` + "`overview`" + ` | guessing from directory structure |
| Find where a package is imported | ` + "`imports_of`" + ` with ` + "`direction: \"in\"`" + ` | grep for import path |
| Find what a package depends on | ` + "`imports_of`" + ` | reading import blocks |
| List all packages | ` + "`package`" + ` without path | ls and guess |
| Search file contents | ` + "`search_text`" + ` | grep |
| Get symbol context bundle | ` + "`get_context_bundle`" + ` | manual assembly |
| Find code hotspots | ` + "`get_hotspots`" + ` | manual review |
| Rank symbol importance | ` + "`get_hotspots`" + ` with ` + "`mode: \"pagerank\"`" + ` | guessing |

` + ToolsAndTipsBlock + `
`

const opencodeNavContent = `<!-- Context: codemap/navigation | Priority: high | Version: 1.2 -->

# Codemap MCP Tools

Codemap provides Go code analysis via MCP tools. **Use codemap tools instead of grep/glob/read for any Go codebase navigation** — they return file paths, line numbers, signatures, and call relationships that grep cannot.

## How It Works

Indexing is **automatic** — the first tool call triggers indexing if needed (~50ms). If Go files change, the index refreshes automatically. No manual ` + "`index`" + ` call required.

## Decision Guide

| You want... | Use | Instead of |
|---|---|---|
| Find a function/type by name | ` + "`search`" + ` | grep |
| See a symbol's signature and docs | ` + "`show`" + ` | reading source files |
| Who calls this function? | ` + "`callers_of`" + ` | grep for function name |
| What does this function call? | ` + "`callees_of`" + ` | reading code |
| Get a package's full API | ` + "`package`" + ` | listing .go files |
| All methods on a type | ` + "`methods_of`" + ` | grep for receiver |
| Project architecture overview | ` + "`overview`" + ` | guessing from dirs |
| Where is this package imported? | ` + "`imports_of`" + ` with ` + "`direction: \"in\"`" + ` | grep for import path |
| What does this package depend on? | ` + "`imports_of`" + ` | reading import blocks |
| Search file contents | ` + "`search_text`" + ` | grep |
| Get symbol context bundle | ` + "`get_context_bundle`" + ` | manual assembly |
| Find code hotspots | ` + "`get_hotspots`" + ` | manual review |
| Rank symbol importance | ` + "`get_hotspots`" + ` with ` + "`mode: \"pagerank\"`" + ` | guessing |

` + ToolsAndTipsBlock + `
`

const codemapGuardPlugin = `/**
 * Codemap Guard — OpenCode plugin
 *
 * Soft-mode guard that warns agents when they use grep/glob/read on .go files
 * instead of codemap MCP tools. The warning includes a suggestion to use the
 * appropriate codemap tool. Calls are NOT blocked — the agent can proceed if
 * it has a good reason (e.g. reading non-Go files, checking config, etc).
 *
 * Installed by ` + "`codemap inject`" + `.
 */

import type { Plugin } from "@opencode-ai/plugin"

const GO_FILE_RE = /\.go\b/

const TOOL_SUGGESTIONS: Record<string, string> = {
  grep: "Use ` + "`codemap_search`" + ` (symbol names), ` + "`codemap_search_text`" + ` (file contents), or ` + "`codemap_callers_of`" + `/` + "`codemap_callees_of`" + ` (relationships) instead.",
  glob: "Use ` + "`codemap_package`" + ` (package API; without path it lists all packages) to discover Go packages and their symbols.",
  read: "Use ` + "`codemap_show`" + ` (single symbol; source: true for raw source), ` + "`codemap_package`" + ` (full package API), or ` + "`codemap_get_context_bundle`" + ` (symbol + context) instead.",
}

function isGoTarget(args: Record<string, any>): boolean {
  for (const key of ["pattern", "path", "file", "include", "filePath"]) {
    const val = args[key]
    if (typeof val === "string" && GO_FILE_RE.test(val)) return true
  }
  return false
}

export const CodemapGuard: Plugin = async () => {
  return {
    "tool.execute.after": async (input, output) => {
      const tool = input.tool.toLowerCase()

      const CODEMAP_SEARCH = ["search", "search_text", "methods_of"]
      if (CODEMAP_SEARCH.includes(tool)) {
        const rendered = output.output ?? ""
        if (!rendered.trim()) {
          output.output = ` + "`[codemap-guard] \"${input.tool}\" returned no results. The index may be stale or incomplete — run the 'index' tool to rebuild it, then retry.\\n\\n`" + ` + rendered
        }
        return
      }

      const suggestion = TOOL_SUGGESTIONS[tool]
      if (!suggestion) return

      const args = (input.args ?? {}) as Record<string, any>
      if (!isGoTarget(args)) return

      const warning = ` + "`[codemap-guard] \"${input.tool}\" tool output references Go files (${input.args?.pattern || input.args?.filePath || input.args?.file || \"\"}). ${suggestion}\\n\\n`" + `
      output.output = warning + (output.output ?? "")
    },
  }
}
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
	// Context file (navigation.md)
	navDir := filepath.Join(os.Getenv("HOME"), ".config", "opencode", "context", "codemap")
	if err := os.MkdirAll(navDir, 0755); err != nil {
		return fmt.Errorf("creating opencode context directory: %w", err)
	}

	navPath := filepath.Join(navDir, "navigation.md")
	if err := os.WriteFile(navPath, []byte(opencodeNavContent), 0644); err != nil {
		return fmt.Errorf("writing navigation.md: %w", err)
	}
	fmt.Printf("opencode: updated %s\n", navPath)

	// Plugin (codemap-guard.ts)
	pluginDir := filepath.Join(os.Getenv("HOME"), ".config", "opencode", "plugins")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		return fmt.Errorf("creating opencode plugins directory: %w", err)
	}

	pluginPath := filepath.Join(pluginDir, "codemap-guard.ts")
	if err := os.WriteFile(pluginPath, []byte(codemapGuardPlugin), 0644); err != nil {
		return fmt.Errorf("writing codemap-guard.ts: %w", err)
	}
	fmt.Printf("opencode: updated %s\n", pluginPath)

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

func UpdateAgents() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	agentsPath := filepath.Join(cwd, "AGENTS.md")
	data, err := os.ReadFile(agentsPath)
	if err != nil {
		return fmt.Errorf("reading AGENTS.md: %w", err)
	}

	content := string(data)
	tip := "`search` only returns exported symbols"
	if strings.Contains(content, tip) {
		fmt.Println("agents: codemap tip already present, skipping")
		return nil
	}

	// Find the Tips section and append
	lines := strings.Split(content, "\n")
	var out []string
	tipsFound := false
	for _, line := range lines {
		out = append(out, line)
		if strings.HasPrefix(strings.TrimSpace(line), "## Tips") {
			tipsFound = true
		}
	}

	if !tipsFound {
		// No Tips section, append at end
		out = append(out, "", "## Tips", "", "- Qualified names use `package/path.SymbolName` format", fmt.Sprintf("- %s — unexported (lowercase) functions/types are not indexed. Use `package` with `include_unexported: true` to find them.", tip))
	} else {
		// Find the end of tips section (next ## or EOF) and insert before it
		var final []string
		inTips := false
		inserted := false
		for _, line := range out {
			if strings.HasPrefix(strings.TrimSpace(line), "## Tips") {
				inTips = true
			} else if inTips && strings.HasPrefix(strings.TrimSpace(line), "## ") {
				// Next section starts, insert before
				if !inserted {
					final = append(final, "", fmt.Sprintf("- %s — unexported (lowercase) functions/types are not indexed. Use `package` with `include_unexported: true` to find them.", tip))
					inserted = true
				}
				inTips = false
			}
			final = append(final, line)
		}
		if !inserted && inTips {
			final = append(final, "", fmt.Sprintf("- %s — unexported (lowercase) functions/types are not indexed. Use `package` with `include_unexported: true` to find them.", tip))
		}
		out = final
	}

	if err := os.WriteFile(agentsPath, []byte(strings.Join(out, "\n")), 0644); err != nil {
		return fmt.Errorf("writing AGENTS.md: %w", err)
	}

	fmt.Printf("agents: updated %s\n", agentsPath)
	return nil
}
