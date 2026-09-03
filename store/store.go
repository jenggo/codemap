package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	sqlite "modernc.org/sqlite"

	"codemap/parse"
	"codemap/resolve"
	"codemap/vcs"
)

var regexCache sync.Map

func cachedRegex(pattern string) (*regexp.Regexp, error) {
	if value, ok := regexCache.Load(pattern); ok {
		return value.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	actual, _ := regexCache.LoadOrStore(pattern, re)
	return actual.(*regexp.Regexp), nil
}

func openDB(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func jsonUnmarshal(data string, v any) error {
	return json.Unmarshal([]byte(data), v)
}

func init() {
	_ = sqlite.RegisterDeterministicScalarFunction(
		"regexp",
		2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			pattern, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("regexp: pattern must be a string, got %T", args[0])
			}
			s, ok := args[1].(string)
			if !ok {
				return nil, fmt.Errorf("regexp: value must be a string, got %T", args[1])
			}
			re, err := cachedRegex(pattern)
			if err != nil {
				return nil, err
			}
			if re.MatchString(s) {
				return int64(1), nil
			}
			return int64(0), nil
		},
	)
}

// parenReceiverRe matches a Go-style receiver wrapper in a qualified name,
// e.g. "pkg/path.(*Type).Method" or "pkg/path.(Type).Method", and is used by
// NormalizeQualifiedName to reduce it to the indexed form "pkg/path.Type.Method".
var parenReceiverRe = regexp.MustCompile(`\.\((?:\*?)([^()]+)\)\.`)

// NormalizeQualifiedName reduces a qualified name that may carry a Go-style
// receiver wrapper ("pkg/path.(*Type).Method", "pkg/path.(Type).Method") to the
// form stored in the index ("pkg/path.Type.Method"). Names already in the
// indexed form are returned unchanged, so the call is idempotent.
func NormalizeQualifiedName(qn string) string {
	if !strings.Contains(qn, "(") {
		return qn
	}
	return parenReceiverRe.ReplaceAllString(qn, ".$1.")
}

const schemaVersion = 4

const schemaSQL = `
CREATE TABLE IF NOT EXISTS repos (
    id          INTEGER PRIMARY KEY,
    module_path TEXT    NOT NULL UNIQUE,
    dir         TEXT    NOT NULL,
    missing     BOOLEAN NOT NULL DEFAULT FALSE,
    indexed_at  TEXT
);

CREATE TABLE IF NOT EXISTS packages (
    id          INTEGER PRIMARY KEY,
    path        TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL,
    dir         TEXT    NOT NULL,
    is_test     BOOLEAN NOT NULL DEFAULT FALSE,
    repo_id     INTEGER REFERENCES repos(id)
);

CREATE TABLE IF NOT EXISTS symbols (
    id              INTEGER PRIMARY KEY,
    qualified_name  TEXT    NOT NULL UNIQUE,
    package_id      INTEGER NOT NULL REFERENCES packages(id),
    name            TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    receiver        TEXT,
    signature       TEXT,
    doc             TEXT,
    pos_file        TEXT    NOT NULL,
    pos_line        INTEGER NOT NULL,
    exported        BOOLEAN NOT NULL DEFAULT FALSE,
    is_test         BOOLEAN NOT NULL DEFAULT FALSE,
    complexity      INTEGER NOT NULL DEFAULT 0,
    churn_count     INTEGER NOT NULL DEFAULT 0,
    importance      REAL    NOT NULL DEFAULT 0.0,
    repo_id         INTEGER REFERENCES repos(id),
    fields_json     TEXT
);

CREATE TABLE IF NOT EXISTS edges (
    id          INTEGER PRIMARY KEY,
    from_ref    TEXT    NOT NULL,
    to_ref      TEXT    NOT NULL,
    edge_type   TEXT    NOT NULL,
    pos_file    TEXT    NOT NULL,
    pos_line    INTEGER NOT NULL,
    sites       TEXT    NOT NULL DEFAULT '[]',
    repo_id     INTEGER REFERENCES repos(id)
);

CREATE TABLE IF NOT EXISTS files (
    path    TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    repo_id INTEGER REFERENCES repos(id)
);

CREATE INDEX IF NOT EXISTS idx_symbols_package   ON symbols(package_id);
CREATE INDEX IF NOT EXISTS idx_symbols_qualified  ON symbols(qualified_name);
CREATE INDEX IF NOT EXISTS idx_symbols_kind        ON symbols(kind);
CREATE INDEX IF NOT EXISTS idx_symbols_repo        ON symbols(repo_id);
CREATE INDEX IF NOT EXISTS idx_symbols_pos_file    ON symbols(pos_file);
CREATE INDEX IF NOT EXISTS idx_edges_from          ON edges(from_ref);
CREATE INDEX IF NOT EXISTS idx_edges_to_ref        ON edges(to_ref);
CREATE INDEX IF NOT EXISTS idx_edges_type          ON edges(edge_type);
CREATE INDEX IF NOT EXISTS idx_edges_repo          ON edges(repo_id);
CREATE UNIQUE INDEX IF NOT EXISTS edges_pair       ON edges(COALESCE(repo_id, 0), from_ref, to_ref, edge_type);
CREATE INDEX IF NOT EXISTS idx_packages_repo       ON packages(repo_id);

CREATE TABLE IF NOT EXISTS contracts (
    id          INTEGER PRIMARY KEY,
    from_ref    TEXT    NOT NULL,
    to_ref      TEXT    NOT NULL,
    direction   TEXT    NOT NULL,
    confidence  REAL    NOT NULL DEFAULT 0.0,
    severity    TEXT    NOT NULL DEFAULT 'unknown',
    suggested   BOOLEAN NOT NULL DEFAULT FALSE,
    evidence    TEXT,
    indexed_at  TEXT    NOT NULL,
    repo_id     INTEGER REFERENCES repos(id)
);

CREATE INDEX IF NOT EXISTS idx_contracts_from     ON contracts(from_ref);
CREATE INDEX IF NOT EXISTS idx_contracts_to_ref   ON contracts(to_ref);
CREATE INDEX IF NOT EXISTS idx_contracts_direction ON contracts(direction);
CREATE INDEX IF NOT EXISTS idx_contracts_severity  ON contracts(severity);
CREATE INDEX IF NOT EXISTS idx_contracts_repo      ON contracts(repo_id);

CREATE TABLE IF NOT EXISTS runtime_contracts (
    id          INTEGER PRIMARY KEY,
    kind        TEXT    NOT NULL,
    pattern     TEXT    NOT NULL,
    from_ref    TEXT    NOT NULL,
    to_ref      TEXT    NOT NULL,
    direction   TEXT    NOT NULL,
    evidence    TEXT,
    indexed_at  TEXT    NOT NULL,
    repo_id     INTEGER REFERENCES repos(id)
);

CREATE INDEX IF NOT EXISTS idx_rt_contracts_kind   ON runtime_contracts(kind);
CREATE INDEX IF NOT EXISTS idx_rt_contracts_pattern ON runtime_contracts(pattern);
CREATE INDEX IF NOT EXISTS idx_rt_contracts_repo    ON runtime_contracts(repo_id);

CREATE TABLE IF NOT EXISTS drift (
    id          INTEGER PRIMARY KEY,
    from_ref    TEXT    NOT NULL,
    to_ref      TEXT    NOT NULL,
    severity    TEXT    NOT NULL,
    fields_json TEXT,
    indexed_at  TEXT    NOT NULL,
    repo_id     INTEGER REFERENCES repos(id)
);

CREATE INDEX IF NOT EXISTS idx_drift_from        ON drift(from_ref);
CREATE INDEX IF NOT EXISTS idx_drift_to_ref      ON drift(to_ref);
CREATE INDEX IF NOT EXISTS idx_drift_severity    ON drift(severity);
CREATE INDEX IF NOT EXISTS idx_drift_repo        ON drift(repo_id);

CREATE TABLE IF NOT EXISTS contract_suppressions (
    from_ref TEXT NOT NULL,
    to_ref   TEXT NOT NULL,
    PRIMARY KEY (from_ref, to_ref)
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
    qualified_name,
    name,
    kind,
    receiver,
    signature,
    doc,
    content=symbols,
    content_rowid=id,
    tokenize='porter unicode61'
);

CREATE VIRTUAL TABLE IF NOT EXISTS file_content_fts USING fts5(
    path,
    content,
    content=files,
    content_rowid=rowid,
    tokenize='trigram'
);

CREATE VIRTUAL TABLE IF NOT EXISTS response_chunks USING fts5(
    response_id UNINDEXED,
    tool        UNINDEXED,
    section_idx UNINDEXED,
    title       UNINDEXED,
    content,
    byte_count  UNINDEXED,
    created_at  UNINDEXED,
    tokenize='trigram'
);
`

const andIsTestFalse = " AND s.is_test = FALSE"
const orderByQualifiedName = " ORDER BY s.qualified_name"

type Store struct {
	db *sql.DB

	// testHook, when set, is invoked at named points in the write path.
	// Returning a non-nil error aborts the enclosing transaction. Test-only
	// fault-injection seam for simulating failures at each write stage.
	testHook func(stage, key string) error
}

func DefaultPath() string {
	return filepath.Join(".codemap", "codemap.db")
}

func Create(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	gitignorePath := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		_ = os.WriteFile(gitignorePath, []byte("*\n"), 0644)
	}

	db, err := openDB(path)
	if err != nil {
		return nil, err
	}

	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := ensureSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func Open(path string) (*Store, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("codemap index not found: run 'codemap index' first or use the index MCP tool")
	}

	db, err := openDB(path)
	if err != nil {
		return nil, err
	}

	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM symbols").Scan(&count); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("codemap database is empty: run 'codemap index' first or use the index MCP tool")
	}

	// Upgrade older DBs additively so single-repo databases continue to open
	// unchanged (repo_id stays NULL until a workspace index runs).
	if err := ensureSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// ensureSchema migrates an existing database to the current schema version.
// All changes are additive: new table, new nullable columns. Existing
// single-repo rows keep repo_id NULL and behave exactly as before.
func ensureSchema(db *sql.DB) error {
	ctx := context.Background()

	// Existing databases below the current schema version get destructive
	// migrations (see migrateFileContentFTS). Fresh databases created by
	// schemaSQL already carry the latest schema, so a missing version row is
	// treated as current.
	currentVersion, err := readSchemaVersion(ctx, db)
	if err != nil {
		return err
	}

	if err := migrateFromVersion(ctx, db, currentVersion); err != nil {
		return err
	}

	if !tableExists(db, "repos") {
		if _, err := db.ExecContext(ctx, `CREATE TABLE repos (
			id          INTEGER PRIMARY KEY,
			module_path TEXT    NOT NULL UNIQUE,
			dir         TEXT    NOT NULL,
			missing     BOOLEAN NOT NULL DEFAULT FALSE,
			indexed_at  TEXT
		)`); err != nil {
			return err
		}
	}

	if err := ensureRepoIDColumns(ctx, db); err != nil {
		return err
	}

	// Struct field info for contract shape matching (additive).
	hasFields, err := columnExists(db, "symbols", "fields_json")
	if err != nil {
		return fmt.Errorf("ensure schema: inspect symbols.fields_json: %w", err)
	}
	if !hasFields {
		if _, err := db.ExecContext(ctx, `ALTER TABLE symbols ADD COLUMN fields_json TEXT`); err != nil {
			return err
		}
	}

	// Edge deduplication: the edges table gains a sites column aggregating the
	// distinct occurrence locations per unique (repo_id, from_ref, to_ref,
	// edge_type) pair. Existing databases may already contain duplicate rows;
	// collapse them (keeping the first row per pair, MIN(rowid)) before the
	// unique index is built, or the index creation would fail.
	if err := migrateEdgesDedup(db); err != nil {
		return err
	}

	// Overflow response chunks (additive FTS5 table).
	if err := migrateResponseChunks(ctx, db); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, strconv.Itoa(schemaVersion)); err != nil {
		return err
	}

	// Churn application looks symbols up by pos_file; older databases lack the
	// index and would fall back to full-table scans per update.
	if !indexExists(db, "idx_symbols_pos_file") {
		if _, err := db.ExecContext(ctx, `CREATE INDEX idx_symbols_pos_file ON symbols(pos_file)`); err != nil {
			return err
		}
	}

	return ensureContractTables(db)
}

