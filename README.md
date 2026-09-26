# Codemap

**Codemap** is a Go code intelligence tool that parses, indexes, and queries Go source code. It builds a graph of symbols (functions, types, methods, interfaces, constants, variables) and their relationships (calls, references, implementations, embedding, imports), stored in a local SQLite database for fast offline querying.

It can be used both as a **CLI tool** and as an **MCP server** for AI-powered code navigation.

## Quick Start

```bash
# Index the current Go repository
codemap index

# Get a bird's-eye overview
codemap overview

# Search for a symbol
codemap search Server

# See who calls a function
codemap callers-of "pkg.Serve"

# Start an MCP server (for use with AI tools)
codemap mcp
```

## Features

### Symbol Queries (CLI)

| Command | What it does |
|---|---|
| `search <pattern>` | Find symbols by name. Matching spans every indexed field (qualified name, name, kind, receiver, signature, doc) and ranking puts name matches first |
| `show <qualified_name>` | Full details: signature, docs, file location, incoming/outgoing edges |
| `package <path>` | All symbols in a package with signatures and docs |
| `methods-of <type>` | All methods on a type |
| `list-packages` | List every indexed package |
| `search-text <pattern> [file-pattern]` | Fused file-content search: FTS5 merged with a substring scan (RRF), proximity-reranked, with typo correction. `--regex` switches to regex mode |
| `pattern <pattern>` | Match Go AST subtrees against a Go source snippet with `$UPPERCASE` metavariables (ast-grep style), e.g. `defer $CALL`; syntactic only, repeated metavariables must bind identical nodes |
| `diagnostics [file]` | Indexed compile errors (type-check and parse errors captured at index time; auto-refreshed via stale reindex) |

### Graph Queries (CLI)

| Command | What it does |
|---|---|
| `callers-of <name>` | Find all callers of a symbol (filter edge types with `--edge-types`; MCP `callers_of` adds transitive traversal via `depth`) |
| `callees-of <name>` | Find all symbols a function depends on |
| `importers-of <pkg>` | Packages that import the given package |
| `imports-of <pkg>` | Packages imported by the given package |
| `edges-by-type <type>` | All edges of a type: calls, references, satisfies (impl), embeds, imports |
| `all-edges` | Every relationship in the index |

### Code Analysis

`hotspots`, `importance`, and `changed-symbols` are available on the CLI; the traversal and analysis tools below them are exposed only through the **MCP server**.

| Command / Tool | Where | Description |
|---|---|---|
| `hotspots` / `get_hotspots` | CLI + MCP | Rank symbols by `complexity × churn` for refactoring risk (MCP adds `mode: "pagerank"` for call-graph centrality) |
| `importance` | CLI | Rank symbols by PageRank call-graph centrality |
| `changed-symbols [--ref R]` / `changed_symbols` | CLI + MCP | Working-tree changed symbols vs a git ref, classified by change type, with renamed files reported as `from -> to` |
| `search` `mode: "prefix"` | MCP | Find all symbols whose qualified name starts with a prefix |
| `search` `mode: "method"` | MCP | Find all methods with a given name across all types |
| `find_path` | MCP | BFS between two symbols to discover call/reference chains |
| `type_usage` | MCP | Find all symbols that use a given type in signatures |
| `interface_impls` | MCP | List all types implementing an interface and their methods |
| `unused` | MCP | Find dead code (unexported symbols with zero callers) |
| `cycles` | MCP | Detect circular dependencies (imports, calls) |
| `blast_radius` | MCP | Measure impact of changing a symbol (direct/transitive callers, embedders, type users) |
| `symbols_in_file` | MCP | Find all symbols defined in a given file |
| `dependency_flow` | MCP | One package's imports/importers/transitive imports; `layers: true` gives project-wide topological layering |
| `entry_points` | MCP | `main()` functions, test entries, uncalled exported symbols, HTTP handlers |
| `get_context_bundle` | MCP | Bundle a symbol with body, callees, callers, and same-file symbols under a token budget |
| `health` | MCP | Per-repo index freshness (indexed/stale/missing) |
| `stats` | MCP | Per-tool calls, errors, response bytes, estimated raw-read bytes, and reduction |
| `schema` | MCP | Schema of all codemap result types |
| `read_response_section` | MCP | Read back sections of an oversized response by index or keyword |
| **Generated-code handling** | both | Generated files (code-generation markers or filename conventions: `.pb.go`, `_string.go`, `mock_*`, `*_mock.go`, `zz_generated*`) are flagged in the index; search down-ranks (never hides) them by default, `--generated exclude\|only` filters them, and the hotspots, importance, and unused reports skip them |

### Cross-Repo Contract Intelligence

For multi-repo workspaces where binaries are coupled by wire protocols rather than Go imports, codemap detects and tracks **wire contracts**:

- **Contract edges** — links a producer/consumer symbol to its counterpart in another repo via two signals: a shared message-type string constant and a structural shape match (confidence-scored, with `suggested` below a threshold).
- **Structural drift** — compares struct shapes on their **effective wire name**: the first recognized serialization tag (`cbor`, `json`, `yaml`, `toml`, `bson`, `db`), falling back to the Go field name. Field additions are compatible; renames, removals, and type changes are breaking.
- **Runtime contracts** — Redis key patterns, JetStream subjects, WS type strings (plus any custom kinds) extracted from call-site literals, normalized for interpolation, with producer/consumer direction. Call-site detection is receiver-aware via an import gate: the built-in redis extractor fires only in files importing a Redis-protocol client (`redis`, `valkey`, `rueidis`, `keydb`), the jetstream extractor only in `nats` files, so a UI struct's `.Set(...)` or an AMQP `.Publish(...)` never becomes a fake Redis/JetStream contract. Strings are captured by a balanced-paren scanner, so interpolation never truncates a literal and multi-key commands (`MSet(k1, k2)`) capture every key.
- **Runtime extractor registry** — the built-in redis/jetstream/ws extractors ship as Go defaults and are merged with `contracts.extractors` from `codemap.yaml`, so a new infrastructure (Postgres, RabbitMQ, Kafka, ...) is a YAML entry, not new code. Per extractor you can pick:
  - `imports_match` — import substrings gating call-site extraction (empty fires anywhere; entries for a built-in kind extend its default gate),
  - `producer_methods`/`consumer_methods` — method names and their role,
  - `arg_index` — first string-literal argument holding the entity, 0-based, with every string arg from it captured (RabbitMQ `Publish(exchange, key, ...)` uses `1`),
  - `const_prefix` — shared-constant entities by symbol-name prefix plus decode-switch scoping,
  - `config_patterns` — regexes over config literals whose capture group 1 is the entity,
  - `normalize` — a built-in normalizer (`raw` | `redis` | `subject`),
  - `normalize_rules` — inline ordered `{regex, replace}` rules applied after `normalize`, so any pattern shape normalizes without Go changes.
  Anything describable as a call-site literal, config literal, or const-named entity is pure config; only a fundamentally new *scan semantics* (not method calls, config fields, or constants) would need a Go scanner. Config is validated at build time: a negative `arg_index`, an empty `kind`, a malformed `contracts.suppress` entry (missing `→`), or an invalid `normalize_rules` regex fails loudly naming the offending extractor/rule; an unknown `normalize` name warns and falls back to raw.
- **Suppression** — `contracts.suppress` lists `from → to` pairs to drop; suppressed pairs never appear.

Commands: `contracts`, `contract-drift`, `runtime-contracts` (MCP: `contracts`, `contract_drift`, `runtime_contracts`, `suppress_contract`). Contract edges participate in blast-radius and path-finding.

### Output Formats
- **TOON** (default) — token-efficient output optimized for LLM consumption
- **JSON** — machine-readable (`--json`)
- **Compact** — minimal (`--compact`)

The MCP server always renders TOON.

### MCP Server

`codemap mcp` (alias: `codemap serve`) starts a JSON-RPC MCP server over stdio, exposing the query and analysis capabilities as tools for AI coding assistants. This enables AI agents to navigate Go codebases faster and more precisely than grep/glob.

Unlike the CLI, the MCP server keeps itself fresh: it indexes the repo automatically on the first tool call and re-indexes when Go files change. `reindex` forces a full rebuild (the CLI spells this `index`).

Tools: `reindex`, `health`, `overview`, `schema`, `stats`, `show`, `search`, `package`, `methods_of`, `symbols_in_file`, `callers_of`, `callees_of`, `imports_of`, `all_edges`, `find_path`, `cycles`, `dependency_flow`, `blast_radius`, `type_usage`, `interface_impls`, `entry_points`, `unused`, `search_text`, `pattern`, `get_context_bundle`, `diagnostics`, `changed_symbols`, `get_hotspots`, `contracts`, `contract_drift`, `runtime_contracts`, `suppress_contract`, `read_response_section`.

Responses over 16 KiB are stored and replaced by a searchable section manifest, so a large result never floods the context window; `read_response_section` pulls back only the sections you need. Override the threshold with `CODEMAP_RESPONSE_LIMIT`.

### Init Command

`codemap inject` (alias: `codemap init`) installs MCP and guard integrations for the AI tools it finds:

- **Context file** — writes navigation documentation to `~/.config/opencode/context/codemap/navigation.md`
- **Plugin** — installs a soft-mode guard to `~/.config/opencode/plugins/codemap-guard.ts` that warns agents when they use grep/glob/read on `.go` files instead of codemap MCP tools
- **Crush** — injects the MCP entry into `~/.config/crush/crush.json`
- **Command Code** — installs a soft-mode guard mod to `~/.commandcode/mods/codemap-guard.ts` (skipped when `~/.commandcode` is absent)
- **Maki** — registers `[mcp.codemap]` in `mcp.toml`, installs a Lua guard plugin (`codemap-guard.lua`) loaded from `init.lua`, and writes permission grants to `plugin.toml`, into `~/.maki` (or `~/.config/maki`); skipped when neither config dir exists. This guard rides along with maki's own `index` tool: after a successful non-empty skeleton of a `.go` file it appends a pointer to the codemap tools that answer what a skeleton cannot (callers, blast radius, interface implementations, symbol search).
- **AGENTS.md** — creates project-level guidance for AI agents (skipped if already exists)

Re-run `codemap init` after upgrading codemap to refresh the installed plugins, navigation doc, and guards; the command reports `updated` or `unchanged` per artifact, and each guard carries a `Version:` marker.

`codemap update-agents` patches an existing `AGENTS.md` with a newer usage tip instead of recreating it.

## Usage

```
codemap <command> [args] [flags]
```

### Commands

`inject`/`init`, `update-agents`, `index [path]`, `changed-symbols [--ref R]`, `overview`, `show <name>`, `callers-of <name>`, `callees-of <name>`, `search <pattern>`, `importers-of <pkg>`, `imports-of <pkg>`, `edges-by-type <type>`, `all-edges`, `list-packages`, `package <path>`, `methods-of <type>`, `search-text <pattern> [file-pattern]`, `pattern <pattern>`, `hotspots`, `importance`, `diagnostics [file]`, `contracts`, `contract-drift`, `runtime-contracts`, `workspace <subcommand>`, `serve`/`mcp`.

### Workspace Subcommands

| Subcommand | What it does |
|---|---|
| `workspace index [path]` | Index the workspace declared in `codemap.yaml` |
| `workspace status` (alias `health`) | Show per-repo index state (indexed/stale/missing) |
| `workspace reindex` | Reindex stale repos |
| `workspace changed-symbols` | Workspace-wide changed symbols (per-repo against its own ref) |

### Flags

| Flag | Description |
|---|---|
| `--include-tests` | Include test packages and symbols |
| `--include-unexported` | Include private symbols (for `package`) |
| `--full-docs` | Show full doc comments instead of first sentence |
| `--json` / `--toon` / `--compact` | Output format |
| `--db <path>` | Database path (default: `.codemap/codemap.db`) |
| `--kind <kind>` | Symbol kind filter for `search` (function, method, type, interface, alias, const, var); for `runtime-contracts` it filters the contract kind (redis, jetstream, ws_type) |
| `--exported <bool>` | Filter by exported status |
| `--edge-types <types>` | Comma-separated edge type filter (calls, references, satisfies, embeds, imports) |
| `--package <pkg>` | Filter by package path |
| `--repo <module>` | Scope query to one workspace repo (`search`, `show`, `callers-of`, `callees-of`, `importers-of`, `imports-of`, `pattern`, `diagnostics`, `contracts`, `contract-drift`, `runtime-contracts`) |
| `--ref <git-ref>` | Diff base for `changed-symbols` (default: integration branch) |
| `--generated <mode>` | Generated-code handling for search: `any` (default), `exclude`, or `only` |
| `--file-pattern <glob>` | Filter by file-path substring (for `pattern`) |
| `--regex` | Use regex mode (for `search-text`) |
| `--context-lines <n>` | Context lines around matches (for `search-text`) |
| `--top-n <n>` | Number of results (for `hotspots`/`importance`) |
| `--min-complexity <n>` / `--min-churn <n>` | Minimum thresholds (for `hotspots`) |
| `--workspace` | Index as a workspace (with `index`) |
| `--discover` | Auto-discover sibling Go modules as a workspace (with `index --workspace`) |
| `--severity <level>` | Filter contract results by severity: compatible/breaking/unknown (for `contracts` and `contract-drift`) |
| `--direction <dir>` / `--min-confidence <f>` | Filter contract results by direction (producer/consumer/shared) and minimum confidence (for `contracts`) |

## How It Works

1. **Parse** — enumerates packages with `go list` (falling back to a directory walk) and walks Go source files using `go/parser` and `go/ast` to extract symbols and import edges
2. **Resolve** — uses `go/types` to resolve type information, method receivers, interface satisfaction, and embedded types
3. **Store** — persists the graph (symbols + typed edges), file contents, and churn data in a local SQLite database
4. **Query** — provides a query layer with filtering, traversal, and analysis on top of the stored graph

The CLI reads an existing index and errors with a hint when none is present, so run `codemap index` (or `codemap index --workspace`) first. The MCP server indexes automatically on first use and re-indexes when source files change.
