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
	"time"

	sqlite "modernc.org/sqlite"

	"codemap/parse"
	"codemap/resolve"
	"codemap/vcs"
)

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
			matched, err := regexp.MatchString(pattern, s)
			if err != nil {
				return nil, err
			}
			if matched {
				return int64(1), nil
			}
			return int64(0), nil
		},
	)
}

const schemaVersion = 2

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
CREATE INDEX IF NOT EXISTS idx_edges_from          ON edges(from_ref);
CREATE INDEX IF NOT EXISTS idx_edges_to_ref        ON edges(to_ref);
CREATE INDEX IF NOT EXISTS idx_edges_type          ON edges(edge_type);
CREATE INDEX IF NOT EXISTS idx_edges_repo          ON edges(repo_id);
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
    tokenize='porter unicode61'
);
`

const andIsTestFalse = " AND s.is_test = FALSE"
const orderByQualifiedName = " ORDER BY s.qualified_name"

type Store struct {
	db *sql.DB
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

	if _, err := os.Stat(path); err == nil {
		_ = os.Remove(path)
	}

	db, err := sql.Open("sqlite", path)
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

	db, err := sql.Open("sqlite", path)
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

	const repoColumnDecl = "INTEGER REFERENCES repos(id)"
	const repoIDCol = "repo_id"
	for _, tc := range []struct{ table, column, decl string }{
		{"packages", repoIDCol, repoColumnDecl},
		{"symbols", repoIDCol, repoColumnDecl},
		{"edges", repoIDCol, repoColumnDecl},
		{"files", repoIDCol, repoColumnDecl},
	} {
		if !columnExists(db, tc.table, tc.column) {
			if _, err := db.ExecContext(ctx, "ALTER TABLE "+tc.table+" ADD COLUMN "+tc.column+" "+tc.decl); err != nil {
				return err
			}
		}
	}

	// Struct field info for contract shape matching (additive).
	if !columnExists(db, "symbols", "fields_json") {
		if _, err := db.ExecContext(ctx, `ALTER TABLE symbols ADD COLUMN fields_json TEXT`); err != nil {
			return err
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, strconv.Itoa(schemaVersion)); err != nil {
		return err
	}

	return ensureContractTables(db)
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

func tableExists(db *sql.DB, name string) bool {
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return err == nil && n > 0
}

func columnExists(db *sql.DB, table, column string) bool {
	rows, err := db.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		return false
	}
	return false
}

func (s *Store) Close() error {
	return s.db.Close()
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	repoByModule, err := registerRepos(ctx, tx, repos, workspace)
	if err != nil {
		return err
	}

	pkgCache, err := writePackages(ctx, tx, result.Packages, repoByModule)
	if err != nil {
		return err
	}

	if err := writeSymbols(ctx, tx, result.Symbols, pkgCache, repoByModule); err != nil {
		return err
	}

	if err := writeEdges(ctx, tx, result.Edges, pkgCache, repoByModule); err != nil {
		return err
	}

	if churn != nil {
		if err := applyChurn(ctx, tx, churn); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if err := s.populateFTS(); err != nil {
		return err
	}

	if len(files) > 0 {
		var repoIDFor func(string) int64
		if workspace {
			repoIDFor = buildRepoIDFor(repos, repoByModule)
		}
		if err := s.WriteFilesRepo(files, repoIDFor); err != nil {
			return err
		}
		if err := s.populateFileContentFTS(); err != nil {
			return err
		}
	}

	return nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteRepoRows(ctx, tx, repoID); err != nil {
		return err
	}
	repoByModule := map[string]int64{modulePath: repoID}
	if err := writeRepoRows(ctx, tx, result, repoByModule, churn); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := s.populateFTS(); err != nil {
		return err
	}
	if len(files) > 0 {
		if err := s.WriteFilesRepo(files, func(string) int64 { return repoID }); err != nil {
			return err
		}
		if err := s.populateFileContentFTS(); err != nil {
			return err
		}
	}
	return nil
}

// deleteRepoRows removes every row attributed to a repo, including any
// per-repo contract intelligence derived from it.
func deleteRepoRows(ctx context.Context, tx *sql.Tx, repoID int64) error {
	for _, stmt := range []string{
		`DELETE FROM edges WHERE repo_id = ?`,
		`DELETE FROM symbols WHERE repo_id = ?`,
		`DELETE FROM packages WHERE repo_id = ?`,
		`DELETE FROM files WHERE repo_id = ?`,
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
	if err := writeSymbols(ctx, tx, result.Symbols, pkgCache, repoByModule); err != nil {
		return err
	}
	if err := writeEdges(ctx, tx, result.Edges, pkgCache, repoByModule); err != nil {
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
// full re-index replaces them atomically.
func clearWorkspaceRows(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`DELETE FROM edges WHERE repo_id IS NOT NULL`,
		`DELETE FROM symbols WHERE repo_id IS NOT NULL`,
		`DELETE FROM packages WHERE repo_id IS NOT NULL`,
		`DELETE FROM files WHERE repo_id IS NOT NULL`,
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

// packageForRef returns the package import path owning ref, where ref is either
// the package path itself or a qualified symbol name built from it.
func packageForRef(ref string, pkgPaths []string) string {
	best := ""
	for _, p := range pkgPaths {
		if ref == p || strings.HasPrefix(ref, p+".") {
			if len(p) > len(best) {
				best = p
			}
		}
	}
	return best
}

func writeSymbols(ctx context.Context, tx *sql.Tx, symbols []resolve.ResolvedSymbol, pkgCache, repoByModule map[string]int64) error {
	symInsert := `INSERT OR IGNORE INTO symbols (qualified_name, package_id, name, kind, receiver, signature, doc, pos_file, pos_line, exported, is_test, complexity, repo_id, fields_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	pkgPaths := make([]string, 0, len(pkgCache))
	for p := range pkgCache {
		pkgPaths = append(pkgPaths, p)
	}

	for _, sym := range symbols {
		pkgPath := packageForRef(sym.Symbol.QualifiedName, pkgPaths)
		pkgID, ok := pkgCache[sym.Symbol.QualifiedName]
		if !ok {
			pkgID = pkgCache[pkgPath]
		}
		if pkgID == 0 {
			continue
		}
		repoID := repoForPackage(pkgPath, repoByModule)

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

func writeEdges(ctx context.Context, tx *sql.Tx, edges []resolve.ResolvedEdge, pkgCache, repoByModule map[string]int64) error {
	edgeInsert := `INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, repo_id) VALUES (?, ?, ?, ?, ?, ?)`

	pkgPaths := make([]string, 0, len(pkgCache))
	for p := range pkgCache {
		pkgPaths = append(pkgPaths, p)
	}

	for _, edge := range edges {
		pkgPath := packageForRef(edge.Edge.FromRef, pkgPaths)
		repoID := repoForPackage(pkgPath, repoByModule)
		_, err := tx.ExecContext(ctx, edgeInsert,
			edge.Edge.FromRef,
			edge.Edge.ToRef,
			edge.Edge.EdgeType,
			edge.Edge.Pos.File,
			edge.Edge.Pos.Line,
			nullableRepo(repoID),
		)
		if err != nil {
			return fmt.Errorf("inserting edge %s -> %s: %w", edge.Edge.FromRef, edge.Edge.ToRef, err)
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

func (s *Store) populateFTS() error {
	// symbols_fts is an external-content table; 'rebuild' re-indexes it from the
	// symbols table and is safe on both fresh and existing databases.
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO symbols_fts(symbols_fts) VALUES('rebuild')`)
	return err
}

func sanitizeFTSQuery(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ""
	}

	terms := strings.Fields(pattern)
	var quoted []string
	for _, t := range terms {
		t = strings.ReplaceAll(t, `"`, "")
		t = strings.ReplaceAll(t, "(", "")
		t = strings.ReplaceAll(t, ")", "")
		t = strings.ReplaceAll(t, "*", "")
		t = strings.TrimSpace(t)
		if t != "" {
			quoted = append(quoted, `"`+t+`"`)
		}
	}
	if len(quoted) == 0 {
		return ""
	}
	return strings.Join(quoted, " OR ")
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

type Edge struct {
	FromRef  string
	ToRef    string
	EdgeType string
	PosFile  string
	Repo     string `json:"repo,omitempty"`
	PosLine  int
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
	return s.queryEdges("from_ref = ?", ref)
}

func (s *Store) EdgesTo(ref string) ([]Edge, error) {
	return s.queryEdges("to_ref = ?", ref)
}

func (s *Store) EdgesByType(edgeType string) ([]Edge, error) {
	return s.queryEdges("edge_type = ?", edgeType)
}

func (s *Store) AllEdges() ([]Edge, error) {
	return s.queryEdges(sqlAllRows, "")
}

// WriteContracts replaces all contract intelligence data for a repo atomically.
// WriteContractsAll replaces all contract intelligence data globally in one
// transaction. Contract analysis is cross-repo, so a single run owns every row;
// each row is attributed to the repo of its from-side reference. The analysis
// timestamp is recorded so staleness checks can compare it to per-repo indexes.
func (s *Store) WriteContractsAll(contracts []Contract, runtimeContracts []RuntimeContract, drifts []DriftReport, indexedAt string) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DELETE FROM contracts`,
		`DELETE FROM runtime_contracts`,
		`DELETE FROM drift`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	if err := insertContracts(ctx, tx, contracts, indexedAt); err != nil {
		return err
	}
	if err := insertRuntimeContracts(ctx, tx, runtimeContracts, indexedAt); err != nil {
		return err
	}
	if err := insertDriftReports(ctx, tx, drifts, indexedAt); err != nil {
		return err
	}

	// Record the analysis time at nanosecond precision so staleness comparisons
	// against per-repo index times (same precision) are correct; parsing the
	// caller's RFC3339 string would truncate to whole seconds and make every
	// repo look perpetually stale.
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.SetContractAnalysisTime(time.Now().UTC())
}

func insertContracts(ctx context.Context, tx *sql.Tx, contracts []Contract, indexedAt string) error {
	for _, c := range contracts {
		repoID, err := repoIDForRef(ctx, tx, c.FromRef)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO contracts (from_ref, to_ref, direction, confidence, severity, suggested, evidence, indexed_at, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.FromRef, c.ToRef, string(c.Direction), c.Confidence, string(c.Severity), c.Suggested, c.Evidence, indexedAt, nullableRepo(repoID)); err != nil {
			return err
		}
	}
	return nil
}

func insertRuntimeContracts(ctx context.Context, tx *sql.Tx, runtimeContracts []RuntimeContract, indexedAt string) error {
	for _, rc := range runtimeContracts {
		repoID, err := repoIDForRef(ctx, tx, rc.FromRef)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runtime_contracts (kind, pattern, from_ref, to_ref, direction, evidence, indexed_at, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			string(rc.Kind), rc.Pattern, rc.FromRef, rc.ToRef, string(rc.Direction), rc.Evidence, indexedAt, nullableRepo(repoID)); err != nil {
			return err
		}
	}
	return nil
}

func insertDriftReports(ctx context.Context, tx *sql.Tx, drifts []DriftReport, indexedAt string) error {
	for _, d := range drifts {
		fieldsJSON, err := jsonMarshal(d.Fields)
		if err != nil {
			return err
		}
		repoID, err := repoIDForRef(ctx, tx, d.FromRef)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO drift (from_ref, to_ref, severity, fields_json, indexed_at, repo_id) VALUES (?, ?, ?, ?, ?, ?)`,
			d.FromRef, d.ToRef, string(d.Severity), fieldsJSON, indexedAt, nullableRepo(repoID)); err != nil {
			return err
		}
	}
	return nil
}

// repoIDForRef resolves the repo owning a qualified reference by the longest
// matching package path. Returns 0 (NULL) when no repo is attributed.
func repoIDForRef(ctx context.Context, tx *sql.Tx, ref string) (int64, error) {
	var repoID sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT p.repo_id
		FROM packages p
		WHERE ? = p.path OR ? LIKE p.path || '.%'
		ORDER BY length(p.path) DESC
		LIMIT 1
	`, ref, ref).Scan(&repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return repoID.Int64, nil
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
	_, err := s.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('contract_indexed_at', ?)`,
		t.Format(time.RFC3339Nano))
	return err
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

func (s *Store) queryEdges(where string, arg any) ([]Edge, error) {
	rows, err := s.db.QueryContext(context.Background(), "SELECT e.from_ref, e.to_ref, e.edge_type, e.pos_file, e.pos_line, r.module_path FROM edges e LEFT JOIN repos r ON r.id = e.repo_id WHERE "+where, arg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var edges []Edge
	for rows.Next() {
		var e Edge
		var repo sql.NullString
		if err := rows.Scan(&e.FromRef, &e.ToRef, &e.EdgeType, &e.PosFile, &e.PosLine, &repo); err != nil {
			return nil, err
		}
		e.Repo = repo.String
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
		WHERE s.pos_file LIKE ?
	`
	args := []any{"%" + filePattern + "%"}

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
	ftsQuery := sanitizeFTSQuery(pattern)
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
		WHERE s.qualified_name LIKE ?
	`
	args := []any{prefix + "%"}

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

func (s *Store) TransitiveImports(pkgPath string) ([]Edge, error) {
	visited := make(map[string]bool)
	result := make([]Edge, 0)
	queue := []string{pkgPath}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current] {
			continue
		}
		visited[current] = true

		edges, err := s.EdgesFrom(current)
		if err != nil {
			return nil, err
		}

		for _, e := range edges {
			if e.EdgeType == "imports" {
				result = append(result, e)
				if !visited[e.ToRef] {
					queue = append(queue, e.ToRef)
				}
			}
		}
	}

	return result, nil
}

func (s *Store) SearchByType(typeName string, includeTests bool) ([]Symbol, error) {
	query := `SELECT ` + symbolColumns + `
		` +
		symbolFrom + `
		WHERE s.signature LIKE '%' || ? || ' %'
		   OR s.signature LIKE '%' || ? || ')%'
		   OR s.signature LIKE '%' || ? || ',%'
		   OR s.signature LIKE '%.' || ? || ' %'
		   OR s.signature LIKE '%.' || ? || ')%'
		   OR s.signature LIKE '%.' || ? || ',%'
	`
	args := []any{typeName, typeName, typeName, typeName, typeName, typeName}

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
	ctx := context.Background()
	meta := map[string]string{
		"repo_path":     repoPath,
		"git_head":      gitHead,
		"package_count": strconv.Itoa(packageCount),
		"symbol_count":  strconv.Itoa(symbolCount),
	}
	for k, v := range meta {
		if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, k, v); err != nil {
			return fmt.Errorf("setting meta %s: %w", k, err)
		}
	}
	return nil
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
// working on workspace databases.
func (s *Store) SetRepoIndexedAt(modulePath string, t time.Time) error {
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET indexed_at = ? WHERE module_path = ?`, t.Format(time.RFC3339Nano), modulePath)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('indexed_at', ?)`, t.Format(time.RFC3339Nano))
	return err
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
		return time.Time{}, false, nil
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
	PackageCount int
	SymbolCount  int
}

func (s *Store) Health() HealthInfo {
	var h HealthInfo
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT key, value FROM meta WHERE key IN ('indexed_at','repo_path','git_head','package_count','symbol_count')`)
	if err != nil {
		return h
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		switch k {
		case "indexed_at":
			h.IndexedAt = v
		case "repo_path":
			h.RepoPath = v
		case "git_head":
			h.GitHead = v
		case "package_count":
			h.PackageCount, _ = strconv.Atoi(v)
		case "symbol_count":
			h.SymbolCount, _ = strconv.Atoi(v)
		}
	}
	if err := rows.Err(); err != nil {
		return h
	}
	return h
}

func (s *Store) WriteFiles(files map[string]string) error {
	return s.WriteFilesRepo(files, nil)
}

// WriteFilesRepo writes file contents, attributing each file to a repo via
// repoIDFor when provided (workspace mode); single-repo mode passes nil.
func (s *Store) WriteFilesRepo(files map[string]string, repoIDFor func(path string) int64) error {
	ctx := context.Background()
	for path, content := range files {
		var repoID any
		if repoIDFor != nil {
			repoID = nullableRepo(repoIDFor(path))
		}
		_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO files (path, content, repo_id) VALUES (?, ?, ?)`, path, content, repoID)
		if err != nil {
			return fmt.Errorf("writing file %s: %w", path, err)
		}
	}
	return nil
}