// readSchemaVersion returns the schema version recorded in meta, or 0 when no
// version row exists (fresh databases created by schemaSQL).
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("ensure schema: read schema_version: %w", err)
		}
		return 0, nil
	}
	return version, nil
}

// migrateFromVersion runs any migrations required to bring a database recorded
// at version into line with the current schema. Fresh databases (version 0)
// were created by schemaSQL and need nothing.
func migrateFromVersion(ctx context.Context, db *sql.DB, version int) error {
	if version <= 0 || version >= schemaVersion {
		return nil
	}
	return migrateFileContentFTS(ctx, db)
}

// ensureRepoIDColumns adds the repo_id foreign-key column to single-repo tables
// when missing (additive migration from the pre-workspace schema).
func ensureRepoIDColumns(ctx context.Context, db *sql.DB) error {
	const repoColumnDecl = "INTEGER REFERENCES repos(id)"
	const repoIDCol = "repo_id"
	for _, tc := range []struct{ table, column, decl string }{
		{"packages", repoIDCol, repoColumnDecl},
		{"symbols", repoIDCol, repoColumnDecl},
		{"edges", repoIDCol, repoColumnDecl},
		{"files", repoIDCol, repoColumnDecl},
	} {
		hasCol, err := columnExists(db, tc.table, tc.column)
		if err != nil {
			return fmt.Errorf("ensure schema: inspect %s.%s: %w", tc.table, tc.column, err)
		}
		if !hasCol {
			if _, err := db.ExecContext(ctx, "ALTER TABLE "+tc.table+" ADD COLUMN "+tc.column+" "+tc.decl); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateFileContentFTS recreates file_content_fts with the trigram tokenizer.
// An FTS5 table's tokenizer is fixed at creation time, so enabling substring
// matches inside camelCase identifiers (the porter/unicode61 tokenizer stores
// each identifier as a single term) requires dropping the virtual table and
// rebuilding it from the external `files` content table. Only databases whose
// recorded schema version predates v4 run this; fresh databases already get
// the trigram table from schemaSQL.
func migrateFileContentFTS(ctx context.Context, db *sql.DB) error {
	if !tableExists(db, "file_content_fts") {
		return nil
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS file_content_fts`); err != nil {
		return fmt.Errorf("migrate file_content_fts: drop: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE VIRTUAL TABLE file_content_fts USING fts5(
		path,
		content,
		content=files,
		content_rowid=rowid,
		tokenize='trigram'
	)`); err != nil {
		return fmt.Errorf("migrate file_content_fts: recreate with trigram: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO file_content_fts(file_content_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("migrate file_content_fts: rebuild: %w", err)
	}
	return nil
}

// migrateResponseChunks creates the response_chunks FTS5 table on databases
// created before overflow indexing existed (all additive). Fresh databases
// already carry the table from schemaSQL.
func migrateResponseChunks(ctx context.Context, db *sql.DB) error {
	if tableExists(db, "response_chunks") {
		return nil
	}
	_, err := db.ExecContext(ctx, `CREATE VIRTUAL TABLE response_chunks USING fts5(
		response_id UNINDEXED,
		tool        UNINDEXED,
		section_idx UNINDEXED,
		title       UNINDEXED,
		content,
		byte_count  UNINDEXED,
		created_at  UNINDEXED,
		tokenize='trigram'
	)`)
	if err != nil {
		return fmt.Errorf("migrate response_chunks: %w", err)
	}
	return nil
}

// ensureContractTables creates the contract intelligence tables and indexes
// when they are missing (all additive).
func ensureContractTables(db *sql.DB) error {
	ctx := context.Background()
	for _, t := range contractTables {
		if tableExists(db, t.name) {
			continue
		}
		if _, err := db.ExecContext(ctx, t.create); err != nil {
			return err
		}
		for _, idx := range t.indexes {
			if _, err := db.ExecContext(ctx, idx); err != nil {
				return err
			}
		}
	}
	return nil
}

// schemaTable describes one additive table creation.
type schemaTable struct {
	name    string
	create  string
	indexes []string
}

var contractTables = []schemaTable{
	{
		name: "contracts",
		create: `CREATE TABLE contracts (
			id          INTEGER PRIMARY KEY,
			from_ref    TEXT    NOT NULL,
			to_ref      TEXT    NOT NULL,
			direction   TEXT    NOT NULL,
			confidence  REAL    NOT NULL DEFAULT 0.0,
			severity    TEXT    NOT NULL DEFAULT 'unknown',
			suggested   BOOLEAN NOT NULL DEFAULT FALSE,
			evidence    TEXT,
			indexed_at  TEXT    NOT NULL,
			repo_id     INTEGER REFERENCES repos(id)
		)`,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_contracts_from     ON contracts(from_ref)`,
			`CREATE INDEX IF NOT EXISTS idx_contracts_to_ref   ON contracts(to_ref)`,
			`CREATE INDEX IF NOT EXISTS idx_contracts_direction ON contracts(direction)`,
			`CREATE INDEX IF NOT EXISTS idx_contracts_severity  ON contracts(severity)`,
			`CREATE INDEX IF NOT EXISTS idx_contracts_repo      ON contracts(repo_id)`,
		},
	},
	{
		name: "runtime_contracts",
		create: `CREATE TABLE runtime_contracts (
			id          INTEGER PRIMARY KEY,
			kind        TEXT    NOT NULL,
			pattern     TEXT    NOT NULL,
			from_ref    TEXT    NOT NULL,
			to_ref      TEXT    NOT NULL,
			direction   TEXT    NOT NULL,
			evidence    TEXT,
			indexed_at  TEXT    NOT NULL,
			repo_id     INTEGER REFERENCES repos(id)
		)`,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_rt_contracts_kind   ON runtime_contracts(kind)`,
			`CREATE INDEX IF NOT EXISTS idx_rt_contracts_pattern ON runtime_contracts(pattern)`,
			`CREATE INDEX IF NOT EXISTS idx_rt_contracts_repo    ON runtime_contracts(repo_id)`,
		},
	},
	{
		name: "drift",
		create: `CREATE TABLE drift (
			id          INTEGER PRIMARY KEY,
			from_ref    TEXT    NOT NULL,
			to_ref      TEXT    NOT NULL,
			severity    TEXT    NOT NULL,
			fields_json TEXT,
			indexed_at  TEXT    NOT NULL,
			repo_id     INTEGER REFERENCES repos(id)
		)`,
		indexes: []string{
			`CREATE INDEX IF NOT EXISTS idx_drift_from        ON drift(from_ref)`,
			`CREATE INDEX IF NOT EXISTS idx_drift_to_ref      ON drift(to_ref)`,
			`CREATE INDEX IF NOT EXISTS idx_drift_severity    ON drift(severity)`,
			`CREATE INDEX IF NOT EXISTS idx_drift_repo        ON drift(repo_id)`,
		},
	},
	{
		name: "contract_suppressions",
		create: `CREATE TABLE contract_suppressions (
			from_ref TEXT NOT NULL,
			to_ref   TEXT NOT NULL,
			PRIMARY KEY (from_ref, to_ref)
		)`,
	},
}

// migrateEdgesDedup upgrades the edges table to the deduplicated schema: it adds
// the aggregate `sites` column and, when the unique pair index is missing,
// collapses duplicate rows first so the index build can succeed.
func migrateEdgesDedup(db *sql.DB) error {
	ctx := context.Background()
	hasSites, err := columnExists(db, "edges", "sites")
	if err != nil {
		return fmt.Errorf("migrate edges dedup: inspect edges.sites: %w", err)
	}
	if !hasSites {
		if _, err := db.ExecContext(ctx, `ALTER TABLE edges ADD COLUMN sites TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return err
		}
	}
	if indexExists(db, "edges_pair") {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		DELETE FROM edges
		WHERE rowid NOT IN (
			SELECT MIN(rowid)
			FROM edges
			GROUP BY COALESCE(repo_id, 0), from_ref, to_ref, edge_type
		)`); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `CREATE UNIQUE INDEX edges_pair ON edges(COALESCE(repo_id, 0), from_ref, to_ref, edge_type)`)
	return err
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return err == nil && n > 0
}

func indexExists(db *sql.DB, name string) bool {
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n)
	return err == nil && n > 0
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// withTx runs fn inside a single transaction and commits it only when fn
// returns nil. Any error rolls the transaction back, preserving the previous
// committed state.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// maybeFail is the fault-injection seam used by tests to simulate a failure at
// a specific write stage or item.
func (s *Store) maybeFail(stage, key string) error {
	if s.testHook == nil {
		return nil
	}
	return s.testHook(stage, key)
}

func (s *Store) Write(result *resolve.Result, files map[string]string, churn map[string]int) error {
	return s.write(result, files, churn, nil, false)
}

// WriteWorkspace writes a full workspace index: every member repo is
// registered in the repos table and packages/symbols/edges/files are
// attributed to it. Rows from any previous workspace index (repo_id not NULL)
// are replaced atomically.
func (s *Store) WriteWorkspace(result *resolve.Result, files map[string]string, churn map[string]int, repos []RepoSpec) error {
	return s.write(result, files, churn, repos, true)
}

func (s *Store) write(result *resolve.Result, files map[string]string, churn map[string]int, repos []RepoSpec, workspace bool) error {
	ctx := context.Background()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.writeTx(ctx, tx, result, files, churn, repos, workspace)
	})
}

// writeTx performs the whole index write inside the caller's transaction:
// packages, symbols, edges, churn, file contents, then both FTS rebuilds. Any
// failure rolls everything back, preserving the previous committed index.
func (s *Store) writeTx(ctx context.Context, tx *sql.Tx, result *resolve.Result, files map[string]string, churn map[string]int, repos []RepoSpec, workspace bool) error {
	repoByModule, err := registerRepos(ctx, tx, repos, workspace)
	if err != nil {
		return err
	}

	// Single-repo mode has no repo attribution (repo_id NULL); delete the
	// previous index's rows so a full re-index is an atomic swap rather than
	// an append that accumulates stale packages/symbols/edges/files.
	if !workspace {
		if err := clearSingleRepoRows(ctx, tx); err != nil {
			return err
		}
	}

	pkgCache, err := writePackages(ctx, tx, result.Packages, repoByModule)
	if err != nil {
		return err
	}
	attrib := newPkgAttribution(pkgCache, repoByModule)

	if err := writeSymbols(ctx, tx, result.Symbols, attrib); err != nil {
		return err
	}

	if err := writeEdges(ctx, tx, result.Edges, attrib); err != nil {
		return err
	}

	if churn != nil {
		if err := applyChurn(ctx, tx, churn); err != nil {
			return err
		}
	}

	var repoIDFor func(string) int64
	if workspace {
		repoIDFor = buildRepoIDFor(repos, repoByModule)
	}
	scope := fileScope{repoID: 0}
	fileDirty, err := s.writeFiles(ctx, tx, files, repoIDFor, scope)
	if err != nil {
		return err
	}

	if err := s.maybeFail("fts.symbols", ""); err != nil {
		return err
	}
	if err := s.populateFTS(ctx, tx); err != nil {
		return err
	}

	// The file-content FTS is rebuilt only when a file was inserted, updated with
	// different content, or deleted. An incremental re-index of unchanged files
	// skips the rebuild entirely (rowids stay stable because unchanged rows are
	// not rewritten).
	if err := s.maybeFail("fts.content", ""); err != nil {
		return err
	}
	if fileDirty {
		if err := s.populateFileContentFTS(ctx, tx); err != nil {
			return err
		}
	}

	// Overflow response chunks are a cache keyed to the previous index
	// generation; any successful full index write invalidates them.
	return deleteResponseChunksTx(ctx, tx)
}

// registerRepos upserts every member into the repos table (workspace mode) and
// clears rows from any previous workspace index.
func registerRepos(ctx context.Context, tx *sql.Tx, repos []RepoSpec, workspace bool) (map[string]int64, error) {
	repoByModule := make(map[string]int64)
	if !workspace {
		return repoByModule, nil
	}
	for _, r := range repos {
		id, err := ensureRepoInTx(ctx, tx, r)
		if err != nil {
			return nil, err
		}
		repoByModule[r.ModulePath] = id
	}
	if err := clearWorkspaceRows(ctx, tx); err != nil {
		return nil, err
	}
	return repoByModule, nil
}

// buildRepoIDFor returns a function mapping a file path to its repo_id by the
// longest member directory prefix.
func buildRepoIDFor(repos []RepoSpec, repoByModule map[string]int64) func(string) int64 {
	return func(path string) int64 {
		best := ""
		bestID := int64(0)
		for _, r := range repos {
			if r.Missing {
				continue
			}
			dir := filepath.Clean(r.Dir)
			if strings.HasPrefix(path, dir+string(filepath.Separator)) || path == dir {
				if len(dir) > len(best) {
					best = dir
					bestID = repoByModule[r.ModulePath]
				}
			}
		}
		return bestID
	}
}

// ReplaceRepo incrementally reindexes a single member repo, replacing only the
// rows attributed to that repo. Used for per-repo auto-reindex on staleness.
func (s *Store) ReplaceRepo(result *resolve.Result, files map[string]string, churn map[string]int, modulePath string) error {
	repoID, ok, err := s.RepoIDByModule(modulePath)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("repo %s is not registered in this workspace", modulePath)
	}

	ctx := context.Background()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := deleteRepoRows(ctx, tx, repoID); err != nil {
			return err
		}
		repoByModule := map[string]int64{modulePath: repoID}
		if err := writeRepoRows(ctx, tx, result, repoByModule, churn); err != nil {
			return err
		}
		fileDirty, err := s.writeFiles(ctx, tx, files, func(string) int64 { return repoID }, fileScope{repoID: repoID})
		if err != nil {
			return err
		}
		if err := s.populateFTS(ctx, tx); err != nil {
			return err
		}
		if fileDirty {
			if err := s.populateFileContentFTS(ctx, tx); err != nil {
				return err
			}
		}
		// A per-member re-index also invalidates the overflow cache.
		return deleteResponseChunksTx(ctx, tx)
	})
}

