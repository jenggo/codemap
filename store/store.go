package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"codemap/parse"
	"codemap/resolve"
)

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
    is_test         BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS edges (
    id          INTEGER PRIMARY KEY,
    from_ref    TEXT    NOT NULL,
    to_ref      TEXT    NOT NULL,
    edge_type   TEXT    NOT NULL,
    pos_file    TEXT    NOT NULL,
    pos_line    INTEGER NOT NULL
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
		return nil, fmt.Errorf("codemap index not found: run 'codemap index' first or use the codemap_index MCP tool")
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM symbols").Scan(&count); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("codemap database is empty: run 'codemap index' first or use the codemap_index MCP tool")
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Write(result *resolve.Result) error {
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

	if err := tx.Commit(); err != nil {
		return err
	}

	return s.populateFTS()
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
	symInsert := `INSERT OR IGNORE INTO symbols (qualified_name, package_id, name, kind, receiver, signature, doc, pos_file, pos_line, exported, is_test) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
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
	Path    string
	Name    string
	Dir     string
	IsTest  bool
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
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
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
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
		FROM symbols s
		JOIN packages p ON s.package_id = p.id
		WHERE s.qualified_name = ?
	`, qualifiedName).Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest)
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

func (s *Store) SearchSymbolsByFile(filePattern string, kind string, exported *bool, includeTests bool) ([]Symbol, error) {
	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return syms, nil
}

func (s *Store) SearchSymbols(pattern string, kind string, exported *bool, pkgPath string, includeTests bool) ([]Symbol, error) {
	ftsQuery := sanitizeFTSQuery(pattern)
	if ftsQuery == "" {
		return nil, nil
	}

	query := `
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
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
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
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
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
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
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
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
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
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
		SELECT s.qualified_name, p.path, s.name, s.kind, s.receiver, s.signature, s.doc, s.pos_file, s.pos_line, s.exported, s.is_test
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
		if err := rows.Scan(&sym.QualifiedName, &sym.PackagePath, &sym.Name, &sym.Kind, &sym.Receiver, &sym.Signature, &sym.Doc, &sym.PosFile, &sym.PosLine, &sym.Exported, &sym.IsTest); err != nil {
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

func (s *Store) IndexedAt() (time.Time, error) {
	var val string
	err := s.db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = 'indexed_at'`).Scan(&val)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, val)
}

func IsStale(dbPath string, repoPath string) (bool, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return true, nil
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return true, nil
	}
	defer func() { _ = db.Close() }()

	var val string
	if err := db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = 'indexed_at'`).Scan(&val); err != nil {
		return true, nil
	}

	indexedAt, err := time.Parse(time.RFC3339, val)
	if err != nil {
		return true, nil
	}

	stale := false
	err = filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || stale {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" {
			return filepath.SkipDir
		}
		entries, _ := os.ReadDir(path)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".go") {
				fi, err := e.Info()
				if err != nil {
					continue
				}
				if fi.ModTime().After(indexedAt) {
					stale = true
					return filepath.SkipDir
				}
			}
		}
		return nil
	})
	if err != nil {
		return true, nil
	}

	return stale, nil
}
