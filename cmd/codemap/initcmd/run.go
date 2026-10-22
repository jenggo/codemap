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
| ` + "`reindex`" + ` | Force a full re-index | Indexing is automatic otherwise |
| ` + "`summary`" + ` | Module, package map with docs, entry points, deps, hubs (~1.5 KB) | First call in an unfamiliar repo |
| ` + "`overview`" + ` | Each package's exports + signatures (full inventory) | Enumerating the API surface |
| ` + "`search`" + ` | Qualified names, file:line, signatures, docs; mode: substring/prefix/method | Finding symbols by name |
| ` + "`show`" + ` | Signature, docs, file:line, incoming+outgoing edges; source: true returns raw source | Understanding a single symbol |
| ` + "`callers_of`" + ` | Caller names, file:line, edge types | Tracing who calls a function |
| ` + "`callees_of`" + ` | Callee names, file:line, edge types | Tracing a symbol's dependencies |
| ` + "`package`" + ` | All symbols with signatures and docs; without path lists all packages | Inspecting a package's API or discovering packages |
| ` + "`methods_of`" + ` | Method names, signatures, file:line | Finding a type's method set |
| ` + "`imports_of`" + ` | Imports (direction out) or importers (direction in); transitive: true walks the tree | Import relationships |
| ` + "`dependency_flow`" + ` | One package's imports/importers; layers: true gives project-wide layering | Package dependency structure |
| ` + "`all_edges`" + ` | Every edge in the codebase; optional edge_type filter | Bulk relationship analysis |
| ` + "`type_usage`" + ` | All symbols whose signatures use a given type | Finding where a type matters |
| ` + "`interface_impls`" + ` | All types implementing an interface and their methods | Finding implementations |
| ` + "`blast_radius`" + ` | Callers, interface implementations, embedders, type users (transitive) | Impact check before refactoring |
| ` + "`find_path`" + ` | BFS edge path between two symbols | How two symbols connect |
| ` + "`cycles`" + ` | Cycles in one edge-type graph (imports, calls) | Detecting circular dependencies |
| ` + "`symbols_in_file`" + ` | All symbols defined in one file | File-level inventory |
| ` + "`entry_points`" + ` | main(), test entries, uncalled exported funcs, HTTP handlers | Finding where execution starts |
| ` + "`unused`" + ` | Unexported symbols with zero callers | Finding dead code |
| ` + "`search_text`" + ` | Matching file paths, line numbers, context; regex mode and context lines available | Full-text search across indexed files |
| ` + "`pattern`" + ` | Matching file paths, line numbers, matched source, enclosing symbol | Structural AST search when name/text search is not enough |
| ` + "`get_context_bundle`" + ` | Symbol body, callees, callers, same-file symbols | LLM context preparation |
| ` + "`get_hotspots`" + ` | Symbols ranked by complexity x churn (mode churn) or PageRank centrality (mode pagerank) | Finding risky or critical code |
| ` + "`changed_symbols`" + ` | Symbols changed in the working tree; renames reported as from -> to; workspace: true covers every member repo | One line per symbol with ` + "`compact: true`" + ` |
| ` + "`read_response_section`" + ` | Stored sections of an oversized response, by index or keyword | Reading back an oversized-response manifest |
| ` + "`stats`" + ` | Per-tool calls, errors, response bytes, estimated raw-read bytes, reduction % | Verifying context savings and tuning output formats |
| ` + "`contracts`" + ` | Cross-repo producer/consumer contract edges (shared constants + shape match) | Tracking wire contracts in multi-repo workspaces |
| ` + "`contract_drift`" + ` | Structural drift between shape-matched types across repos, with field-level evidence | Detecting breaking protocol changes |
| ` + "`runtime_contracts`" + ` | Redis key patterns, JetStream subjects, WS type strings with producer/consumer sites | Tracking infra contracts |
| ` + "`suppress_contract`" + ` | Suppress a confirmed false-positive contract pair | Cleaning false positives |
| ` + "`health`" + ` | Per-repo index freshness (indexed/stale/missing) | Diagnosing stale results |
| ` + "`schema`" + ` | Schema of all result types | Understanding response shapes |
| ` + "`replace_symbol_body`" + ` | Replaces a symbol's declaration span (doc comment too with ` + "`include_doc`" + `) | Editing a declaration without counting lines; needs ` + "`serve --allow-write`" + ` |
| ` + "`insert_before_symbol`" + ` | Inserts Go source before a symbol's declaration (before its doc comment) | Adding a declaration next to its siblings |
| ` + "`insert_after_symbol`" + ` | Inserts Go source after a symbol's declaration | Adding a declaration next to its siblings |
| ` + "`safe_delete_symbol`" + ` | Deletes a symbol only when the index records no incoming references | Removing dead code with the referrers checked for you |