// deleteRepoRows removes every row attributed to a repo, including any
// per-repo contract intelligence derived from it. File rows are NOT deleted
// here: writeFiles now owns stale-file cleanup so unchanged files keep their
// rowid (which keeps file_content_fts consistent without a rebuild).
func deleteRepoRows(ctx context.Context, tx *sql.Tx, repoID int64) error {
	for _, stmt := range []string{
		`DELETE FROM edges WHERE repo_id = ?`,
		`DELETE FROM symbols WHERE repo_id = ?`,
		`DELETE FROM packages WHERE repo_id = ?`,
		`DELETE FROM contracts WHERE repo_id = ?`,
		`DELETE FROM runtime_contracts WHERE repo_id = ?`,
		`DELETE FROM drift WHERE repo_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, repoID); err != nil {
			return err
		}
	}
	return nil
}

// writeRepoRows writes packages, symbols, edges and churn for one repo's
// re-index inside the caller's transaction.
func writeRepoRows(ctx context.Context, tx *sql.Tx, result *resolve.Result, repoByModule map[string]int64, churn map[string]int) error {
	pkgCache, err := writePackages(ctx, tx, result.Packages, repoByModule)
	if err != nil {
		return err
	}
	attrib := newPkgAttribution(pkgCache, repoByModule)
	if err := writeSymbols(ctx, tx, result.Symbols, attrib); err != nil {
		return err
	}
	if err := writeEdges(ctx, tx, result.Edges, attrib); err != nil {
		return err
	}
	if churn != nil {
		if err := applyChurn(ctx, tx, churn); err != nil {
			return err
		}
	}
	return nil
}

// clearWorkspaceRows removes rows attributed to a previous workspace index so a
// full re-index replaces them atomically. File rows are owned by writeFiles,
// which deletes stale paths and keeps unchanged rows' rowids stable.
func clearWorkspaceRows(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`DELETE FROM edges WHERE repo_id IS NOT NULL`,
		`DELETE FROM symbols WHERE repo_id IS NOT NULL`,
		`DELETE FROM packages WHERE repo_id IS NOT NULL`,
		`DELETE FROM contracts WHERE repo_id IS NOT NULL`,
		`DELETE FROM runtime_contracts WHERE repo_id IS NOT NULL`,
		`DELETE FROM drift WHERE repo_id IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// clearSingleRepoRows removes the previous single-repo index (rows with no repo
// attribution, repo_id NULL) so a re-index replaces it atomically. Contracts are
// owned by the contract-analysis write path, not the index write, so they are
// left untouched. File rows are owned by writeFiles, which deletes stale paths
// and keeps unchanged rows' rowids stable.
func clearSingleRepoRows(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`DELETE FROM edges WHERE repo_id IS NULL`,
		`DELETE FROM symbols WHERE repo_id IS NULL`,
		`DELETE FROM packages WHERE repo_id IS NULL`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// ensureRepoInTx inserts the repo if missing and returns its id, refreshing
// dir/missing/indexed_at state.
func ensureRepoInTx(ctx context.Context, tx *sql.Tx, spec RepoSpec) (int64, error) {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO repos (module_path, dir, missing) VALUES (?, ?, ?)`,
		spec.ModulePath, spec.Dir, spec.Missing)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM repos WHERE module_path = ?`, spec.ModulePath).Scan(&id); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE repos SET dir = ?, missing = ? WHERE id = ?`, spec.Dir, spec.Missing, id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func writePackages(ctx context.Context, tx *sql.Tx, packages []parse.PackageInfo, repoByModule map[string]int64) (map[string]int64, error) {
	pkgCache := make(map[string]int64)
	for _, pkg := range packages {
		repoID := repoByModule[pkg.ModulePath]
		var id int64
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO packages (path, name, dir, is_test, repo_id) VALUES (?, ?, ?, ?, ?)`,
			pkg.ImportPath, pkg.Name, pkg.Dir, pkg.IsTest, nullableRepo(repoID))
		if err != nil {
			return nil, err
		}

		err = tx.QueryRowContext(ctx, `SELECT id FROM packages WHERE path = ?`, pkg.ImportPath).Scan(&id)
		if err != nil {
			return nil, err
		}
		pkgCache[pkg.ImportPath] = id
	}
	return pkgCache, nil
}

// nullableRepo returns a sql.NullInt64 usable as a nullable repo_id column.
func nullableRepo(repoID int64) any {
	if repoID == 0 {
		return nil
	}
	return repoID
}

// pkgAttribution resolves the owning package and repo for symbol and edge
// writes in constant time per row. It is built once per write from the parsed
// package set, replacing the former per-row linear scans over all package paths
// (O(S × P) indexing cost).
type pkgAttribution struct {
	pkgCache map[string]int64 // import path -> package id
	repo     map[string]int64 // import path -> repo id
}

// newPkgAttribution precomputes repo attribution for every indexed package from
// the module map, so later per-symbol/edge lookups are plain map hits.
func newPkgAttribution(pkgCache, repoByModule map[string]int64) *pkgAttribution {
	a := &pkgAttribution{
		pkgCache: pkgCache,
		repo:     make(map[string]int64, len(pkgCache)),
	}
	for p := range pkgCache {
		a.repo[p] = repoForPackage(p, repoByModule)
	}
	return a
}

// pkgFor returns the package id and repo id owning a qualified name. The
// qualified name itself is tried first (package-level symbols whose name equals
// the package path); otherwise the longest package-path prefix is probed via
// dot-boundary map hits — O(len(ref)) lookups, never a scan over all packages.
func (a *pkgAttribution) pkgFor(ref string) (pkgID, repoID int64) {
	if id, ok := a.pkgCache[ref]; ok {
		return id, a.repo[ref]
	}
	pkgPath := longestPackagePrefix(ref, a.pkgCache)
	if pkgPath == "" {
		return 0, 0
	}
	return a.pkgCache[pkgPath], a.repo[pkgPath]
}

// longestPackagePrefix returns the longest indexed package path that owns ref,
// where ref is a qualified symbol name built from a package path. The probe
// only considers prefixes ending at a '.' boundary so short packages sharing a
// prefix (a vs a.b) never hijack longer ones.
func longestPackagePrefix(ref string, paths map[string]int64) string {
	best := ""
	for i := 0; i < len(ref); i++ {
		if ref[i] != '.' {
			continue
		}
		prefix := ref[:i]
		if _, ok := paths[prefix]; ok && len(prefix) > len(best) {
			best = prefix
		}
	}
	return best
}

