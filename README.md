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
codemap serve
```

## Features

### Symbol Queries
| Command | What it does |
|---|---|
| `search <pattern>` | Find symbols by name (functions, types, methods, consts, vars) |
| `show <qualified_name>` | Full details: signature, docs, file location, incoming/outgoing edges |
| `package <path>` | All symbols in a package with signatures and docs |
| `methods-of <type>` | All methods on a type |
| `list-packages` | List every indexed package |

### Graph Queries
| Command | What it does |
|---|---|
| `callers-of <name>` | Find all callers of a symbol (transitively via `--edge-types` + `--depth`) |
| `callees-of <name>` | Find all symbols a function depends on |
| `importers-of <pkg>` | Packages that import the given package |
| `imports-of <pkg>` | Packages imported by the given package |
| `transitive-imports <pkg>` | Full dependency tree |
| `edges-by-type <type>` | All edges of a type: calls, references, satisfies (impl), embeds, imports |
| `all-edges` | Every relationship in the index |

### Code Analysis
| Feature | Description |
|---|---|
| **Path finding** | BFS between two symbols to discover call/reference chains |
| **Type usage** | Find all symbols that use a given type in signatures |
| **Interface implementations** | List all types implementing an interface and their methods |
| **Unused symbols** | Find dead code (unexported symbols with zero callers) |
| **Cycle detection** | Detect circular dependencies (imports, calls) |
| **Blast radius** | Measure impact of changing a symbol (direct/transitive callers, embedders, type users) |
| **Symbols in file** | Find all symbols defined in a given file |
| **Search by prefix** | Find all symbols whose qualified name starts with a prefix |
| **Method search** | Find all methods with a given name across all types |

### Cross-Repo Contract Intelligence

For multi-repo workspaces where binaries are coupled by wire protocols rather than Go imports, codemap detects and tracks **wire contracts**:

- **Contract edges** — links a producer/consumer symbol to its counterpart in another repo via two signals: a shared message-type string constant and a structural shape match (confidence-scored, with `suggested` below a threshold).
- **Structural drift** — compares struct shapes on their **effective wire name**: the first recognized serialization tag (`cbor`, `json`, `yaml`, `toml`, `bson`, `db`), falling back to the Go field name. Field additions are compatible; renames, removals, and type changes are breaking.
- **Runtime contracts** — Redis key patterns, JetStream subjects, WS type strings (plus any custom kinds) extracted from call-site literals, normalized for interpolation, with producer/consumer direction. Call-site detection is receiver-aware via an import gate: the built-in redis extractor fires only in files importing a Redis-protocol client (`redis`, `valkey`, `rueidis`, `keydb`), the jetstream extractor only in `nats` files, so a UI struct's `.Set(...)` or an AMQP `.Publish(...)` never becomes a fake Redis/JetStream contract. Strings are captured by a balanced-paren scanner, so interpolation never truncates a literal and multi-key commands (`MSet(k1, k2)`) capture every key.
- **Runtime extractor registry** — the built-in redis/jetstream/ws extractors are data-driven (`contracts.extractors` in `codemap.yaml`), so a new infrastructure (Postgres, RabbitMQ, Kafka, ...) is a YAML entry, not new code. Per extractor you can pick:
  - `imports_match` — import substrings gating call-site extraction (empty fires anywhere; entries for a built-in kind extend its default gate),
  - `producer_methods`/`consumer_methods` — method names and their role,
  - `arg_index` — first string-literal argument holding the entity, 0-based, with every string arg from it captured (RabbitMQ `Publish(exchange, key, ...)` uses `1`),
  - `const_prefix` — shared-constant entities by symbol-name prefix plus decode-switch scoping,
  - `config_patterns` — regexes over config literals whose capture group 1 is the entity,
  - `normalize` — a built-in normalizer (`raw` | `redis` | `subject`),
  - `normalize_rules` — inline ordered `{regex, replace}` rules applied after `normalize`, so any pattern shape normalizes without Go changes.
  Anything describable as a call-site literal, config literal, or const-named entity is pure config; only a fundamentally new *scan semantics* (not method calls, config fields, or constants) would need a Go scanner. Config is validated at build time: a negative `arg_index`, an empty `kind`, a malformed `contracts.suppress` entry (missing `→`), or an invalid `normalize_rules` regex fails loudly naming the offending extractor/rule; an unknown `normalize` name warns and falls back to raw.
- **Suppression** — `contracts.suppress` drops specific from→to pairs; suppressed pairs never appear.

Commands: `contracts`, `contract-drift`, `runtime-contracts` (MCP: `contracts`, `contract_drift`, `runtime_contracts`, `suppress_contract`). Contract edges participate in blast-radius and path-finding.

### Output Formats
- **TOON** (default) — token-efficient output optimized for LLM consumption
- **JSON** — machine-readable (`--json`)
- **Compact** — minimal (`--compact`)

### MCP Server

`codemap serve` starts a JSON-RPC MCP server over stdio, exposing all query capabilities as tools for AI coding assistants. This enables AI agents to navigate Go codebases faster and more precisely than grep/glob.

### Init Command

`codemap inject` (alias: `codemap init`) installs opencode integration files:
- **Context file** — writes navigation documentation to `~/.config/opencode/context/codemap/navigation.md`
- **Plugin** — installs a soft-mode guard to `~/.config/opencode/plugins/codemap-guard.ts` that warns agents when they use grep/glob/read on `.go` files instead of codemap MCP tools
- **Crush** — injects MCP config into `~/.config/crush/crush.json`
- **AGENTS.md** — creates project-level guidance for AI agents (skipped if already exists)

## Usage

```
codemap <command> [args] [flags]
```

### Flags
| Flag | Description |
|---|---|
| `--include-tests` | Include test packages and symbols |
| `--include-unexported` | Include private symbols (for `package` command) |
| `--full-docs` | Show full doc comments instead of first sentence |
| `--json` / `--toon` / `--compact` | Output format |
| `--db <path>` | Database path (default: `.codemap/codemap.db`) |
| `--kind <kind>` | Filter by symbol kind (function, method, type, const, var, interface) |
| `--exported <bool>` | Filter by exported status |
| `--edge-types <types>` | Comma-separated edge type filter (calls, references, satisfies, embeds, imports) |
| `--package <pkg>` | Filter by package path |

## How It Works

1. **Parse** — walks Go source files using `go/parser` and `go/ast` to extract symbols and import edges
2. **Resolve** — uses `go/types` to resolve type information, method receivers, interface satisfaction, and embedded types
3. **Store** — persists the graph (symbols + typed edges) in a local SQLite database
4. **Query** — provides a query layer with filtering, traversal, and analysis on top of the stored graph

Indexing is automatic on first query and re-indexes when source files change. A manual `index` command is available to force re-indexing.

## Installation

```bash
go install codemap/cmd/codemap@latest
```

Requires Go 1.26+.