## Tips

- Qualified names use ` + "`package/path.SymbolName`" + ` format
- ` + "`include_tests: true`" + ` includes test packages/symbols
- ` + "`full_docs: true`" + ` shows complete documentation instead of the first sentence
- ` + "`include_unexported: true`" + ` includes private symbols (package tool)
- ` + "`methods_of`" + ` takes a short type name (e.g., ` + "`Store`" + `, not the full path)
- ` + "`search`" + ` is case-insensitive substring match by default; ` + "`mode: \"prefix\"`" + ` matches qualified-name starts, ` + "`mode: \"method\"`" + ` finds methods by name across types
- ` + "`search`" + ` matches every indexed field, not only the name (qualified_name, name, kind, receiver, signature, doc). A const's signature is its full literal value, so a short pattern can match a large const body and return a wide result; narrow with ` + "`kind`" + ` or ` + "`exported`" + `
- ` + "`search`" + ` returns exported and unexported symbols by default; ` + "`exported: true`" + ` filters to exported-only, ` + "`exported: false`" + ` to unexported-only
- ` + "`imports_of`" + ` defaults to direction ` + "`out`" + ` (imports); use ` + "`direction: \"in\"`" + ` for importers and ` + "`transitive: true`" + ` for the full dependency tree
- ` + "`pattern`" + ` takes Go source with ` + "`$UPPERCASE`" + ` metavariables (ast-grep style); matching is syntactic and repeated metavariables must bind identical nodes; ` + "`file_pattern`" + ` and ` + "`repo`" + ` narrow the scan
- ` + "`search`" + ` down-ranks generated and test symbols (never hides them); ` + "`generated: \"exclude\"|\"only\"`" + ` filters them
- The four write tools refuse unless the server was started with ` + "`serve --allow-write`" + `; a read-only server never touches a source file
- Indexing is automatic and fast (~50ms). Re-indexes when Go files change.
- In a Go module the guard injects a ` + "`codemap summary`" + ` of the project into the system prompt, so the package map is already available before the first tool call.
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
| Get oriented in a new repo | ` + "`summary`" + ` | reading the directory tree |
| Find where a package is imported | ` + "`imports_of`" + ` with ` + "`direction: \"in\"`" + ` | grep for import path |
| Find what a package depends on | ` + "`imports_of`" + ` | reading import blocks |
| List all packages | ` + "`package`" + ` without path | ls and guess |
| Search file contents | ` + "`search_text`" + ` | grep |
| Find code by structural shape (e.g. every ` + "`defer X.Close()`" + `) | ` + "`pattern`" + ` | grep with context flags |
| Find all types implementing an interface | ` + "`interface_impls`" + ` | grep for method sets |
| Measure impact before refactoring | ` + "`blast_radius`" + ` | manual search |
| Get symbol context bundle | ` + "`get_context_bundle`" + ` | manual assembly |
| Find code hotspots | ` + "`get_hotspots`" + ` | manual review |
| Rank symbol importance | ` + "`get_hotspots`" + ` with ` + "`mode: \"pagerank\"`" + ` | guessing |

` + ToolsAndTipsBlock + `
`