func writeSymbols(ctx context.Context, tx *sql.Tx, symbols []resolve.ResolvedSymbol, attrib *pkgAttribution) error {
	symInsert := `INSERT OR IGNORE INTO symbols (qualified_name, package_id, name, kind, receiver, signature, doc, pos_file, pos_line, exported, is_test, complexity, repo_id, fields_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	for _, sym := range symbols {
		pkgID, repoID := attrib.pkgFor(sym.Symbol.QualifiedName)
		if pkgID == 0 {
			continue
		}

		var fieldsJSON any
		if len(sym.Symbol.Fields) > 0 {
			data, err := json.Marshal(sym.Symbol.Fields)
			if err != nil {
				return fmt.Errorf("marshaling fields for %s: %w", sym.Symbol.QualifiedName, err)
			}
			fieldsJSON = string(data)
		}

		_, err := tx.ExecContext(ctx, symInsert,
			sym.Symbol.QualifiedName,
			pkgID,
			sym.Symbol.Name,
			sym.Symbol.Kind,
			sym.Symbol.Receiver,
			sym.Symbol.Signature,
			sym.Symbol.Doc,
			sym.Symbol.Pos.File,
			sym.Symbol.Pos.Line,
			sym.Symbol.Exported,
			sym.Symbol.IsTest,
			sym.Symbol.Complexity,
			nullableRepo(repoID),
			fieldsJSON,
		)
		if err != nil {
			return fmt.Errorf("inserting symbol %s: %w", sym.Symbol.QualifiedName, err)
		}
	}
	return nil
}

func writeEdges(ctx context.Context, tx *sql.Tx, edges []resolve.ResolvedEdge, attrib *pkgAttribution) error {
	// One row per (repo_id, from_ref, to_ref, edge_type): re-indexing the same
	// pair converges instead of inserting duplicates. On conflict the existing
	// sites array is merged with the incoming one, deduping identical sites and
	// keeping the first (primary) position first.
	edgeUpsert := `INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, sites, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(COALESCE(repo_id, 0), from_ref, to_ref, edge_type)
DO UPDATE SET
	sites = (SELECT json_group_array(json(value)) FROM (
		SELECT value FROM json_each(edges.sites)
		UNION
		SELECT value FROM json_each(excluded.sites)
	))`

	for _, edge := range edges {
		_, repoID := attrib.pkgFor(edge.Edge.FromRef)

		pos := edge.Edge.Pos
		sitesJSON, err := jsonMarshal([]Site{{File: pos.File, Line: pos.Line}})
		if err != nil {
			return fmt.Errorf("marshaling sites for %s -> %s: %w", edge.Edge.FromRef, edge.Edge.ToRef, err)
		}

		_, err = tx.ExecContext(ctx, edgeUpsert,
			edge.Edge.FromRef,
			edge.Edge.ToRef,
			edge.Edge.EdgeType,
			pos.File,
			pos.Line,
			sitesJSON,
			nullableRepo(repoID),
		)
		if err != nil {
			return fmt.Errorf("upserting edge %s -> %s: %w", edge.Edge.FromRef, edge.Edge.ToRef, err)
		}
	}
	return nil
}

// repoForPackage maps a package import path to its repo_id using the
// module-attribution map built at write time. Returns 0 (NULL) when the package
// has no repo (single-repo mode).
func repoForPackage(pkgPath string, repoByModule map[string]int64) int64 {
	if len(repoByModule) == 0 {
		return 0
	}
	for module, id := range repoByModule {
		if pkgPath == module || strings.HasPrefix(pkgPath, module+"/") {
			return id
		}
	}
	return 0
}

func (s *Store) populateFTS(ctx context.Context, tx *sql.Tx) error {
	// symbols_fts is an external-content table; 'rebuild' re-indexes it from the
	// symbols table and is safe on both fresh and existing databases. Running it
	// inside the index transaction keeps symbols and the FTS index consistent.
	_, err := tx.ExecContext(ctx, `INSERT INTO symbols_fts(symbols_fts) VALUES('rebuild')`)
	return err
}

// likeEscape is the escape character used in every LIKE predicate that receives
// user input. SQLite's LIKE has no default escape, so an explicit ESCAPE clause
// is required for user-supplied wildcards to be matched literally.
const likeEscape = `\`

// escapeLike escapes the LIKE wildcard characters plus the escape character
// itself so that user input is matched literally. Escape order matters: the
// escape character must be escaped first, otherwise a user-supplied `\` would
// escape the escaping we add for `%`/`_`.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// likePattern wraps a user pattern in the %...% contains form with LIKE special
// characters escaped, matching the escaped value literally.
func likePattern(user string) string {
	return "%" + escapeLike(user) + "%"
}

// likeMatch builds a LIKE predicate with an explicit escape clause for a given
// column. The argument is produced by likePattern/escapeLike.
func likeMatch(column string) string {
	return column + " LIKE ? ESCAPE '" + likeEscape + "'"
}

// sanitizeFTSQuery converts a user pattern into an FTS5 expression in which
// every term is a phrase (embedded `"` doubled), joined with implicit AND.
// FTS5 operator characters in the user input are inert literals inside the
// phrases. A term that is punctuation-only (e.g. `-`) is dropped rather than
// quoted, since an empty quoted string is a MATCH error.
func sanitizeFTSQuery(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}

	terms := strings.Fields(pattern)
	var quoted []string
	for _, t := range terms {
		t = strings.ReplaceAll(t, `"`, `""`)
		if isPunctuationOnly(t) {
			continue
		}
		quoted = append(quoted, `"`+t+`"`)
	}
	if len(quoted) == 0 {
		return ""
	}
	return strings.Join(quoted, " ")
}

// buildSymbolFTSQuery converts a user pattern into an FTS5 expression that
// matches whole identifiers AND partial (prefix) identifiers. For each term it
// emits a parenthesized OR group: the exact phrase (matching the whole token
// anywhere in the indexed columns) OR a name-column-scoped prefix (matching
// symbol names that begin with the term). Groups are AND-joined so multi-term
// patterns keep their current whole-token AND semantics while gaining prefix
// support. The name-column scope keeps a prefix from over-matching unrelated
// columns (e.g. "pkg:F" must not match "Foo" via the qualified_name column).
// This mirrors bleve's PrefixQuery: an identifier search on a partial name
// ("GetSym") returns every symbol whose name starts with it ("GetSymbol",
// "GetSymbolBody") instead of silently matching nothing.
func buildSymbolFTSQuery(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}

	terms := strings.Fields(pattern)
	var groups []string
	for _, t := range terms {
		t = strings.ReplaceAll(t, `"`, `""`)
		if isPunctuationOnly(t) {
			continue
		}
		groups = append(groups, `("`+t+`" OR {name} : "`+t+`"*)`)
	}
	if len(groups) == 0 {
		return ""
	}
	return strings.Join(groups, " AND ")
}

// isPunctuationOnly reports whether s consists entirely of non-letter,
// non-digit characters. Such terms are dropped by sanitizeFTSQuery because
// quoting them yields an empty phrase, which is a MATCH error.
func isPunctuationOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// ftsNeedsFullScan reports whether the FTS5 trigram tokenizer cannot serve the
// pattern. The tokenizer only indexes terms of at least three characters, and
// any shorter token in a MATCH query silently matches nothing. Because the
// trigram tokenizer splits on non-alphanumerics just like unicode61, a short
// token can hide inside a longer term (e.g. "pkg:F"), so every alphanumeric
// run in every kept term must reach three characters.
func ftsNeedsFullScan(pattern string) bool {
	for term := range strings.FieldsSeq(pattern) {
		if isPunctuationOnly(term) {
			continue
		}
		run := 0
		for _, r := range term {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				run++
				continue
			}
			if run > 0 && run < 3 {
				return true
			}
			run = 0
		}
		if run > 0 && run < 3 {
			return true
		}
	}
	return false
}

type Package struct {
	Path     string
	Name     string
	Dir      string
	Repo     string `json:"repo,omitempty"`
	SymCount int
	IsTest   bool
}

// SymbolField mirrors extract.StructField: one struct field's Go name,
// effective wire name and type. Persisted as JSON on the symbols row.
type SymbolField struct {
	GoName   string `json:"go_name,omitempty"`
	WireName string `json:"wire_name,omitempty"`
	GoType   string `json:"go_type,omitempty"`
}

type Symbol struct {
	QualifiedName string
	PackagePath   string
	Name          string
	Kind          string
	Receiver      string
	Signature     string
	Doc           string
	PosFile       string
	Repo          string `json:"repo,omitempty"`
	Fields        []SymbolField
	PosLine       int
	Complexity    int
	ChurnCount    int
	Importance    float64
	Exported      bool
	IsTest        bool
}

type FileMatch struct {
	FilePath      string
	Line          string
	ContextBefore string
	ContextAfter  string
	LineNumber    int
}

// Site is one occurrence location of an edge: the repo-relative file path and
// line at which the edge was observed during indexing.
type Site struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

type Edge struct {
	FromRef   string
	ToRef     string
	EdgeType  string
	PosFile   string
	Repo      string `json:"repo,omitempty"`
	Sites     []Site
	PosLine   int
	SiteCount int
}

// ContractDirection describes the wire relationship between producer and consumer.
type ContractDirection string

const (
	ContractDirectionProducer ContractDirection = "producer"
	ContractDirectionConsumer ContractDirection = "consumer"
	ContractDirectionShared   ContractDirection = "shared"
)

// ContractSeverity classifies structural divergence between shape-matched types.
type ContractSeverity string

const (
	ContractSeverityCompatible ContractSeverity = "compatible"
	ContractSeverityBreaking   ContractSeverity = "breaking"
	ContractSeverityUnknown    ContractSeverity = "unknown"
)

// Contract represents a wire-contract edge linking a producer/consumer symbol
// in one repo to its counterpart in another repo, matched by shared message type
// constant and/or CBOR-name-aware structural shape.
type Contract struct {
	FromRef    string            `json:"from_ref"`
	ToRef      string            `json:"to_ref"`
	Direction  ContractDirection `json:"direction"`
	Severity   ContractSeverity  `json:"severity"`
	Evidence   string            `json:"evidence"`
	IndexedAt  string            `json:"indexed_at"`
	Repo       string            `json:"repo,omitempty"`
	ID         int64             `json:"id"`
	Confidence float64           `json:"confidence"`
	Suggested  bool              `json:"suggested"`
}

// RuntimeContractKind classifies a runtime contract entity.
type RuntimeContractKind string

const (
	RuntimeContractRedis     RuntimeContractKind = "redis"
	RuntimeContractJetStream RuntimeContractKind = "jetstream"
	RuntimeContractWSType    RuntimeContractKind = "ws_type"
)

// RuntimeContract represents a first-class runtime contract entity (Redis key
// pattern, JetStream stream/subject, WS type string) with edges to its
// producer/consumer sites.
type RuntimeContract struct {
	Kind      RuntimeContractKind `json:"kind"`
	Pattern   string              `json:"pattern"`
	FromRef   string              `json:"from_ref"`
	ToRef     string              `json:"to_ref"`
	Direction ContractDirection   `json:"direction"`
	Evidence  string              `json:"evidence"`
	IndexedAt string              `json:"indexed_at"`
	Repo      string              `json:"repo,omitempty"`
	ID        int64               `json:"id"`
}

// DriftSeverity classifies divergence between two shape-matched types.
type DriftSeverity string

const (
	DriftSeverityCompatible DriftSeverity = "compatible"
	DriftSeverityBreaking   DriftSeverity = "breaking"
	DriftSeverityUnknown    DriftSeverity = "unknown"
)

// DriftField represents a single field-level divergence.
type DriftField struct {
	WireName string `json:"wire_name"`
	TypeA    string `json:"type_a"`
	TypeB    string `json:"type_b"`
	RepoA    string `json:"repo_a"`
	RepoB    string `json:"repo_b"`
	Status   string `json:"status"` // "removed", "renamed", "type_changed", "added"
}

// DriftReport represents structural divergence between shape-matched types.
type DriftReport struct {
	FromRef   string        `json:"from_ref"`
	ToRef     string        `json:"to_ref"`
	Severity  DriftSeverity `json:"severity"`
	Evidence  string        `json:"evidence,omitempty"`
	IndexedAt string        `json:"indexed_at"`
	Repo      string        `json:"repo,omitempty"`
	Fields    []DriftField  `json:"fields"`
	ID        int64         `json:"id"`
}

// ContractFilter defines filter criteria for contract queries.
type ContractFilter struct {
	Suggested     *bool
	Repo          string
	Direction     string
	Severity      string
	MinConfidence float64
}

// Shared SQL fragments for the contract query builders.
const (
	sqlAllRows     = "1=1"
	sqlAndRepoPath = " AND r.module_path = ?"
)

// RepoSpec describes one workspace member at index time.
type RepoSpec struct {
	ModulePath string
	Dir        string
	Missing    bool
}

// Repo is the persisted workspace member record.
type Repo struct {
	ModulePath string
	Dir        string
	IndexedAt  string
	ID         int64
	Missing    bool
}

const (
	repoStateIndexed = "indexed"
	repoStateStale   = "stale"
	repoStateMissing = "missing"
)

const symbolColumns = `s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance, s.repo_id, r.module_path, s.fields_json`

const symbolFrom = `FROM symbols s
		JOIN packages p ON s.package_id = p.id
		LEFT JOIN repos r ON r.id = s.repo_id`

func scanSymbol(rows *sql.Rows) (Symbol, error) {
	var sym Symbol
	var repoID sql.NullInt64
	var repo sql.NullString
	var fieldsJSON sql.NullString
	err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance, &repoID, &repo, &fieldsJSON)
	sym.Repo = repo.String
	if err != nil {
		return sym, err
	}
	if fieldsJSON.Valid && fieldsJSON.String != "" {
		if err := json.Unmarshal([]byte(fieldsJSON.String), &sym.Fields); err != nil {
			return sym, err
		}
	}
	return sym, nil
}

