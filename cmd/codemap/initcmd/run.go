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

const opencodeNavContent = `<!-- Context: codemap/navigation | Priority: high | Version: 1.3 -->

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
 * Version: 1.3
 *
 * Soft-mode guard that warns agents when they use grep/glob/read on .go files
 * instead of codemap MCP tools. The warning includes a suggestion to use the
 * appropriate codemap tool. Calls are NOT blocked — the agent can proceed if
 * it has a good reason (e.g. reading non-Go files, checking config, etc).
 *
 * Installed by ` + "`codemap inject`" + ` (alias: ` + "`codemap init`" + `).
 */

import type { Plugin } from "@opencode-ai/plugin"

const GO_FILE_RE = /\.go\b/

const TOOL_SUGGESTIONS: Record<string, string> = {
  grep: "Use ` + "`codemap_search`" + ` (symbol names), ` + "`codemap_search_text`" + ` (file contents), or ` + "`codemap_callers_of`" + `/` + "`codemap_callees_of`" + ` (relationships) instead.",
  glob: "Use ` + "`codemap_package`" + ` (package API; without path it lists all packages) to discover Go packages and their symbols.",
  read: "For one symbol: ` + "`codemap_show`" + ` (source: true for raw source). For a file's symbols: ` + "`codemap_symbols_in_file`" + `. For symbol context: ` + "`codemap_get_context_bundle`" + `. For a changed tree: ` + "`codemap_changed_symbols`" + `.",
}

function isGoTarget(args: Record<string, any>): boolean {
  for (const key of ["pattern", "path", "file", "include", "filePath"]) {
    const val = args[key]
    if (typeof val === "string" && GO_FILE_RE.test(val)) return true
  }
  return false
}

// opencode exposes MCP tools namespaced (codemap_search); built-ins are bare.
function baseToolName(tool: string): string {
  return tool.toLowerCase().replace(/^codemap[._-]/, "")
}

function warnKey(tool: string, args: Record<string, any>): string {
  const target = args.filePath || args.file || args.path || args.pattern || tool
  return ` + "`${tool}:${target}`" + `
}

export const CodemapGuard: Plugin = async () => {
  const warned = new Set<string>()

  return {
    "tool.execute.after": async (input, output) => {
      const tool = input.tool.toLowerCase()
      const name = baseToolName(tool)

      const CODEMAP_SEARCH = ["search", "search_text", "methods_of"]
      if (CODEMAP_SEARCH.includes(name)) {
        const rendered = output.output ?? ""
        if (!rendered.trim()) {
          output.output = ` + "`[codemap-guard] \"${input.tool}\" returned no results. The index may be stale or incomplete — run the 'index' tool to rebuild it, then retry.\\n\\n`" + ` + rendered
        }
        return
      }

      const suggestion = TOOL_SUGGESTIONS[name]
      if (!suggestion) return

      const args = (input.args ?? {}) as Record<string, any>
      if (!isGoTarget(args)) return

      // ` + "`read`" + ` is correct when an Edit follows; warn once per file.
      const key = warnKey(name, args)
      if (warned.has(key)) return
      warned.add(key)

      const warning = ` + "`[codemap-guard] \"${input.tool}\" on a Go file (${args.pattern || args.filePath || args.file || \"\"}). ${suggestion}\\n\\n`" + `
      output.output = warning + (output.output ?? "")
    },
  }
}
`

const codemapGuardMod = `/**
 * Codemap Guard — Command Code mod
 * Version: 1.0
 *
 * Soft-mode guard that warns agents when they use grep/glob/read on .go files
 * instead of codemap MCP tools. Calls are NOT blocked.
 *
 * Installed by ` + "`codemap inject`" + ` (alias: ` + "`codemap init`" + `).
 */

import type { ModApi } from "@commandcode/harness"

const GO_FILE_RE = /\.go\b/

const TOOL_SUGGESTIONS: Record<string, string> = {
  grep: "Use ` + "`codemap_search`" + ` (symbol names), ` + "`codemap_search_text`" + ` (file contents), or ` + "`codemap_callers_of`" + `/` + "`codemap_callees_of`" + ` (relationships) instead.",
  glob: "Use ` + "`codemap_package`" + ` (package API; without path it lists all packages) to discover Go packages and their symbols.",
  read_file: "For one symbol: ` + "`codemap_show`" + ` (source: true for raw source). For a file's symbols: ` + "`codemap_symbols_in_file`" + `. For symbol context: ` + "`codemap_get_context_bundle`" + `. For a changed tree: ` + "`codemap_changed_symbols`" + `.",
}

function isGoTarget(input: Record<string, any>): boolean {
  for (const key of ["pattern", "glob", "path", "file_path", "include"]) {
    const val = input[key]
    if (typeof val === "string" && GO_FILE_RE.test(val)) return true
  }
  const paths = input.paths
  return Array.isArray(paths) && paths.some((p) => typeof p === "string" && GO_FILE_RE.test(p))
}

// MCP tools may surface namespaced; built-ins are bare.
function baseToolName(tool: string): string {
  return tool.toLowerCase().replace(/^mcp__codemap__/, "").replace(/^codemap[._-]/, "")
}

function warnKey(tool: string, input: Record<string, any>): string {
  const target = input.file_path || input.path || input.pattern || input.glob || tool
  return ` + "`${tool}:${target}`" + `
}

export default function (cmd: ModApi): void {
  const warned = new Set<string>()

  cmd.hooks({
    afterToolCall({ toolName, input, isError }) {
      if (isError) return undefined

      const name = baseToolName(toolName)
      const suggestion = TOOL_SUGGESTIONS[name]
      if (!suggestion) return undefined

      const args = (input ?? {}) as Record<string, any>
      if (!isGoTarget(args)) return undefined

      const key = warnKey(name, args)
      if (warned.has(key)) return undefined
      warned.add(key)

      return {
        additionalContext: ` + "`[codemap-guard] \"${toolName}\" on a Go file (${args.pattern || args.file_path || args.path || \"\"}). ${suggestion}`" + `,
      }
    },
  })
}
`

