package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"codemap/parse"
	"codemap/resolve"
)

// schemaV2DDL reproduces the pre-dedup schema (version 2): edges has no sites
// column and no unique pair index, so duplicate rows are permitted.
const schemaV2DDL = `
CREATE TABLE repos (
	id          INTEGER PRIMARY KEY,
	module_path TEXT NOT NULL UNIQUE,
	dir         TEXT NOT NULL,
	missing     BOOLEAN NOT NULL DEFAULT FALSE,
	indexed_at  TEXT
);
CREATE TABLE packages (
	id      INTEGER PRIMARY KEY,
	path    TEXT NOT NULL UNIQUE,
	name    TEXT NOT NULL,
	dir     TEXT NOT NULL,
	is_test BOOLEAN NOT NULL DEFAULT FALSE,
	repo_id INTEGER REFERENCES repos(id)
);
CREATE TABLE symbols (
	id             INTEGER PRIMARY KEY,
	qualified_name TEXT    NOT NULL UNIQUE,
	package_id     INTEGER NOT NULL REFERENCES packages(id),
	name           TEXT    NOT NULL,
	kind           TEXT    NOT NULL,
	receiver       TEXT,
	signature      TEXT,
	doc            TEXT,
	pos_file       TEXT    NOT NULL,
	pos_line       INTEGER NOT NULL,
	exported       BOOLEAN NOT NULL DEFAULT FALSE,
	is_test        BOOLEAN NOT NULL DEFAULT FALSE,
	complexity     INTEGER NOT NULL DEFAULT 0,
	churn_count    INTEGER NOT NULL DEFAULT 0,
	importance     REAL    NOT NULL DEFAULT 0.0,
	repo_id        INTEGER REFERENCES repos(id),
	fields_json    TEXT
);
CREATE TABLE edges (
	id        INTEGER PRIMARY KEY,
	from_ref  TEXT NOT NULL,
	to_ref    TEXT NOT NULL,
	edge_type TEXT NOT NULL,
	pos_file  TEXT NOT NULL,
	pos_line  INTEGER NOT NULL,
	repo_id   INTEGER REFERENCES repos(id)
);
CREATE TABLE files (
	path    TEXT PRIMARY KEY,
	content TEXT NOT NULL,
	repo_id INTEGER REFERENCES repos(id)
);
CREATE TABLE meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE contract_suppressions (
	from_ref TEXT NOT NULL,
	to_ref   TEXT NOT NULL,
	PRIMARY KEY (from_ref, to_ref)
);
CREATE VIRTUAL TABLE symbols_fts USING fts5(
	qualified_name, name, kind, receiver, signature, doc,
	content=symbols, content_rowid=id, tokenize='porter unicode61'
);
CREATE VIRTUAL TABLE file_content_fts USING fts5(
	path, content, content=files, content_rowid=rowid, tokenize='porter unicode61'
);
`