func scanSymbols(rows *sql.Rows) ([]Symbol, error) {
	defer func() { _ = rows.Close() }()
	var syms []Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) ListPackages() ([]Package, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT p.path, p.name, p.dir, p.is_test, COUNT(s.id), r.module_path
		FROM packages p
		LEFT JOIN symbols s ON s.package_id = p.id
		LEFT JOIN repos r ON r.id = p.repo_id
		GROUP BY p.id
		ORDER BY p.path
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var pkgs []Package
	for rows.Next() {
		var p Package
		var repo sql.NullString
		if err := rows.Scan(&p.Path, &p.Name, &p.Dir, &p.IsTest, &p.SymCount, &repo); err != nil {
			return nil, err
		}
		p.Repo = repo.String
		pkgs = append(pkgs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pkgs, nil
}

func (s *Store) SymbolsByPackage(pkgPath string, includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom + `
		WHERE p.path = ?
	`
	args := []any{pkgPath}
	if !includeTests {
		query += andIsTestFalse
	}
	query += orderByQualifiedName

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) SymbolByName(qualifiedName string) (*Symbol, error) {
	qualifiedName = NormalizeQualifiedName(qualifiedName)
	var sym Symbol
	var repoID sql.NullInt64
	var repo sql.NullString
	var fieldsJSON sql.NullString
	err := s.db.QueryRowContext(context.Background(), `SELECT `+symbolColumns+`
		`+symbolFrom+`
		WHERE s.qualified_name = ?
	`, qualifiedName).Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance, &repoID, &repo, &fieldsJSON)
	sym.Repo = repo.String
	if err != nil {
		return nil, err
	}
	if fieldsJSON.Valid && fieldsJSON.String != "" {
		if err := json.Unmarshal([]byte(fieldsJSON.String), &sym.Fields); err != nil {
			return nil, err
		}
	}
	return &sym, nil
}

func (s *Store) EdgesFrom(ref string) ([]Edge, error) {
	return s.queryEdges("from_ref = ?", NormalizeQualifiedName(ref))
}

func (s *Store) EdgesTo(ref string) ([]Edge, error) {
	return s.queryEdges("to_ref = ?", NormalizeQualifiedName(ref))
}

func (s *Store) EdgesByType(edgeType string) ([]Edge, error) {
	return s.queryEdges("edge_type = ?", edgeType)
}

// edgesInBatchSize is the chunk size for IN (...) lists in EdgesForNodes and
// other multi-ref queries, kept well under SQLite's default variable limit
// (999) so a bounded collection of refs never overflows the binding capacity.
const edgesInBatchSize = 500

// EdgesForNodes returns every edge whose from_ref (outgoing=true) or to_ref
// (outgoing=false) is one of refs, optionally restricted to the given edge
// types. The IN list is chunked into edgesInBatchSize entries, so the number of
// queries is O(len(refs)/500) regardless of graph fanout, which keeps deep
// traversals at O(depth) round-trips instead of one query per visited node.
func (s *Store) EdgesForNodes(refs, edgeTypes []string, outgoing bool) ([]Edge, error) {
	return s.edgesForNodes(context.Background(), refs, edgeTypes, outgoing)
}

func (s *Store) edgesForNodes(ctx context.Context, refs, edgeTypes []string, outgoing bool) ([]Edge, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	col := "from_ref"
	if !outgoing {
		col = "to_ref"
	}

	typeClause := ""
	var typeArgs []any
	if len(edgeTypes) > 0 {
		typeClause = " AND e.edge_type IN (" + inPlaceholders(len(edgeTypes)) + ")"
		for _, t := range edgeTypes {
			typeArgs = append(typeArgs, t)
		}
	}

	var out []Edge
	for _, batch := range chunks(refs, edgesInBatchSize) {
		where := "e." + col + " IN (" + inPlaceholders(len(batch)) + ")" + typeClause
		args := make([]any, 0, len(batch)+len(typeArgs))
		for _, r := range batch {
			args = append(args, NormalizeQualifiedName(r))
		}
		args = append(args, typeArgs...)

		rows, err := s.db.QueryContext(ctx,
			"SELECT e.from_ref, e.to_ref, e.edge_type, e.pos_file, e.pos_line, e.sites, r.module_path FROM edges e LEFT JOIN repos r ON r.id = e.repo_id WHERE "+where,
			args...)
		if err != nil {
			return nil, err
		}
		batchEdges, err := scanEdges(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, batchEdges...)
	}
	return out, nil
}

// inPlaceholders returns a comma-separated list of n SQL placeholders.
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// chunks splits a slice into non-empty batches of at most size elements.
func chunks[T any](items []T, size int) [][]T {
	if size <= 0 {
		return [][]T{items}
	}
	var out [][]T
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		out = append(out, items[start:end])
	}
	return out
}

// SymbolEdges returns symbol-to-symbol edges of the given edge types
// (calls, references, satisfies, embeds, ...), each attributed with its repo.
// Edges whose from- or to-endpoint is not an indexed symbol are excluded.
func (s *Store) SymbolEdges(edgeTypes []string) ([]Edge, error) {
	if len(edgeTypes) == 0 {
		return []Edge{}, nil
	}
	placeholders := strings.Repeat("?,", len(edgeTypes))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(edgeTypes))
	for i, t := range edgeTypes {
		args[i] = t
	}
	where := "e.edge_type IN (" + placeholders + ") AND EXISTS (SELECT 1 FROM symbols f WHERE f.qualified_name = e.from_ref) AND EXISTS (SELECT 1 FROM symbols t WHERE t.qualified_name = e.to_ref)"
	return s.queryEdges(where, args...)
}

func (s *Store) AllEdges() ([]Edge, error) {
	return s.queryEdges(sqlAllRows)
}

// CountEdges returns the total number of edges without materializing them.
func (s *Store) CountEdges() (int, error) {
	var n int
	err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM edges`).Scan(&n)
	return n, err
}

// ImportCounts returns, for every source package path, how many import edges
// leave it. Built with one grouped query so callers such as Overview avoid one
// edge query per package.
func (s *Store) ImportCounts() (map[string]int, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT from_ref, COUNT(*) FROM edges WHERE edge_type = ? GROUP BY from_ref`, edgeTypeImports)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int)
	for rows.Next() {
		var from string
		var n int
		if err := rows.Scan(&from, &n); err != nil {
			return nil, err
		}
		counts[from] = n
	}
	return counts, rows.Err()
}

// WriteContracts replaces all contract intelligence data for a repo atomically.
// WriteContractsAll replaces all contract intelligence data globally in one
// transaction. Contract analysis is cross-repo, so a single run owns every row;
// each row is attributed to the repo of its from-side reference. The analysis
// timestamp is recorded so staleness checks can compare it to per-repo indexes.
func (s *Store) WriteContractsAll(contracts []Contract, runtimeContracts []RuntimeContract, drifts []DriftReport, indexedAt string) error {
	ctx := context.Background()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DELETE FROM contracts`,
			`DELETE FROM runtime_contracts`,
			`DELETE FROM drift`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}

		// Resolve repo attribution once from the package table into memory;
		// per-row attribution is then a plain map probe instead of a SELECT per
		// contract row.
		repoByPath, err := readPackageRepoMap(ctx, tx)
		if err != nil {
			return err
		}

		if err := insertContracts(ctx, tx, contracts, indexedAt, repoByPath); err != nil {
			return err
		}
		if err := insertRuntimeContracts(ctx, tx, runtimeContracts, indexedAt, repoByPath); err != nil {
			return err
		}
		if err := insertDriftReports(ctx, tx, drifts, indexedAt, repoByPath); err != nil {
			return err
		}

		// Record the analysis time at nanosecond precision so staleness comparisons
		// against per-repo index times (same precision) are correct; parsing the
		// caller's RFC3339 string would truncate to whole seconds and make every
		// repo look perpetually stale. Written in the same transaction as the rows
		// so a failure leaves neither, not a half-applied analysis.
		if err := s.maybeFail("contract.meta", ""); err != nil {
			return err
		}
		return s.setContractAnalysisTimeInTx(ctx, tx, time.Now().UTC())
	})
}

// setContractAnalysisTimeInTx records the global contract-analysis time inside
// the caller's transaction.
func (s *Store) setContractAnalysisTimeInTx(ctx context.Context, tx *sql.Tx, t time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('contract_indexed_at', ?)`,
		t.Format(time.RFC3339Nano))
	return err
}

func insertContracts(ctx context.Context, tx *sql.Tx, contracts []Contract, indexedAt string, repoByPath map[string]int64) error {
	for _, c := range contracts {
		repoID := repoIDForRefMapped(c.FromRef, repoByPath)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO contracts (from_ref, to_ref, direction, confidence, severity, suggested, evidence, indexed_at, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.FromRef, c.ToRef, string(c.Direction), c.Confidence, string(c.Severity), c.Suggested, c.Evidence, indexedAt, nullableRepo(repoID)); err != nil {
			return err
		}
	}
	return nil
}

func insertRuntimeContracts(ctx context.Context, tx *sql.Tx, runtimeContracts []RuntimeContract, indexedAt string, repoByPath map[string]int64) error {
	for _, rc := range runtimeContracts {
		repoID := repoIDForRefMapped(rc.FromRef, repoByPath)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runtime_contracts (kind, pattern, from_ref, to_ref, direction, evidence, indexed_at, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			string(rc.Kind), rc.Pattern, rc.FromRef, rc.ToRef, string(rc.Direction), rc.Evidence, indexedAt, nullableRepo(repoID)); err != nil {
			return err
		}
	}
	return nil
}