const codemapGuardLua = `-- Codemap Guard — maki plugin
-- Installed by ` + "`codemap inject`" + ` (alias: ` + "`codemap init`" + `).
-- Soft-mode guard that warns when grep/glob target .go files instead of
-- codemap MCP tools. Calls are NOT blocked.

local SUGGESTIONS = {
  grep = "Use codemap__search (symbol names), codemap__search_text (file contents), or codemap__callers_of/codemap__callees_of (relationships) instead.",
  glob = "Use codemap__package (package API; without path it lists all packages) to discover Go packages and their symbols.",
}

local warned = {}
local pending = {} -- tool -> input stashed at the input stage

local function is_go_target(input)
  for _, key in ipairs({ "pattern", "path" }) do
    local v = input[key]
    if type(v) == "string" and v:find("%.go") then
      return true
    end
  end
  return false
end

for _, name in ipairs({ "grep", "glob" }) do
  maki.api.set_slot("tool." .. name .. ".input", function(prev, input, ctx)
    pending[name] = is_go_target(input) and input or nil
    return prev(input, ctx)
  end)

  maki.api.set_slot("tool." .. name .. ".output", function(prev, out, ctx)
    local input = pending[name]
    pending[name] = nil
    if not input or out.is_error then
      return prev(out, ctx)
    end
    local target = input.pattern or input.path or ""
    local key = name .. ":" .. target
    if warned[key] then
      return prev(out, ctx)
    end
    warned[key] = true
    out.text = '[codemap-guard] "' .. name .. '" on a Go file (' .. target .. "). "
      .. SUGGESTIONS[name] .. "\n\n" .. (out.text or "")
    return prev(out, ctx)
  end)
end

-- Empty codemap results hint: the index may be stale. Slot names are fine to
-- set before the MCP server registers.
for _, name in ipairs({ "codemap__search", "codemap__search_text", "codemap__methods_of" }) do
  maki.api.set_slot("tool." .. name .. ".output", function(prev, out, ctx)
    if not out.is_error and (out.text or ""):match("^%s*$") then
      out.text = '[codemap-guard] "' .. name .. '" returned no results. The index may be stale — run the codemap__index tool, then retry.\n\n' .. (out.text or "")
    end
    return prev(out, ctx)
  end)
end
`