func writeV2DB(t *testing.T, dir string, edges [][6]any) string {
	t.Helper()
	path := filepath.Join(dir, "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, schemaV2DDL); err != nil {
		t.Fatalf("creating v2 schema: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (id, module_path, dir) VALUES (1, 'legacy.example/repo', '/repo')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO packages (id, path, name, dir, repo_id) VALUES (1, 'legacy.example/repo', 'repo', '/repo', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO symbols (id, qualified_name, package_id, name, kind, pos_file, pos_line, exported, repo_id) VALUES
			(1, 'legacy.example/repo.A', 1, 'A', 'func', 'a.go', 1, 1, 1),
			(2, 'legacy.example/repo.B', 1, 'B', 'func', 'a.go', 2, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	for i, e := range edges {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, repo_id) VALUES (?, ?, ?, ?, ?, ?)`,
			e[0], e[1], e[2], e[3], e[4], e[5]); err != nil {
			t.Fatalf("inserting edge %d: %v", i, err)
		}
	}
	// Meta must be present for the version check to see an upgrade.
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', '2')`); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMigrateDedupUpgrade verifies that opening a pre-dedup database with
// duplicate edge rows collapses them (keeping the first row per pair) and
// creates the unique pair index so the upgrade succeeds without failure.
func TestMigrateDedupUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := writeV2DB(t, dir, [][6]any{
		{"legacy.example/repo.A", "legacy.example/repo.B", "calls", "a.go", 3, 1},
		{"legacy.example/repo.A", "legacy.example/repo.B", "calls", "a.go", 3, 1},
		{"legacy.example/repo.A", "legacy.example/repo.B", "calls", "b.go", 7, 1},
		{"legacy.example/repo.A", "legacy.example/repo.B", "references", "c.go", 9, 1},
	})

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	edges, err := s.EdgesFrom("legacy.example/repo.A")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 unique edges after migration, got %d: %+v", len(edges), edges)
	}

	// The first row per pair is kept: calls for A->B retains the earliest line.
	if edges[0].EdgeType == "calls" && edges[0].PosLine != 3 {
		t.Errorf("expected first call site line 3 kept, got %d", edges[0].PosLine)
	}

	if !indexExists(s.db, "edges_pair") {
		t.Fatal("expected edges_pair unique index after migration")
	}
}

// TestMigrateFileContentFTSUpgradesTokenizer verifies opening a pre-v4 database
// recreates file_content_fts with the trigram tokenizer so identifier-fragment
// matches work after the upgrade.
func TestMigrateFileContentFTSUpgradesTokenizer(t *testing.T) {
	dir := t.TempDir()
	path := writeV2DB(t, dir, nil)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var ddl string
	if err := s.db.QueryRowContext(context.Background(), `SELECT sql FROM sqlite_master WHERE name = 'file_content_fts'`).Scan(&ddl); err != nil {
		t.Fatalf("read file_content_fts schema: %v", err)
	}
	if !strings.Contains(ddl, "trigram") {
		t.Fatalf("file_content_fts not migrated to trigram, got: %s", ddl)
	}
	if strings.Contains(ddl, "porter") {
		t.Fatalf("file_content_fts still uses porter tokenizer: %s", ddl)
	}
}

// TestMigrateUpgradeIdempotent verifies re-opening an upgraded database does
// not re-run the destructive dedup pass or fail on the index.
func TestMigrateUpgradeIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := writeV2DB(t, dir, [][6]any{
		{"legacy.example/repo.A", "legacy.example/repo.B", "calls", "a.go", 3, 1},
	})

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	edges, err := s2.EdgesFrom("legacy.example/repo.A")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge after idempotent open, got %d", len(edges))
	}
}

// TestFreshDBCreatesUniqueIndex verifies Create builds the dedup schema
// directly: the sites column exists and the unique pair index is present.
func TestFreshDBCreatesUniqueIndex(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	hasSites, err := columnExists(s.db, "edges", "sites")
	if err != nil {
		t.Fatalf("columnExists: %v", err)
	}
	if !hasSites {
		t.Fatal("expected sites column on fresh DB")
	}
	if !indexExists(s.db, "edges_pair") {
		t.Fatal("expected edges_pair index on fresh DB")
	}

	// Duplicate writes of the same pair within one transaction collapse.
	ctx := context.Background()
	up := `INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, sites, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(COALESCE(repo_id, 0), from_ref, to_ref, edge_type) DO UPDATE SET sites = excluded.sites`
	for i := range 3 {
		if _, err := s.db.ExecContext(ctx, up, "a.A", "a.B", "calls", "a.go", i+1, `[{"file":"a.go","line":1}]`, nil); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after duplicate upserts, got %d", count)
	}
}

// TestWorkspaceSamePairDistinctRepos verifies the unique index keeps the same
// (from, to, type) pair distinct across two repos while collapsing within one.
func TestWorkspaceSamePairDistinctRepos(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	exec(`INSERT INTO repos (id, module_path, dir) VALUES (1, 'example.com/r1', '/r1'), (2, 'example.com/r2', '/r2')`)
	exec(`INSERT INTO packages (id, path, name, dir, repo_id) VALUES (1, 'example.com/r1', 'r1', '/r1', 1), (2, 'example.com/r2', 'r2', '/r2', 2)`)
	exec(`INSERT INTO symbols (id, qualified_name, package_id, name, kind, pos_file, pos_line, exported, repo_id) VALUES
		(1, 'example.com/r1.Inbox', 1, 'Inbox', 'type', 'a.go', 1, 1, 1),
		(2, 'example.com/r1.Handler', 1, 'Handler', 'type', 'a.go', 2, 1, 1),
		(3, 'example.com/r2.Inbox', 2, 'Inbox', 'type', 'b.go', 1, 1, 2),
		(4, 'example.com/r2.Handler', 2, 'Handler', 'type', 'b.go', 2, 1, 2)`)
	up := `INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, sites, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(COALESCE(repo_id, 0), from_ref, to_ref, edge_type) DO UPDATE SET
			sites = (SELECT json_group_array(json(value)) FROM (
				SELECT value FROM json_each(edges.sites)
				UNION
				SELECT value FROM json_each(excluded.sites)
			))`
	// Same pair in both repos — must stay two rows.
	exec(up, "example.com/r1.Handler", "example.com/r1.Inbox", "satisfies", "a.go", 5, `[{"file":"a.go","line":5}]`, 1)
	exec(up, "example.com/r1.Handler", "example.com/r1.Inbox", "satisfies", "a.go", 6, `[{"file":"a.go","line":6}]`, 1)
	exec(up, "example.com/r2.Handler", "example.com/r2.Inbox", "satisfies", "b.go", 5, `[{"file":"b.go","line":5}]`, 2)

	edges, err := s.SymbolEdges([]string{"satisfies"})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 rows (one per repo), got %d: %+v", len(edges), edges)
	}
	repos := map[string]int{}
	for _, e := range edges {
		repos[e.Repo]++
	}
	if repos["example.com/r1"] != 1 || repos["example.com/r2"] != 1 {
		t.Errorf("expected exactly one row per repo, got %+v", repos)
	}
	for _, e := range edges {
		if e.Repo == "example.com/r1" && e.SiteCount != 2 {
			t.Errorf("r1: expected 2 aggregated sites, got %d", e.SiteCount)
		}
		if e.Repo == "example.com/r2" && e.SiteCount != 1 {
			t.Errorf("r2: expected single site, got %d", e.SiteCount)
		}
	}
}

