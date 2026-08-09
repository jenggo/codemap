package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

const schemaSQL = `
CREATE TABLE IF NOT EXISTS packages (
    id          INTEGER PRIMARY KEY,
    path        TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL,
    dir         TEXT    NOT NULL,
    is_test     BOOLEAN NOT NULL DEFAULT FALSE
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
    importance      REAL    NOT NULL DEFAULT 0.0
);

CREATE TABLE IF NOT EXISTS edges (
    id          INTEGER PRIMARY KEY,
    from_ref    TEXT    NOT NULL,
    to_ref      TEXT    NOT NULL,
    edge_type   TEXT    NOT NULL,
    pos_file    TEXT    NOT NULL,
    pos_line    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
    path    TEXT PRIMARY KEY,
    content TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_symbols_package   ON symbols(package_id);
CREATE INDEX IF NOT EXISTS idx_symbols_qualified  ON symbols(qualified_name);
CREATE INDEX IF NOT EXISTS idx_symbols_kind        ON symbols(kind);
CREATE INDEX IF NOT EXISTS idx_edges_from          ON edges(from_ref);
CREATE INDEX IF NOT EXISTS idx_edges_to_ref        ON edges(to_ref);
CREATE INDEX IF NOT EXISTS idx_edges_type          ON edges(edge_type);

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

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Write(result *resolve.Result, files map[string]string, churn map[string]int) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	pkgCache, err := writePackages(ctx, tx, result.Packages)
	if err != nil {
		return err
	}

	if err := writeSymbols(ctx, tx, result.Symbols, pkgCache); err != nil {
		return err
	}

	if err := writeEdges(ctx, tx, result.Edges); err != nil {
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
		if err := s.WriteFiles(files); err != nil {
			return err
		}
		if err := s.populateFileContentFTS(); err != nil {
			return err
		}
	}

	return nil
}

func writePackages(ctx context.Context, tx *sql.Tx, packages []parse.PackageInfo) (map[string]int64, error) {
	pkgCache := make(map[string]int64)
	for _, pkg := range packages {
		var id int64
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO packages (path, name, dir, is_test) VALUES (?, ?, ?, ?)`,
			pkg.ImportPath, pkg.Name, pkg.Dir, pkg.IsTest)
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

func writeSymbols(ctx context.Context, tx *sql.Tx, symbols []resolve.ResolvedSymbol, pkgCache map[string]int64) error {
	symInsert := `INSERT OR IGNORE INTO symbols (qualified_name, package_id, name, kind, receiver, signature, doc, pos_file, pos_line, exported, is_test, complexity) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, sym := range symbols {
		pkgID, ok := pkgCache[sym.Symbol.QualifiedName]
		if !ok {
			for pkgPath := range pkgCache {
				if strings.HasPrefix(sym.Symbol.QualifiedName, pkgPath+".") {
					pkgID = pkgCache[pkgPath]
					break
				}
			}
		}
		if pkgID == 0 {
			continue
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
		)
		if err != nil {
			return fmt.Errorf("inserting symbol %s: %w", sym.Symbol.QualifiedName, err)
		}
	}
	return nil
}

func writeEdges(ctx context.Context, tx *sql.Tx, edges []resolve.ResolvedEdge) error {
	edgeInsert := `INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line) VALUES (?, ?, ?, ?, ?)`
	for _, edge := range edges {
		_, err := tx.ExecContext(ctx, edgeInsert,
			edge.Edge.FromRef,
			edge.Edge.ToRef,
			edge.Edge.EdgeType,
			edge.Edge.Pos.File,
			edge.Edge.Pos.Line,
		)
		if err != nil {
			return fmt.Errorf("inserting edge %s -> %s: %w", edge.Edge.FromRef, edge.Edge.ToRef, err)
		}
	}
	return nil
}

func (s *Store) populateFTS() error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO symbols_fts(rowid, qualified_name, name, kind, receiver, signature, doc)
		SELECT id, qualified_name, name, kind, receiver, signature, doc FROM symbols
	`)
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
	IsTest   bool
	SymCount int
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
	PosLine       int
	Exported      bool
	IsTest        bool
	Complexity    int
	ChurnCount    int
	Importance    float64
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
	PosLine  int
}

func (s *Store) ListPackages() ([]Package, error) {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT p.path, p.name, p.dir, p.is_test, COUNT(s.id)
		FROM packages p
		LEFT JOIN symbols s ON s.package_id = p.id
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
		if err := rows.Scan(&p.Path, &p.Name, &p.Dir, &p.IsTest, &p.SymCount); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pkgs, nil
}

func (s *Store) SymbolsByPackage(pkgPath string, includeTests bool) ([]Symbol, error) {
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
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
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) SymbolByName(qualifiedName string) (*Symbol, error) {
	var sym Symbol
	err := s.db.QueryRowContext(context.Background(), `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
		WHERE s.qualified_name = ?
	`, qualifiedName).Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance)
	if err != nil {
		return nil, err
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
	return s.queryEdges("1=1", "")
}

func (s *Store) queryEdges(where string, arg any) ([]Edge, error) {
	rows, err := s.db.QueryContext(context.Background(), "SELECT from_ref, to_ref, edge_type, pos_file, pos_line FROM edges WHERE "+where, arg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var edges []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.FromRef, &e.ToRef, &e.EdgeType, &e.PosFile, &e.PosLine); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return edges, nil
}

func (s *Store) SearchSymbolsByFile(filePattern, kind string, exported *bool, includeTests bool) ([]Symbol, error) {
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
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
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) SearchSymbols(pattern, kind string, exported *bool, pkgPath string, includeTests bool) ([]Symbol, error) {
	ftsQuery := sanitizeFTSQuery(pattern)
	if ftsQuery == "" {
		return nil, nil
	}

	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols_fts fts
		JOIN symbols s ON s.id = fts.rowid
		JOIN packages p ON s.package_id = p.id
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
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) SearchByQualifiedNamePrefix(prefix string, includeTests bool) ([]Symbol, error) {
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
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
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
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
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
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
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) MethodsByReceiver(typeName string, includeTests bool) ([]Symbol, error) {
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
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
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) MethodsByName(methodName string, includeTests bool) ([]Symbol, error) {
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
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
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) AllSymbols(includeTests bool) ([]Symbol, error) {
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test, s.complexity, s.churn_count, s.importance
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
	`
	if !includeTests {
		query += " WHERE s.is_test = FALSE"
	}
	query += " ORDER BY s.qualified_name"

	rows, err := s.db.QueryContext(context.Background(), query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var syms []Symbol
	for rows.Next() {
		var sym Symbol
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest, &sym.Complexity, &sym.ChurnCount, &sym.Importance); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) SetIndexedAt(t time.Time) error {
	_, err := s.db.ExecContext(context.Background(), `INSERT OR REPLACE INTO meta (key, value) VALUES ('indexed_at', ?)`, t.Format(time.RFC3339))
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
	ctx := context.Background()
	for path, content := range files {
		_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO files (path, content) VALUES (?, ?)`, path, content)
		if err != nil {
			return fmt.Errorf("writing file %s: %w", path, err)
		}
	}
	return nil
}

func (s *Store) populateFileContentFTS() error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO file_content_fts(rowid, path, content)
		SELECT rowid, path, content FROM files
	`)
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
	return time.Parse(time.RFC3339, val)
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

	indexedAt, err := time.Parse(time.RFC3339, val)
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