const makiPluginToml = `# Grants required by codemap-guard: it wraps grep/glob tool slots, and tools
# that declare no permission capability require the plugin to hold every
# permission grant.
[permissions]
fs_read = true
fs_write = true
net = true
run = true
env = true
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

	if err := injectCommandCode(); err != nil {
		fmt.Fprintf(os.Stderr, "commandcode: %v\n", err)
		errors = true
	}

	if err := injectMaki(); err != nil {
		fmt.Fprintf(os.Stderr, "maki: %v\n", err)
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
	changed, err := writeIfChanged(navPath, opencodeNavContent)
	if err != nil {
		return fmt.Errorf("writing navigation.md: %w", err)
	}
	fmt.Printf("opencode: %s %s\n", changeVerb(changed), navPath)

	// Plugin (codemap-guard.ts)
	pluginDir := filepath.Join(os.Getenv("HOME"), ".config", "opencode", "plugins")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		return fmt.Errorf("creating opencode plugins directory: %w", err)
	}

	pluginPath := filepath.Join(pluginDir, "codemap-guard.ts")
	changed, err = writeIfChanged(pluginPath, codemapGuardPlugin)
	if err != nil {
		return fmt.Errorf("writing codemap-guard.ts: %w", err)
	}
	fmt.Printf("opencode: %s %s\n", changeVerb(changed), pluginPath)

	return nil
}

// writeIfChanged writes content to path and reports whether the bytes differed
// from what was already there, so callers can tell a refreshed install from a
// no-op run of a stale binary.
func writeIfChanged(path, content string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == content {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return false, err
	}
	return true, nil
}

func changeVerb(changed bool) string {
	if changed {
		return "updated"
	}
	return "unchanged"
}

// injectCommandCode installs the guard mod only where Command Code is present,
// so machines without it are never given a stray ~/.commandcode tree.
func injectCommandCode() error {
	baseDir := filepath.Join(os.Getenv("HOME"), ".commandcode")
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		fmt.Println("commandcode: ~/.commandcode not found, skipping")
		return nil
	}

	modDir := filepath.Join(baseDir, "mods")
	if err := os.MkdirAll(modDir, 0755); err != nil {
		return fmt.Errorf("creating commandcode mods directory: %w", err)
	}

	modPath := filepath.Join(modDir, "codemap-guard.ts")
	changed, err := writeIfChanged(modPath, codemapGuardMod)
	if err != nil {
		return fmt.Errorf("writing codemap-guard.ts: %w", err)
	}
	fmt.Printf("commandcode: %s %s\n", changeVerb(changed), modPath)

	return nil
}

// injectMaki installs the MCP entry and guard plugin only where maki is
// present, so machines without it are never given a stray config directory.
// ~/.maki wins over ~/.config/maki, matching maki's own precedence rule.
func injectMaki() error {
	home := os.Getenv("HOME")

	dir := filepath.Join(home, ".maki")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		dir = filepath.Join(home, ".config", "maki")
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			fmt.Println("maki: config dir not found, skipping")
			return nil
		}
	}

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding codemap binary: %w", err)
	}

	if err := injectMakiMCP(dir, selfPath); err != nil {
		return err
	}
	return injectMakiGuard(dir)
}

func injectMakiMCP(dir, selfPath string) error {
	cfgPath := filepath.Join(dir, "mcp.toml")

	block := "[mcp.codemap]\ncommand = [\"" + selfPath + "\", \"serve\"]\nalways_load = true\n"

	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath, []byte(block), 0644); err != nil {
			return fmt.Errorf("writing mcp.toml: %w", err)
		}
		fmt.Printf("maki: created %s\n", cfgPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading mcp.toml: %w", err)
	}

	content := string(data)
	if strings.Contains(content, "[mcp.codemap]") {
		fmt.Println("maki: codemap MCP entry already exists, skipping")
		return nil
	}

	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(cfgPath, []byte(content+"\n"+block), 0644); err != nil {
		return fmt.Errorf("writing mcp.toml: %w", err)
	}

	fmt.Printf("maki: updated %s\n", cfgPath)
	return nil
}

func injectMakiGuard(dir string) error {
	luaDir := filepath.Join(dir, "lua")
	if err := os.MkdirAll(luaDir, 0755); err != nil {
		return fmt.Errorf("creating maki lua directory: %w", err)
	}

	luaPath := filepath.Join(luaDir, "codemap-guard.lua")
	changed, err := writeIfChanged(luaPath, codemapGuardLua)
	if err != nil {
		return fmt.Errorf("writing codemap-guard.lua: %w", err)
	}
	fmt.Printf("maki: %s %s\n", changeVerb(changed), luaPath)

	initPath := filepath.Join(dir, "init.lua")
	data, err := os.ReadFile(initPath)
	switch {
	case os.IsNotExist(err):
		if err := os.WriteFile(initPath, []byte("require(\"codemap-guard\")\n"), 0644); err != nil {
			return fmt.Errorf("writing init.lua: %w", err)
		}
		fmt.Printf("maki: created %s\n", initPath)
	case err != nil:
		return fmt.Errorf("reading init.lua: %w", err)
	case !strings.Contains(string(data), "codemap-guard"):
		content := string(data)
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(initPath, []byte(content+"require(\"codemap-guard\")\n"), 0644); err != nil {
			return fmt.Errorf("writing init.lua: %w", err)
		}
		fmt.Printf("maki: updated %s\n", initPath)
	default:
		fmt.Println("maki: init.lua already loads codemap-guard, skipping")
	}

	permsPath := filepath.Join(dir, "plugin.toml")
	if _, err := os.Stat(permsPath); err == nil {
		fmt.Println("maki: plugin.toml already exists, skipping")
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking plugin.toml: %w", err)
	}
	if err := os.WriteFile(permsPath, []byte(makiPluginToml), 0644); err != nil {
		return fmt.Errorf("writing plugin.toml: %w", err)
	}
	fmt.Printf("maki: created %s\n", permsPath)

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