func (s *Store) populateFileContentFTS() error {
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO file_content_fts(file_content_fts) VALUES('rebuild')`)
	return err
}

func (s *Store) SearchFileContent(pattern, filePattern string, isRegex bool, contextLines int) ([]FileMatch, error) {
	var query string
	var args []any

	if isRegex {
		query = `SELECT path, content FROM files WHERE content REGEXP ?`
		args = []any{pattern}
	} else {
		sanitized := sanitizeFTSQuery(pattern)
		if sanitized == "" {
			return nil, nil
		}
		query = `SELECT f.path, f.content FROM file_content_fts fts JOIN files f ON f.rowid = fts.rowid WHERE file_content_fts MATCH ?`
		args = []any{sanitized}
	}

	if filePattern != "" {
		if isRegex {
			query += " AND path REGEXP ?"
		} else {
			query += " AND f.path LIKE ?"
			args = append(args, "%"+filePattern+"%")
		}
	}

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var matches []FileMatch
	for rows.Next() {
		var filePath, content string
		if err := rows.Scan(&filePath, &content); err != nil {
			return nil, err
		}
		fileMatches := extractMatches(filePath, content, pattern, isRegex, contextLines)
		matches = append(matches, fileMatches...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}

func extractMatches(filePath, content, pattern string, isRegex bool, contextLines int) []FileMatch {
	lines := strings.Split(content, "\n")
	var matches []FileMatch
	for i, line := range lines {
		var matched bool
		if isRegex {
			re, err := compileRegex(pattern)
			if err != nil {
				continue
			}
			matched = re.MatchString(line)
		} else {
			matched = strings.Contains(line, pattern)
		}
		if matched {
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
	}
	return matches
}

func compileRegex(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(pattern)
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

func applyChurn(ctx context.Context, tx *sql.Tx, churn map[string]int) error {
	for filePath, count := range churn {
		_, err := tx.ExecContext(ctx, `UPDATE symbols SET churn_count = ? WHERE pos_file = ?`, count, filePath)
		if err != nil {
			return fmt.Errorf("applying churn to %s: %w", filePath, err)
		}
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

func IsStale(dbPath, repoPath string) (bool, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return true, nil
	}

	indexedAt, ok := readIndexedAt(dbPath)
	if !ok {
		return true, nil
	}

	if mismatch, known := repoIdentityMismatch(dbPath, repoPath); known && mismatch {
		return true, nil
	}

	return hasNewerGoFiles(repoPath, indexedAt), nil
}

// repoIdentityMismatch reports whether the DB was indexed for a different repo
// path or git HEAD than repoPath. known is false for legacy DBs (no meta) or
// when git is unavailable, so callers fall back to mtime-only staleness.
func repoIdentityMismatch(dbPath, repoPath string) (mismatch, known bool) {
	metaRepo, ok := readMeta(dbPath, "repo_path")
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
	metaHead, ok := readMeta(dbPath, "git_head")
	if !ok {
		return false, false
	}
	head, err := vcs.GitHead(absRepo)
	if err != nil {
		return false, false
	}
	return metaHead != head, true
}

func readMeta(dbPath, key string) (string, bool) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = db.Close() }()

	var val string
	if err := db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = ?`, key).Scan(&val); err != nil {
		return "", false
	}
	return val, true
}

func readIndexedAt(dbPath string) (time.Time, bool) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = db.Close() }()

	var val string
	if err := db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = 'indexed_at'`).Scan(&val); err != nil {
		return time.Time{}, false
	}

	indexedAt, err := time.Parse(time.RFC3339Nano, val)
	if err != nil {
		return time.Time{}, false
	}
	return indexedAt, true
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
