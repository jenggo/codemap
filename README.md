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

### Output Formats
- **TOON** (default) — human-readable terminal output
- **JSON** — machine-readable (`--json`)
- **Compact** — minimal (`--compact`)

### MCP Server

`codemap serve` starts a JSON-RPC MCP server over stdio, exposing all query capabilities as tools for AI coding assistants. This enables AI agents to navigate Go codebases faster and more precisely than grep/glob.

### Init Command

`codemap init` auto-generates context files for:
- **Crush** — injects MCP config into `~/.config/crush/crush.json`
- **Opencode** — writes navigation documentation to `~/.config/opencode/context/codemap/navigation.md`
- **AGENTS.md** — creates project-level guidance for AI agents

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