const opencodeNavContent = `<!-- Context: codemap/navigation | Priority: high | Version: 1.4 -->

# Codemap MCP Tools

Codemap provides Go code analysis via MCP tools. **Use codemap tools instead of grep/glob/read for any Go codebase navigation** — they return file paths, line numbers, signatures, and call relationships that grep cannot.

## How It Works

Indexing is **automatic** — the first tool call triggers indexing if needed (~50ms). If Go files change, the index refreshes automatically. No manual ` + "`reindex`" + ` call required.

## Decision Guide

| You want... | Use | Instead of |
|---|---|---|
| Find a function/type by name | ` + "`search`" + ` | grep |
| See a symbol's signature and docs | ` + "`show`" + ` | reading source files |
| Who calls this function? | ` + "`callers_of`" + ` | grep for function name |
| What does this function call? | ` + "`callees_of`" + ` | reading code |
| Get a package's full API | ` + "`package`" + ` | listing .go files |
| All methods on a type | ` + "`methods_of`" + ` | grep for receiver |
| Get oriented in a new repo | ` + "`summary`" + ` | guessing from dirs |
| Where is this package imported? | ` + "`imports_of`" + ` with ` + "`direction: \"in\"`" + ` | grep for import path |
| What does this package depend on? | ` + "`imports_of`" + ` | reading import blocks |
| Search file contents | ` + "`search_text`" + ` | grep |
| Find code by structural shape (e.g. every ` + "`defer X.Close()`" + `) | ` + "`pattern`" + ` | grep with context flags |
| Find all types implementing an interface | ` + "`interface_impls`" + ` | grep for method sets |
| Measure impact before refactoring | ` + "`blast_radius`" + ` | manual search |
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
          output.output = ` + "`[codemap-guard] \"${input.tool}\" returned no results. The index may be stale or incomplete — run the 'reindex' tool to rebuild it, then retry.\\n\\n`" + ` + rendered
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
-- Version: 5.0
-- Installed by ` + "`" + `codemap inject` + "`" + ` (alias: ` + "`" + `codemap init` + "`" + `).
-- Soft-mode guard that rides along with the host read/grep/glob/index tools: on
-- a Go project it points at the codemap tool that answers what a raw file read
-- or a text search structurally cannot. Calls are NOT blocked and the agent can
-- ignore the nudge and keep the result it already has.
--
-- The escalation ladder is question-typed rather than tool-typed:
--   shape of one file         -> index (skeleton), then read
--   one symbol / a file's API -> codemap__symbols_in_file / codemap__show
--   who calls / what breaks   -> codemap__callers_of / codemap__blast_radius
--   symbol or package lookup  -> codemap__search / codemap__package
--   file contents / AST shape -> codemap__search_text / codemap__pattern
--
-- Only Go projects are nudged: a nudge is emitted when the tool's own target
-- (the read file, the grep path/include, the glob path, or the cwd) sits under
-- a directory holding go.mod or go.work. Elsewhere the guard stays silent.
--
-- Why append to the tool text instead of session.notify: a notify is delivered
-- asynchronously and does not reach the model inside the turn that produced the
-- result, so it cannot steer the next call. Appending rides the result the
-- model is about to read.

-- The markers that make a directory the root of a Go codebase.
local GO_MARKERS = { "go.mod", "go.work" }

local warned = {}

-- One codemap summary per project per session, injected into the system prompt
-- so the agent starts oriented instead of discovering the layout by reading
-- files. codemap summary on an unindexed repo prints an error and exits 0,
-- so the output is validated rather than the exit code.
local SUMMARY_TIMEOUT_MS = 15000
local summary_cache = {}
local summary_inflight = {}

-- The binary codemap inject registered in mcp.toml, or codemap on PATH.
local function codemap_bin()
  local cfg = maki.fs.read(maki.env.config_dir() .. "/mcp.toml")
  if cfg then
    local cmd = cfg:match('%[mcp%.codemap%][^%[]-"command"%s*=%s*%[%s*"([^"]+)"')
    if cmd and cmd ~= "" then
      return cmd
    end
  end
  return "codemap"
end

local function summary_key(root)
  return tostring(maki.session.current()) .. "|" .. root
end

-- Text format opens with "<module> | N packages, ..."; TOON opens with a
-- "packages[" block. Anything else (an error line) is not a summary.
local function valid_summary(out)
  if type(out) ~= "string" or out == "" then
    return false
  end
  return out:match("^packages%[") ~= nil
    or out:match("^%S+ %| %d+ packages") ~= nil
end

-- Runs codemap summary once per (session, project root) and caches the text.
-- A failure is cached too: retrying every turn would spawn a process per turn.
-- The MCP server owns the index, so a missing database is not created here.
local function summary_for(root)
  local key = summary_key(root)
  if summary_cache[key] ~= nil then
    return summary_cache[key]
  end
  if summary_inflight[key] then
    return nil
  end

  local db = root .. "/.codemap/codemap.db"
  local meta, meta_err = maki.fs.metadata(db)
  if not meta or meta_err then
    summary_cache[key] = false
    return nil
  end

  summary_inflight[key] = true

  local id, err = maki.fn.jobstart({ codemap_bin(), "summary", "--db", db }, {
    cwd = root,
    scope = { session = maki.session.current() },
  })
  if not id then
    maki.log.warn("codemap_guard: could not start codemap summary: " .. tostring(err))
    summary_inflight[key] = nil
    summary_cache[key] = false
    return nil
  end

  local result, wait_err = maki.fn.jobwait(id, SUMMARY_TIMEOUT_MS)
  summary_inflight[key] = nil
  if not result then
    maki.log.warn("codemap_guard: codemap summary did not finish: " .. tostring(wait_err))
    summary_cache[key] = false
    return nil
  end
  if not valid_summary(result.stdout) then
    summary_cache[key] = false
    return nil
  end
  summary_cache[key] = result.stdout
  return result.stdout
end

-- The Go module the session is working in, or nil for a non-Go project.
local function go_root()
  local cwd = maki.uv.cwd()
  if not cwd then
    return nil
  end
  return maki.fs.root(cwd, GO_MARKERS)
end

maki.api.register_prompt_hint({
  slot = "after_instructions",
  prompt = "system",
  content = function()
    local root = go_root()
    if not root then
      return ""
    end
    local summary = summary_for(root)
    if not summary then
      return "\n\n<!-- codemap:summary -->\n"
        .. "## Codebase summary (codemap)\n\n"
        .. "This is a Go module with no codemap index yet. Call codemap__summary "
        .. "before exploring: it indexes the repo and returns the package map, "
        .. "entry points, and dependencies in about a second.\n"
        .. "<!-- /codemap:summary -->"
    end
    return "\n\n<!-- codemap:summary -->\n"
      .. "## Codebase summary (codemap)\n\n"
      .. "This is a Go module. Start from this map instead of re-deriving it: "
      .. "the full symbol inventory is codemap__overview, and relationships "
      .. "come from codemap__callers_of, codemap__callees_of, and "
      .. "codemap__blast_radius.\n\n"
      .. summary
      .. "\n<!-- /codemap:summary -->"
  end,
})

-- Appended only after a successful, non-empty result on a Go target.
local NUDGE_INDEX = "codemap-guard: this skeleton lists declarations, not relationships. "
  .. "For who calls a symbol use codemap__callers_of, for what an edit affects "
  .. "codemap__blast_radius, for interface implementers codemap__interface_impls, "
  .. "and for symbols by name codemap__search."

local NUDGE_READ = "codemap-guard: reading a Go file line by line is slower than asking codemap for "
  .. "its structure. For one file's symbols use codemap__symbols_in_file, for one symbol's "
  .. "signature/docs/source use codemap__show, and for a symbol plus its callers and callees "
  .. "use codemap__get_context_bundle."

local NUDGE_GREP = "codemap-guard: grepping a Go module for a symbol or an import misses what the "
  .. "index already knows. For symbols by name use codemap__search, for file contents with "
  .. "line numbers use codemap__search_text, for import relationships use codemap__imports_of, "
  .. "and for structural shapes (e.g. every ` + "`" + `defer X.Close()` + "`" + `) use codemap__pattern."

local NUDGE_GLOB = "codemap-guard: globbing for Go files is slower than asking codemap. "
  .. "codemap__package with no path lists every package and its symbols, and with a path "
  .. "gives that package's full API."

local NUDGE = {
  index = NUDGE_INDEX,
  read = NUDGE_READ,
  grep = NUDGE_GREP,
  glob = NUDGE_GLOB,
}

-- True when v ends in a .go path. The extension must sit at a real boundary:
-- "a.gox" and "a.go.bak" are not Go files, while "dir.golang/m.go" is.
local function has_go_ext(v)
  local i = 1
  while true do
    local s, e = v:find("%.[Gg][Oo]", i)
    if not e then
      return false
    end
    local after = v:sub(e + 1, e + 1)
    local before = s > 1 and v:sub(s - 1, s - 1) or ""
    if before ~= "." and (after == "" or after == "/" or after:match("[%s\"',]")) then
      return true
    end
    i = e + 1
  end
end

local function first_string(...)
  for _, v in ipairs({ ... }) do
    if type(v) == "string" and v ~= "" then
      return v
    end
  end
  return nil
end

-- Whether the call is about Go, using the tool's own target:
--   read/index -- the file must be a .go file inside a Go module;
--   grep/glob  -- the search root (its path, else the cwd) must be a Go module.
-- maki.fs.root walks upward, so a file in a nested module is recognised even
-- when the cwd is above it, and a non-Go read in a Go repo stays silent.
local function on_go_target(tool, input)
  input = type(input) == "table" and input or {}
  local path = first_string(input.path, input.file_path, input.file)
  if path then
    path = maki.fs.abspath(path)
  end

  if tool == "read" or tool == "index" then
    if not (type(path) == "string" and has_go_ext(path)) then
      return false
    end
    return maki.fs.root(path, GO_MARKERS) ~= nil
  end

  local root = path or maki.uv.cwd()
  if not root then
    return false
  end
  return maki.fs.root(root, GO_MARKERS) ~= nil
end

-- session_id is a field on the slot ctx, not a method. Older hosts may omit it;
-- the fallback key keeps the once-per-session dedupe working.
local function session_of(ctx)
  if type(ctx) == "table" and ctx.session_id ~= nil then
    return tostring(ctx.session_id)
  end
  return "?"
end

-- True the first time (session, tool) is seen, so a session is nudged at most
-- once per tool rather than once per file: the point is to teach the ladder,
-- not to nag on every call.
local function first_time(session, tool)
  local key = session .. "|" .. tool
  if warned[key] then
    return false
  end
  warned[key] = true
  return true
end

-- An empty result means nothing to explain; an error means the agent has a
-- problem to fix first. Both stay untouched. "No files found" (grep/glob) is
-- also empty in spirit.
local function worth_nudging(out)
  if type(out) ~= "table" or out.is_error then
    return false
  end
  local text = out.text or ""
  return text ~= ""
    and not text:match("^%s*%(no entries")
    and text ~= "No files found"
    and text ~= "No matches found"
end

for tool, nudge in pairs(NUDGE) do
  maki.api.set_slot("tool." .. tool .. ".output", function(prev, out, ctx)
    if not worth_nudging(out) then
      return prev(out, ctx)
    end
    local input = type(ctx) == "table" and ctx.input or nil
    if not on_go_target(tool, input) then
      return prev(out, ctx)
    end
    if not first_time(session_of(ctx), tool) then
      return prev(out, ctx)
    end
    out.text = out.text .. "\n\n" .. nudge
    return prev(out, ctx)
  end)
end
`
const makiPluginToml = `# Grants required by codemap-guard: it wraps the index tool slot, and tools
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

const (
	makiRequire       = `require("codemap_guard")`
	legacyMakiRequire = `require("codemap-guard")`
)

func injectMakiGuard(dir string) error {
	luaDir := filepath.Join(dir, "lua")
	if err := os.MkdirAll(luaDir, 0755); err != nil {
		return fmt.Errorf("creating maki lua directory: %w", err)
	}

	// Older installs wrote codemap-guard.lua; drop it so init.lua's require
	// cannot resolve to a stale copy alongside the renamed file.
	legacyPath := filepath.Join(luaDir, "codemap-guard.lua")
	if _, statErr := os.Stat(legacyPath); statErr == nil {
		if err := os.Remove(legacyPath); err != nil {
			return fmt.Errorf("removing legacy codemap-guard.lua: %w", err)
		}
		fmt.Printf("maki: removed %s (renamed to codemap_guard.lua)\n", legacyPath)
	}

	luaPath := filepath.Join(luaDir, "codemap_guard.lua")
	changed, err := writeIfChanged(luaPath, codemapGuardLua)
	if err != nil {
		return fmt.Errorf("writing codemap_guard.lua: %w", err)
	}
	fmt.Printf("maki: %s %s\n", changeVerb(changed), luaPath)

	if err := injectMakiInitRequire(dir); err != nil {
		return err
	}
	return injectMakiPermissions(dir)
}

// injectMakiInitRequire makes init.lua load the guard module, rewriting the
// legacy hyphenated require that cannot resolve to codemap_guard.lua.
func injectMakiInitRequire(dir string) error {
	initPath := filepath.Join(dir, "init.lua")
	data, err := os.ReadFile(initPath)
	switch {
	case os.IsNotExist(err):
		if err := os.WriteFile(initPath, []byte(makiRequire+"\n"), 0644); err != nil {
			return fmt.Errorf("writing init.lua: %w", err)
		}
		fmt.Printf("maki: created %s\n", initPath)
	case err != nil:
		return fmt.Errorf("reading init.lua: %w", err)
	case strings.Contains(string(data), legacyMakiRequire):
		// Earlier versions required "codemap-guard", which cannot resolve
		// because the file on disk is codemap_guard.lua.
		content := strings.ReplaceAll(string(data), legacyMakiRequire, makiRequire)
		if err := os.WriteFile(initPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("rewriting init.lua: %w", err)
		}
		fmt.Printf("maki: updated %s\n", initPath)
	case !strings.Contains(string(data), makiRequire):
		content := string(data)
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(initPath, []byte(content+makiRequire+"\n"), 0644); err != nil {
			return fmt.Errorf("writing init.lua: %w", err)
		}
		fmt.Printf("maki: updated %s\n", initPath)
	default:
		fmt.Println("maki: init.lua already loads codemap_guard, skipping")
	}
	return nil
}

func injectMakiPermissions(dir string) error {
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