// TestWriteIdempotentReIndex verifies writing the same result twice produces
// exactly one row per pair and aggregates sites across writes.
func TestWriteIdempotentReIndex(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "dedup"))
	if err != nil {
		t.Fatal(err)
	}
	res := resolve.Run(pr)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(res, nil, nil); err != nil {
		t.Fatalf("first write: %v", err)
	}
	count1 := countEdges(t, s)
	if err := s.Write(res, nil, nil); err != nil {
		t.Fatalf("second write: %v", err)
	}
	count2 := countEdges(t, s)
	if count1 != count2 {
		t.Fatalf("re-index changed row count: %d -> %d", count1, count2)
	}

	// A single multi-site call (e.g. Caller -> NewService across lines) must
	// aggregate into one row.
	edges, err := s.EdgesFrom("samepair.Caller")
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]int{}
	for _, e := range edges {
		byType[e.EdgeType]++
	}
	if byType["calls"] != 4 {
		t.Errorf("expected 4 unique call edges, got %d (%+v)", byType["calls"], edges)
	}
	if byType["references"] != 2 {
		t.Errorf("expected 2 unique reference edges (method value + field access), got %d", byType["references"])
	}
}

// TestSitesAggregationAcrossWrites verifies a call site added on a second write
// merges into the existing row's sites rather than creating a second row.
func TestSitesAggregationAcrossWrites(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "dedup"))
	if err != nil {
		t.Fatal(err)
	}
	res := resolve.Run(pr)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Write(res, nil, nil); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// Simulate a re-index that now sees an additional call site for the same pair
	// Caller -> NewService at a new line.
	up := `INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, sites, repo_id) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(COALESCE(repo_id, 0), from_ref, to_ref, edge_type) DO UPDATE SET
			sites = (SELECT json_group_array(json(value)) FROM (
				SELECT value FROM json_each(edges.sites)
				UNION
				SELECT value FROM json_each(excluded.sites)
			))`
	var rowID int
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM edges WHERE from_ref='samepair.Caller' AND to_ref='samepair.NewService' AND edge_type='calls'`).Scan(&rowID); err != nil {
		t.Fatalf("finding existing edge: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, up, "samepair.Caller", "samepair.NewService", "calls", "use.go", 99, `[{"file":"use.go","line":99}]`, nil); err != nil {
		t.Fatal(err)
	}

	var rowID2 int
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM edges WHERE from_ref='samepair.Caller' AND to_ref='samepair.NewService' AND edge_type='calls'`).Scan(&rowID2); err != nil {
		t.Fatalf("edge disappeared: %v", err)
	}
	if rowID2 != rowID {
		t.Fatalf("expected same row id after merge, got %d != %d", rowID2, rowID)
	}

	edges, err := s.EdgesFrom("samepair.Caller")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.FromRef == "samepair.Caller" && e.ToRef == "samepair.NewService" && e.EdgeType == "calls" {
			if len(e.Sites) != 2 {
				t.Fatalf("expected 2 aggregated sites, got %d: %+v", len(e.Sites), e.Sites)
			}
			if e.SiteCount != 2 {
				t.Fatalf("expected SiteCount 2, got %d", e.SiteCount)
			}
			// Primary position stays the first site.
			if e.PosLine != 4 {
				t.Errorf("expected primary pos to remain line 4, got %d", e.PosLine)
			}
		}
	}
}

func countEdges(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