func insertDriftReports(ctx context.Context, tx *sql.Tx, drifts []DriftReport, indexedAt string, repoByPath map[string]int64) error {
	for _, d := range drifts {
		fieldsJSON, err := jsonMarshal(d.Fields)
		if err != nil {
			return err
		}
		repoID := repoIDForRefMapped(d.FromRef, repoByPath)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO drift (from_ref, to_ref, severity, fields_json, indexed_at, repo_id) VALUES (?, ?, ?, ?, ?, ?)`,
			d.FromRef, d.ToRef, string(d.Severity), fieldsJSON, indexedAt, nullableRepo(repoID)); err != nil {
			return err
		}
	}
	return nil
}

// readPackageRepoMap loads every indexed package's repo attribution into a map
// so contract rows can resolve their repo in memory instead of one SELECT per
// row.
func readPackageRepoMap(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path, repo_id FROM packages`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	m := make(map[string]int64)
	for rows.Next() {
		var path string
		var repoID sql.NullInt64
		if err := rows.Scan(&path, &repoID); err != nil {
			return nil, err
		}
		if repoID.Valid {
			m[path] = repoID.Int64
		}
	}
	return m, rows.Err()
}

// repoIDForRefMapped resolves the repo owning a qualified reference from an
// in-memory package-path map, matching the SQL longest-package-match semantics
// (exact path, else longest dotted-prefix) without a per-row query.
func repoIDForRefMapped(ref string, repoByPath map[string]int64) int64 {
	if id, ok := repoByPath[ref]; ok {
		return id
	}
	path := longestPackagePrefix(ref, repoByPath)
	if path == "" {
		return 0
	}
	return repoByPath[path]
}

// QueryContracts returns contracts matching the given filter.
func (s *Store) QueryContracts(filter ContractFilter) ([]Contract, error) {
	where := sqlAllRows
	var args []any

	if filter.Repo != "" {
		where += sqlAndRepoPath
		args = append(args, filter.Repo)
	}
	if filter.Direction != "" {
		where += " AND c.direction = ?"
		args = append(args, filter.Direction)
	}
	if filter.Severity != "" {
		where += " AND c.severity = ?"
		args = append(args, filter.Severity)
	}
	if filter.MinConfidence > 0 {
		where += " AND c.confidence >= ?"
		args = append(args, filter.MinConfidence)
	}
	if filter.Suggested != nil {
		where += " AND c.suggested = ?"
		args = append(args, *filter.Suggested)
	}

	query := `SELECT c.id, c.from_ref, c.to_ref, c.direction, c.confidence, c.severity, c.suggested, c.evidence, c.indexed_at, r.module_path
		FROM contracts c
		LEFT JOIN repos r ON r.id = c.repo_id
		WHERE ` + where + `
		AND NOT EXISTS (
			SELECT 1 FROM contract_suppressions sp
			WHERE (sp.from_ref = c.from_ref AND sp.to_ref = c.to_ref)
			   OR (sp.from_ref = c.to_ref AND sp.to_ref = c.from_ref)
		)
		ORDER BY c.confidence DESC, c.from_ref`

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var contracts []Contract
	for rows.Next() {
		var c Contract
		var repo sql.NullString
		if err := rows.Scan(&c.ID, &c.FromRef, &c.ToRef, &c.Direction, &c.Confidence, &c.Severity, &c.Suggested, &c.Evidence, &c.IndexedAt, &repo); err != nil {
			return nil, err
		}
		c.Repo = repo.String
		contracts = append(contracts, c)
	}
	return contracts, rows.Err()
}

// QueryRuntimeContracts returns runtime contracts matching the given kind and repo.
func (s *Store) QueryRuntimeContracts(kind RuntimeContractKind, repo string) ([]RuntimeContract, error) {
	where := sqlAllRows
	var args []any

	if kind != "" {
		where += " AND rc.kind = ?"
		args = append(args, string(kind))
	}
	if repo != "" {
		where += sqlAndRepoPath
		args = append(args, repo)
	}

	query := `SELECT rc.id, rc.kind, rc.pattern, rc.from_ref, rc.to_ref, rc.direction, rc.evidence, rc.indexed_at, r.module_path
		FROM runtime_contracts rc
		LEFT JOIN repos r ON r.id = rc.repo_id
		WHERE ` + where + `
		ORDER BY rc.kind, rc.pattern`

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []RuntimeContract
	for rows.Next() {
		var rc RuntimeContract
		var repo sql.NullString
		if err := rows.Scan(&rc.ID, &rc.Kind, &rc.Pattern, &rc.FromRef, &rc.ToRef, &rc.Direction, &rc.Evidence, &rc.IndexedAt, &repo); err != nil {
			return nil, err
		}
		rc.Repo = repo.String
		results = append(results, rc)
	}
	return results, rows.Err()
}

// QueryDrift returns drift reports matching the given severity and repo.
func (s *Store) QueryDrift(severity DriftSeverity, repo string) ([]DriftReport, error) {
	where := sqlAllRows
	var args []any

	if severity != "" {
		where += " AND d.severity = ?"
		args = append(args, string(severity))
	}
	if repo != "" {
		where += sqlAndRepoPath
		args = append(args, repo)
	}

	query := `SELECT d.id, d.from_ref, d.to_ref, d.severity, d.fields_json, d.indexed_at, r.module_path
		FROM drift d
		LEFT JOIN repos r ON r.id = d.repo_id
		WHERE ` + where + `
		AND NOT EXISTS (
			SELECT 1 FROM contract_suppressions sp
			WHERE (sp.from_ref = d.from_ref AND sp.to_ref = d.to_ref)
			   OR (sp.from_ref = d.to_ref AND sp.to_ref = d.from_ref)
		)
		ORDER BY d.severity, d.from_ref`

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []DriftReport
	for rows.Next() {
		var d DriftReport
		var repo sql.NullString
		var fieldsJSON string
		if err := rows.Scan(&d.ID, &d.FromRef, &d.ToRef, &d.Severity, &fieldsJSON, &d.IndexedAt, &repo); err != nil {
			return nil, err
		}
		d.Repo = repo.String
		if fieldsJSON != "" {
			if err := jsonUnmarshal(fieldsJSON, &d.Fields); err != nil {
				return nil, err
			}
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// SuppressContracts durably records that a contract pair must never appear in
// contracts, drift, or blast radius results. Suppression survives reanalysis
// because queries filter against the suppressions table.
func (s *Store) SuppressContracts(fromRef, toRef string) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO contract_suppressions (from_ref, to_ref) VALUES (?, ?)`,
		fromRef, toRef)
	return err
}

// ContractSuppressions returns every recorded from→to suppression pair.
func (s *Store) ContractSuppressions() ([][2]string, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT from_ref, to_ref FROM contract_suppressions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var pairs [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		pairs = append(pairs, [2]string{a, b})
	}
	return pairs, rows.Err()
}

// IsContractStale reports whether the global contract analysis is older than
// the repo's last symbol index. Contract analysis is cross-repo, so freshness
// is tracked globally; any member whose index is newer than the analysis makes
// the whole analysis stale. A repo that has never been analyzed is stale.
func (s *Store) IsContractStale(modulePath string) (bool, error) {
	repoIndexedAt, ok, err := s.RepoIndexedAt(modulePath)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}

	contractTime, ok, err := s.ContractAnalysisTime()
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}

	return contractTime.Before(repoIndexedAt), nil
}

// ContractAnalysisTime returns the timestamp of the last contract analysis run,
// or ok=false when contract analysis has never run.
func (s *Store) ContractAnalysisTime() (time.Time, bool, error) {
	var val string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = 'contract_indexed_at'`).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) || val == "" {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, val)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// SetContractAnalysisTime records when the global contract analysis ran.
func (s *Store) SetContractAnalysisTime(t time.Time) error {
	ctx := context.Background()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		return s.setContractAnalysisTimeInTx(ctx, tx, t)
	})
}

// MissingRepoContracts returns contracts that reference a repo that is not indexed.
func (s *Store) MissingRepoContracts() ([]string, error) {
	repos, err := s.ListRepos()
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, repo := range repos {
		if repo.Missing {
			missing = append(missing, repo.ModulePath)
		}
	}
	return missing, nil
}

func (s *Store) queryEdges(where string, args ...any) ([]Edge, error) {
	rows, err := s.db.QueryContext(context.Background(), "SELECT e.from_ref, e.to_ref, e.edge_type, e.pos_file, e.pos_line, e.sites, r.module_path FROM edges e LEFT JOIN repos r ON r.id = e.repo_id WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	return scanEdges(rows)
}

func scanEdges(rows *sql.Rows) ([]Edge, error) {
	defer func() { _ = rows.Close() }()

	var edges []Edge
	for rows.Next() {
		var e Edge
		var repo sql.NullString
		var sitesJSON sql.NullString
		if err := rows.Scan(&e.FromRef, &e.ToRef, &e.EdgeType, &e.PosFile, &e.PosLine, &sitesJSON, &repo); err != nil {
			return nil, err
		}
		e.Repo = repo.String
		if sitesJSON.Valid && sitesJSON.String != "" {
			if err := jsonUnmarshal(sitesJSON.String, &e.Sites); err != nil {
				return nil, err
			}
		}
		e.SiteCount = len(e.Sites)
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return edges, nil
}

func (s *Store) SearchSymbolsByFile(filePattern, kind string, exported *bool, includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom + `
		WHERE ` +
		likeMatch("s.pos_file") + `
	`
	args := []any{likePattern(filePattern)}

	if kind != "" {
		query += " AND s.kind = ?"
		args = append(args, kind)
	}
	if exported != nil {
		query += " AND s.exported = ?"
		args = append(args, *exported)
	}
	if !includeTests {
		query += andIsTestFalse
	}
	query += orderByQualifiedName

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) SearchSymbols(pattern, kind string, exported *bool, pkgPath string, includeTests bool) ([]Symbol, error) {
	ftsQuery := buildSymbolFTSQuery(pattern)
	if ftsQuery == "" {
		return nil, nil
	}

	query := `SELECT ` + symbolColumns + `
		FROM symbols_fts fts
		JOIN symbols s ON s.id = fts.rowid
		JOIN packages p ON s.package_id = p.id
		LEFT JOIN repos r ON r.id = s.repo_id
		WHERE symbols_fts MATCH ?
	`
	args := []any{ftsQuery}

	if kind != "" {
		query += " AND s.kind = ?"
		args = append(args, kind)
	}
	if exported != nil {
		query += " AND s.exported = ?"
		args = append(args, *exported)
	}
	if pkgPath != "" {
		query += " AND p.path = ?"
		args = append(args, pkgPath)
	}
	if !includeTests {
		query += andIsTestFalse
	}
	query += orderByQualifiedName

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) SearchByQualifiedNamePrefix(prefix string, includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom + `
		WHERE ` +
		likeMatch("s.qualified_name") + `
	`
	args := []any{escapeLike(prefix) + "%"}

	if !includeTests {
		query += andIsTestFalse
	}
	query += orderByQualifiedName

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// BuildImportAdjacency groups import edges into an adjacency map keyed by the
// source package path. Transitive-import walks then visit each edge once via
// the map instead of rescanning the full edge set for every visited node
// (O(V·E) -> O(V+E) per walk).
const edgeTypeImports = "imports"

func BuildImportAdjacency(edges []Edge) map[string][]Edge {
	adj := make(map[string][]Edge)
	for _, e := range edges {
		if e.EdgeType != edgeTypeImports {
			continue
		}
		adj[e.FromRef] = append(adj[e.FromRef], e)
	}
	return adj
}

// ImportFrontierBFS walks an import adjacency map breadth-first from start,
// appending every edge whose next() returns a non-empty neighbor. next decides
// both the neighbor to enqueue and whether an edge belongs in the result: an
// edge is skipped entirely when next returns "". The visited set is shared with
// next (as the second argument) so callers can dedupe by resolved targets. The
// frontier replaces the per-node full-edge scan, bounding each walk to O(V+E).
func ImportFrontierBFS(adj map[string][]Edge, start string, next func(e Edge, visited map[string]bool) string) []Edge {
	if _, ok := adj[start]; !ok {
		return nil
	}
	var out []Edge
	visited := map[string]bool{start: true}
	queue := []string{start}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, e := range adj[current] {
			target := next(e, visited)
			if target == "" {
				continue
			}
			out = append(out, e)
			if !visited[target] {
				visited[target] = true
				queue = append(queue, target)
			}
		}
	}
	return out
}

func (s *Store) TransitiveImports(pkgPath string) ([]Edge, error) {
	edges, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, err
	}
	adj := BuildImportAdjacency(edges)
	return ImportFrontierBFS(adj, pkgPath, func(e Edge, _ map[string]bool) string {
		return e.ToRef
	}), nil
}

func (s *Store) SearchByType(typeName string, includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom + `
		WHERE s.signature LIKE '%' || ? || ' %' ESCAPE '\'
		   OR s.signature LIKE '%' || ? || ')%' ESCAPE '\'
		   OR s.signature LIKE '%' || ? || ',%' ESCAPE '\'
	` // The three '%.' || ? variants are subsumed: '%' matches the dot boundary,
	// so "<type> " / "<type>)" / "<type>," already match after a '.'. Keeping a
	// minimal clause set lets the planner reuse one LIKE index/scan predicate.
	escaped := escapeLike(typeName)
	args := []any{escaped, escaped, escaped}

	if !includeTests {
		query += andIsTestFalse
	}
	query += orderByQualifiedName

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) MethodsByReceiver(typeName string, includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom + `
		WHERE (s.receiver = ? OR s.receiver = ?)
	`
	args := []any{typeName, "*" + typeName}
	if !includeTests {
		query += andIsTestFalse
	}
	query += orderByQualifiedName

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) MethodsByName(methodName string, includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom + `
		WHERE s.kind = 'method' AND s.name = ?
	`
	args := []any{methodName}

	if !includeTests {
		query += " AND s.is_test = FALSE"
	}
	query += " ORDER BY s.qualified_name"

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) AllSymbols(includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom
	if !includeTests {
		query += " WHERE s.is_test = FALSE"
	}
	query += " ORDER BY s.qualified_name"

	rows, err := s.db.QueryContext(context.Background(), query)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func (s *Store) SetIndexedAt(t time.Time) error {
	_, err := s.db.ExecContext(context.Background(), `INSERT OR REPLACE INTO meta (key, value) VALUES ('indexed_at', ?)`, t.Format(time.RFC3339Nano))
	return err
}

// SetRepoMeta records which repo and git revision an index was built from so
// staleness checks can detect when a DB was built elsewhere or on another HEAD.
func (s *Store) SetRepoMeta(repoPath, gitHead string, packageCount, symbolCount int) error {
	return s.SetRepoMetaWith(repoPath, gitHead, packageCount, symbolCount, "")
}

// SetRepoMetaWith records repo identity metadata plus an explicit indexedVia
// value (e.g. "go-list", "dir-walk"). An empty indexedVia omits the key.
func (s *Store) SetRepoMetaWith(repoPath, gitHead string, packageCount, symbolCount int, indexedVia string) error {
	ctx := context.Background()
	meta := map[string]string{
		"repo_path":     repoPath,
		"git_head":      gitHead,
		"package_count": strconv.Itoa(packageCount),
		"symbol_count":  strconv.Itoa(symbolCount),
	}
	if indexedVia != "" {
		meta["indexed_via"] = indexedVia
	}
	for k, v := range meta {
		if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, k, v); err != nil {
			return fmt.Errorf("setting meta %s: %w", k, err)
		}
	}
	return nil
}

// SetIndexedVia records how the index was built ("go-list" or "dir-walk"),
// touching only the indexed_via meta key so workspace writes can set it
// without disturbing repo/git metadata.
func (s *Store) SetIndexedVia(indexedVia string) error {
	if indexedVia == "" {
		return nil
	}
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('indexed_via', ?)`, indexedVia)
	return err
}

// EnsureRepo registers a workspace member by module path (canonical identity)
// and returns its repo_id. Re-registering the same module is idempotent.
func (s *Store) EnsureRepo(spec RepoSpec) (int64, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := ensureRepoInTx(ctx, tx, spec)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// RepoIDByModule returns the repo_id for a module path, or ok=false when the
// module is not registered.
func (s *Store) RepoIDByModule(modulePath string) (id int64, ok bool, err error) {
	err = s.db.QueryRowContext(context.Background(), `SELECT id FROM repos WHERE module_path = ?`, modulePath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// ListRepos returns every registered workspace member.
func (s *Store) ListRepos() ([]Repo, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT id, module_path, dir, missing, COALESCE(indexed_at, '')
		FROM repos
		ORDER BY module_path
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var repos []Repo
	for rows.Next() {
		var r Repo
		if err := rows.Scan(&r.ID, &r.ModulePath, &r.Dir, &r.Missing, &r.IndexedAt); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return repos, nil
}

// KnownModulePaths returns every registered module path (indexed or missing).
func (s *Store) KnownModulePaths() ([]string, error) {
	repos, err := s.ListRepos()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, r.ModulePath)
	}
	return out, nil
}

// SetRepoIndexedAt records when a member repo was last indexed. The global
// indexed_at meta key is also refreshed so single-repo health checks keep
// working on workspace databases. Both statements run in one transaction so a
// failure can never leave the per-repo and global timestamps disagreeing.
func (s *Store) SetRepoIndexedAt(modulePath string, t time.Time) error {
	ctx := context.Background()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE repos SET indexed_at = ? WHERE module_path = ?`, t.Format(time.RFC3339Nano), modulePath); err != nil {
			return err
		}
		if err := s.maybeFail("repo.meta", ""); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('indexed_at', ?)`, t.Format(time.RFC3339Nano))
		return err
	})
}

// RepoIndexedAt returns the last index time for a member, or ok=false when the
// member has never been indexed or is not registered.
func (s *Store) RepoIndexedAt(modulePath string) (time.Time, bool, error) {
	var val string
	err := s.db.QueryRowContext(context.Background(), `SELECT indexed_at FROM repos WHERE module_path = ?`, modulePath).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if val == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339Nano, val)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("repo %s: parse indexed_at %q: %w", modulePath, val, err)
	}
	return t, true, nil
}

// SetRepoMissing marks a member's checkout as absent so queries can report
// "repo not indexed" instead of pretending it is empty.
func (s *Store) SetRepoMissing(modulePath string, missing bool) error {
	_, err := s.db.ExecContext(context.Background(), `UPDATE repos SET missing = ? WHERE module_path = ?`, missing, modulePath)
	return err
}

// IsRepoStale reports whether a member has .go files newer than its last
// index. A repo with no recorded index time is stale. A missing checkout is
// not stale — it is missing, which callers distinguish separately.
func (s *Store) IsRepoStale(modulePath string) (bool, error) {
	indexedAt, ok, err := s.RepoIndexedAt(modulePath)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	var dir string
	if err := s.db.QueryRowContext(context.Background(), `SELECT dir FROM repos WHERE module_path = ?`, modulePath).Scan(&dir); err != nil {
		return false, err
	}
	if dir == "" {
		return false, nil
	}

	// Same dirty-state logic as StaleReason: uncommitted .go edits make the
	// index stale unless the exact state is already recorded.
	dirty, fp, derr := vcs.GitDirtyDiff(dir, "HEAD")
	if derr == nil {
		stored, sok := s.dirtyFingerprint(modulePath)
		if sok && stored == fp && !hasNewerGoFiles(dir, indexedAt) {
			return false, nil
		}
		if dirty {
			return true, nil
		}
	}
	return hasNewerGoFiles(dir, indexedAt), nil
}

// RepoState returns the lifecycle state of a member: indexed, stale, or
// missing. A member whose checkout vanished is missing regardless of mtimes.
func (s *Store) RepoState(r Repo) string {
	if r.Missing {
		return repoStateMissing
	}
	if _, err := os.Stat(r.Dir); os.IsNotExist(err) {
		return repoStateMissing
	}
	indexedAt, ok, _ := s.RepoIndexedAt(r.ModulePath)
	if !ok {
		return repoStateStale
	}
	if hasNewerGoFiles(r.Dir, indexedAt) {
		return repoStateStale
	}
	return repoStateIndexed
}

// WorkspaceHealth returns every member with its lifecycle state, the source of
// truth for per-repo index freshness.
func (s *Store) WorkspaceHealth() ([]Repo, error) {
	repos, err := s.ListRepos()
	if err != nil {
		return nil, err
	}
	return repos, nil
}

// HealthInfo describes the current index so callers can report why a query
// missed even though a symbol may exist on disk.
type HealthInfo struct {
	IndexedAt    string
	RepoPath     string
	GitHead      string
	IndexedVia   string // "go-list" or "dir-walk"; empty if unknown
	PackageCount int
	SymbolCount  int
}

// Health reports the current index metadata, or an error when the metadata is
// unreadable or corrupt. A corrupted database therefore surfaces as an error
// instead of a healthy-looking "indexed at epoch, 0 packages".
func (s *Store) Health() (HealthInfo, error) {
	var h HealthInfo
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT key, value FROM meta WHERE key IN ('indexed_at','repo_path','git_head','package_count','symbol_count','indexed_via')`)
	if err != nil {
		return h, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return h, err
		}
		switch k {
		case "indexed_at":
			h.IndexedAt = v
		case "repo_path":
			h.RepoPath = v
		case "git_head":
			h.GitHead = v
		case "package_count":
			n, aerr := strconv.Atoi(v)
			if aerr != nil {
				return h, fmt.Errorf("health: package_count %q: %w", v, aerr)
			}
			h.PackageCount = n
		case "symbol_count":
			n, aerr := strconv.Atoi(v)
			if aerr != nil {
				return h, fmt.Errorf("health: symbol_count %q: %w", v, aerr)
			}
			h.SymbolCount = n
		case "indexed_via":
			h.IndexedVia = v
		}
	}
	if err := rows.Err(); err != nil {
		return h, err
	}
	return h, nil
}

func (s *Store) WriteFiles(files map[string]string) error {
	_, err := s.writeFiles(context.Background(), nil, files, nil, fileScope{})
	return err
}

// WriteFilesRepo writes file contents, attributing each file to a repo via
// repoIDFor when provided (workspace mode); single-repo mode passes nil. It
// opens and commits its own transaction around the batched upsert.
func (s *Store) WriteFilesRepo(files map[string]string, repoIDFor func(path string) int64) error {
	_, err := s.writeFiles(context.Background(), nil, files, repoIDFor, fileScope{})
	return err
}

// fileScope describes which file rows a write manages for stale-row removal.
// repoID > 0 restricts cleanup to one workspace member (ReplaceRepo); repoID 0
// means the write owns every file row (single-repo and workspace full writes).
type fileScope struct {
	repoID int64
}

// fileWriteDirty reports whether a write changed the file-content index: any
// newly inserted path, any content update, or any deleted stale row. repo_id
// changes alone do not dirty the content index, which indexes path + content.
// For the index to stay consistent when a rebuild is skipped, unchanged rows
// must keep their rowid — hence the in-place upsert below instead of
// INSERT OR REPLACE.
//
// writeFiles writes file contents inside the caller's transaction, or opens and
// commits its own when tx is nil. All files are applied through a single
// prepared upsert, so a failure partway through the set leaves the files table
// completely untouched (none of the batch is visible).
func (s *Store) writeFiles(ctx context.Context, tx *sql.Tx, files map[string]string, repoIDFor func(path string) int64, scope fileScope) (bool, error) {
	own := tx == nil
	if own {
		var err error
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback() }()
	}

	dirty := false

	upsert, err := tx.PrepareContext(ctx, `
		INSERT INTO files (path, content, repo_id) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET content = excluded.content, repo_id = excluded.repo_id
		  WHERE files.content IS NOT excluded.content
		     OR COALESCE(files.repo_id, 0) IS NOT COALESCE(excluded.repo_id, 0)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = upsert.Close() }()

	for path, content := range files {
		var repoID any
		if repoIDFor != nil {
			repoID = nullableRepo(repoIDFor(path))
		}
		if err := s.maybeFail("files", path); err != nil {
			return false, fmt.Errorf("writing file %s: %w", path, err)
		}
		res, err := upsert.ExecContext(ctx, path, content, repoID)
		if err != nil {
			return false, fmt.Errorf("writing file %s: %w", path, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			dirty = true
		}
	}

	staleDirty, err := s.deleteStaleFiles(ctx, tx, files, scope)
	if err != nil {
		return false, err
	}
	dirty = dirty || staleDirty

	if own {
		if err := tx.Commit(); err != nil {
			return false, err
		}
	}
	return dirty, nil
}

// deleteStaleFiles removes file rows managed by a write whose path is no longer
// present in the incoming set. Unchanged rows were not rewritten by the upsert,
// so their rowids are untouched and the FTS content index stays valid without a
// rebuild.
func (s *Store) deleteStaleFiles(ctx context.Context, tx *sql.Tx, files map[string]string, scope fileScope) (bool, error) {
	if len(files) == 0 {
		return false, nil
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}

	scopeCond := ""
	var scopeArg any
	if scope.repoID > 0 {
		scopeCond = " AND repo_id = ?"
		scopeArg = scope.repoID
	}

	dirty := false
	for _, batch := range chunks(paths, edgesInBatchSize) {
		query := `DELETE FROM files WHERE path NOT IN (` + inPlaceholders(len(batch)) + `)` + scopeCond
		args := make([]any, 0, len(batch)+1)
		for _, p := range batch {
			args = append(args, p)
		}
		if scope.repoID > 0 {
			args = append(args, scopeArg)
		}
		if err := s.maybeFail("files", "stale"); err != nil {
			return false, err
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return false, fmt.Errorf("removing stale file rows: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			dirty = true
		}
	}
	return dirty, nil
}

func (s *Store) populateFileContentFTS(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO file_content_fts(file_content_fts) VALUES('rebuild')`)
	return err
}

func (s *Store) SearchFileContent(pattern, filePattern string, isRegex bool, contextLines int) ([]FileMatch, error) {
	var query string
	var args []any

	var re *regexp.Regexp
	if isRegex {
		compiled, err := cachedRegex(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex %q: %w", pattern, err)
		}
		re = compiled
		query = `SELECT path, content FROM files WHERE content REGEXP ?`
		args = []any{pattern}
	} else {
		sanitized := sanitizeFTSQuery(pattern)
		if sanitized == "" {
			return nil, nil
		}
		if ftsNeedsFullScan(pattern) {
			// The trigram tokenizer produces matches only for terms of at
			// least three characters; a shorter term silently matches nothing
			// in MATCH. Scan the files table directly so short patterns keep
			// returning the same verbatim matches they did under unicode61.
			query = `SELECT f.path, f.content FROM files f WHERE instr(f.content, ?) > 0`
			args = []any{pattern}
		} else {
			// The FTS MATCH is a whole-content candidate prefilter; the raw pattern
			// must still occur verbatim in the content so a row selected here always
			// yields at least one line from the per-line extractMatches walk. Without
			// this, a multi-term pattern whose terms sit on different lines would
			// select the row but then produce zero matches.
			query = `SELECT f.path, f.content FROM file_content_fts fts JOIN files f ON f.rowid = fts.rowid WHERE file_content_fts MATCH ? AND instr(f.content, ?) > 0`
			args = []any{sanitized, pattern}
		}
	}

	if filePattern != "" {
		if isRegex {
			query += " AND path REGEXP ?"
			args = append(args, filePattern)
		} else {
			query += " AND " + likeMatch("f.path")
			args = append(args, likePattern(filePattern))
		}
	}

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		if isRegex {
			return nil, fmt.Errorf("regex search for %q failed: %w", pattern, err)
		}
		return nil, fmt.Errorf("file content search failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var matches []FileMatch
	for rows.Next() {
		var filePath, content string
		if err := rows.Scan(&filePath, &content); err != nil {
			return nil, err
		}
		fileMatches := extractMatches(filePath, content, pattern, re, isRegex, contextLines)
		matches = append(matches, fileMatches...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}

func extractMatches(filePath, content, pattern string, re *regexp.Regexp, isRegex bool, contextLines int) []FileMatch {
	return extractMatchesFunc(filePath, content, func(line string) bool {
		if isRegex {
			return re.MatchString(line)
		}
		return strings.Contains(line, pattern)
	}, contextLines)
}

// extractMatchesFunc walks the content line by line and returns a FileMatch
// for every line the predicate accepts, with contextLines of surrounding
// text attached to each.
func extractMatchesFunc(filePath, content string, match func(line string) bool, contextLines int) []FileMatch {
	lines := strings.Split(content, "\n")
	var matches []FileMatch
	for i, line := range lines {
		if !match(line) {
			continue
		}
		_fm := FileMatch{
			FilePath:   filePath,
			LineNumber: i + 1,
			Line:       line,
		}
		if contextLines > 0 {
			start := max(0, i-contextLines)
			end := min(len(lines), i+contextLines+1)
			if start < i {
				_fm.ContextBefore = strings.Join(lines[start:i], "\n")
			}
			if i+1 < end {
				_fm.ContextAfter = strings.Join(lines[i+1:end], "\n")
			}
		}
		matches = append(matches, _fm)
	}
	return matches
}

func (s *Store) FileContent(filePath string) (string, error) {
	var content string
	err := s.db.QueryRowContext(context.Background(), `SELECT content FROM files WHERE path = ?`, filePath).Scan(&content)
	if err != nil {
		return "", err
	}
	return content, nil
}

// FileContentsByRepo returns indexed file contents grouped by member module
// path, used by contract analysis to scan call sites without re-walking source.
func (s *Store) FileContentsByRepo() (map[string]map[string]string, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT f.path, f.content, COALESCE(r.module_path, '')
		FROM files f
		LEFT JOIN repos r ON r.id = f.repo_id
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]map[string]string)
	for rows.Next() {
		var path, content, repo string
		if err := rows.Scan(&path, &content, &repo); err != nil {
			return nil, err
		}
		if repo == "" {
			continue
		}
		if out[repo] == nil {
			out[repo] = make(map[string]string)
		}
		out[repo][path] = content
	}
	return out, rows.Err()
}

// applyChurn applies git-churn counts to every symbol in the given files using
// a single UPDATE backed by a temporary staging table. The statement count is
// independent of the number of changed files; the pos_file lookup is serviced
// by the idx_symbols_pos_file index.
func applyChurn(ctx context.Context, tx *sql.Tx, churn map[string]int) error {
	if len(churn) == 0 {
		return nil
	}

	const stagingTable = "_codemap_churn"
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE `+stagingTable+` (file_path TEXT PRIMARY KEY, count INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("creating churn staging table: %w", err)
	}
	defer func() { _, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+stagingTable) }()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO `+stagingTable+` (file_path, count) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing churn staging insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for filePath, count := range churn {
		if _, err := stmt.ExecContext(ctx, filePath, count); err != nil {
			return fmt.Errorf("staging churn for %s: %w", filePath, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE symbols
		SET churn_count = (SELECT c.count FROM `+
		stagingTable+` c WHERE c.file_path = symbols.pos_file)
		WHERE pos_file IN (SELECT file_path FROM `+
		stagingTable+`)`); err != nil {
		return fmt.Errorf("applying batched churn: %w", err)
	}
	return nil
}

func (s *Store) IndexedAt() (time.Time, error) {
	var val string
	err := s.db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = 'indexed_at'`).Scan(&val)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, val)
}

// IsStale reports whether the database at dbPath should be rebuilt for the
// repo at repoPath. A database is stale when it was never indexed, was indexed
// for a different repo path or git HEAD, holds uncommitted .go edits relative
// to HEAD, or has .go files newer than its index timestamp.
func IsStale(dbPath, repoPath string) (bool, error) {
	stale, _, err := StaleReason(dbPath, repoPath)
	return stale, err
}

// StaleReason reports whether dbPath is stale for repoPath and why. It composes
// the same checks IsStale performs but surfaces the deciding factor so callers
// can distinguish "uncommitted .go changes", "a .go file is newer than the
// index", "indexed on another HEAD/path", "never indexed", and "index current".
// Repos where git is unavailable or the repo is not under git fall back to the
// mtime comparison only.
func StaleReason(dbPath, repoPath string) (bool, string, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return true, "database does not exist; never indexed", nil
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return true, fmt.Sprintf("open %s: %v", dbPath, err), nil
	}
	defer func() { _ = db.Close() }()

	indexedAt, ok, err := readIndexedAtDB(db)
	if err != nil {
		return true, fmt.Sprintf("indexed_at meta unreadable: %v", err), nil
	}
	if !ok {
		return true, "no indexed_at meta (never indexed)", nil
	}

	if mismatch, known := repoIdentityMismatchDB(db, repoPath); known && mismatch {
		return true, "indexed for a different repo path or git HEAD than the served repo", nil
	}

	// Uncommitted edits do not change HEAD, so mtime-based checks can miss them
	// (e.g. git apply preserving timestamps). Diffing against HEAD catches a
	// diverged working tree deterministically — unless that exact dirty state
	// has already been indexed (recorded via the dirty_fingerprint meta key),
	// in which case the index already reflects the working tree.
	dirty, fp, derr := vcs.GitDirtyDiff(repoPath, "HEAD")
	if derr == nil {
		stored, ok := readMetaDB(db, dirtyFingerprintKey)
		if ok && stored == fp && !hasNewerGoFiles(repoPath, indexedAt) {
			return false, "index reflects the current working tree", nil
		}
		if dirty {
			return true, "uncommitted .go changes not covered by the current index", nil
		}
	}

	if hasNewerGoFiles(repoPath, indexedAt) {
		return true, "a .go file is newer than the last index", nil
	}

	return false, "index is current", nil
}

// dirtyFingerprintKey records the working-tree .go diff a database was indexed
// from. Scoped per module (key "dirty_fingerprint" for single-repo databases,
// "dirty_fingerprint:<module>" for workspace members).
const dirtyFingerprintKey = "dirty_fingerprint"

func dirtyFingerprintMetaKey(modulePath string) string {
	if modulePath == "" {
		return dirtyFingerprintKey
	}
	return dirtyFingerprintKey + ":" + modulePath
}

// SetDirtyFingerprint records the working-tree .go diff fingerprint the index
// was built from, so staleness checks can tell "already indexed this exact
// dirty state" from "new uncommitted edits since the index".
func (s *Store) SetDirtyFingerprint(modulePath, fp string) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		dirtyFingerprintMetaKey(modulePath), fp)
	return err
}

// churnDegradedKey records why git churn was unavailable at index time, so
// churn-sensitive tools (hotspots) can report degraded risk instead of
// pretending empty churn is real data. An empty value means "no degradation".
const churnDegradedKey = "churn_degraded"

// SetChurnDegraded records why git churn was degraded (or clears it when reason
// is empty) for the current index.
func (s *Store) SetChurnDegraded(reason string) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, churnDegradedKey, reason)
	return err
}

// RecordChurnDegradation persists the churn-gathering failure as degraded
// churn. Empty repos (ErrNoCommits) are legitimate and produce no marker.
func (s *Store) RecordChurnDegradation(churnErr error) error {
	if churnErr == nil || errors.Is(churnErr, vcs.ErrNoCommits) {
		return nil
	}
	return s.SetChurnDegraded("git churn unavailable: " + churnErr.Error())
}

// ChurnDegraded returns the recorded churn-degradation reason, or "" when the
// index was built with real churn data.
func (s *Store) ChurnDegraded() (string, error) {
	var val string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, churnDegradedKey).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

// RecordDirtyFingerprint captures the working-tree .go diff fingerprint of dir
// into this database's meta for modulePath ("" for single-repo databases), so
// staleness checks know the index already reflects this exact dirty state.
// Failures are silently ignored: the worst case is one extra reindex.
func (s *Store) RecordDirtyFingerprint(dir, modulePath string) {
	if _, fp, err := vcs.GitDirtyDiff(dir, "HEAD"); err == nil {
		_ = s.SetDirtyFingerprint(modulePath, fp)
	}
}

// dirtyFingerprint reads the fingerprint recorded when this module was indexed.
func (s *Store) dirtyFingerprint(modulePath string) (string, bool) {
	var val string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, dirtyFingerprintMetaKey(modulePath)).Scan(&val)
	if err != nil {
		return "", false
	}
	return val, true
}

// repoIdentityMismatch reports whether the DB was indexed for a different repo
// path or git HEAD than repoPath. known is false for legacy DBs (no meta) or
// when git is unavailable, so callers fall back to mtime-only staleness.
func repoIdentityMismatch(dbPath, repoPath string) (mismatch, known bool) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return true, true
	}
	defer func() { _ = db.Close() }()
	return repoIdentityMismatchDB(db, repoPath)
}

// repoIdentityMismatchDB is repoIdentityMismatch against an already-open handle.
func repoIdentityMismatchDB(db *sql.DB, repoPath string) (mismatch, known bool) {
	metaRepo, ok := readMetaDB(db, "repo_path")
	if !ok {
		return false, false
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return true, true
	}
	if filepath.Clean(metaRepo) != filepath.Clean(absRepo) {
		return true, true
	}
	metaHead, ok := readMetaDB(db, "git_head")
	if !ok {
		return false, false
	}
	head, err := vcs.GitHead(absRepo)
	if err != nil {
		return false, false
	}
	return metaHead != head, true
}

func readMetaDB(db *sql.DB, key string) (string, bool) {
	var val string
	if err := db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = ?`, key).Scan(&val); err != nil {
		return "", false
	}
	return val, true
}

func readIndexedAtDB(db *sql.DB) (time.Time, bool, error) {
	var val string
	if err := db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = 'indexed_at'`).Scan(&val); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}

	indexedAt, err := time.Parse(time.RFC3339Nano, val)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse indexed_at %q: %w", val, err)
	}
	return indexedAt, true, nil
}

func hasNewerGoFiles(repoPath string, indexedAt time.Time) bool {
	stale := false
	err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || stale {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		if skipStaleDir(info.Name()) {
			return filepath.SkipDir
		}
		if dirHasNewerGoFile(path, indexedAt) {
			stale = true
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return true
	}
	return stale
}

func skipStaleDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules"
}

func dirHasNewerGoFile(dir string, indexedAt time.Time) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(indexedAt) {
			return true
		}
	}
	return false
}
