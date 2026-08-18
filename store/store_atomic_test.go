package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"codemap/parse"
	"codemap/resolve"
)

// writeModule writes a temporary Go module and returns its root directory.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module atomic.example\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// parseModule indexes a module directory, returning the resolved result and the
// file-contents map keyed by absolute path.
func parseModule(t *testing.T, dir string) (*resolve.Result, map[string]string) {
	t.Helper()
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolve.Run(pr), parse.FileContents(pr)
}

func countFiles(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM files`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWriteFilesStageFailureRollsBack verifies that a failure while writing file
// contents aborts the whole write: no new symbols are committed and no new
// files are visible — the prior index is fully intact.
func TestWriteFilesStageFailureRollsBack(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a.go": "package ftstest\nfunc Keep() {}\n",
	})
	res1, files1 := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res1, files1, nil); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Add a new file with a new symbol and re-index, injecting a failure at the
	// middle of the file batch.
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package ftstest\nfunc New() {}\n"), 0o644)
	res2, files2 := parseModule(t, dir)
	bPath := filepath.Join(dir, "b.go")

	s.testHook = func(stage, key string) error {
		if stage == "files" && key == bPath {
			return errors.New("injected file-write failure")
		}
		return nil
	}
	err = s.Write(res2, files2, nil)
	s.testHook = nil
	if err == nil {
		t.Fatal("expected injected failure at files stage")
	}

	// No symbol from the failed write is visible.
	if _, err := s.SymbolByName("atomic.example.New"); err == nil {
		t.Fatal("new symbol committed despite rollback")
	}
	// The prior symbol and file are still intact.
	if _, err := s.SymbolByName("atomic.example.Keep"); err != nil {
		t.Fatalf("prior symbol lost after rollback: %v", err)
	}
	content, err := s.FileContent(filepath.Join(dir, "a.go"))
	if err != nil || strings.TrimSpace(content) == "" {
		t.Fatalf("prior file lost after rollback: err=%v", err)
	}
	// The partially written new file is invisible.
	if _, err := s.FileContent(bPath); err == nil {
		t.Fatal("partially written file visible after rollback")
	}
}

// TestWriteFTSFailureLeavesConsistentIndex verifies that a failure during the
// FTS rebuild rolls back the whole write: symbols are not committed ahead of
// their FTS index, and the previous combined index stays queryable.
func TestWriteFTSFailureLeavesConsistentIndex(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a.go": "package ftstest\nfunc Keep() {}\n",
	})
	res1, files1 := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res1, files1, nil); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package ftstest\nfunc New() {}\n"), 0o644)
	res2, files2 := parseModule(t, dir)

	s.testHook = func(stage, key string) error {
		if stage == "fts.symbols" {
			return errors.New("injected FTS failure")
		}
		return nil
	}
	err = s.Write(res2, files2, nil)
	s.testHook = nil
	if err == nil {
		t.Fatal("expected injected FTS failure")
	}

	// The new symbol never becomes queryable (symbol rows rolled back with the FTS).
	syms, err := s.SearchSymbols("New", "", nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 0 {
		t.Fatalf("stranded symbol queryable after FTS rollback: %+v", syms)
	}
	// The prior index is still queryable through the FTS.
	syms, err = s.SearchSymbols("Keep", "", nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 {
		t.Fatalf("expected prior symbol still searchable, got %d results", len(syms))
	}
}

// countingConnector wraps the sqlite driver to count prepared statements.
type countingConnector struct {
	driver     driver.Driver
	dsn        string
	prepares   atomic.Int64
	updates    atomic.Int64
	ftsContent atomic.Int64
}

func (c *countingConnector) Connect(_ context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, prepares: &c.prepares, updates: &c.updates, ftsContent: &c.ftsContent}, nil
}

func (c *countingConnector) Driver() driver.Driver { return c.driver }

type countingConn struct {
	driver.Conn
	prepares   *atomic.Int64
	updates    *atomic.Int64
	ftsContent *atomic.Int64
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	c.prepares.Add(1)
	trimmed := strings.TrimSpace(query)
	if strings.HasPrefix(trimmed, "UPDATE symbols") {
		c.updates.Add(1)
	}
	if strings.HasPrefix(trimmed, "INSERT INTO file_content_fts") && strings.Contains(strings.ToLower(trimmed), "values('rebuild')") {
		c.ftsContent.Add(1)
	}
	return c.Conn.Prepare(query)
}

// newCountingStore builds a Store whose underlying connection counts explicit
// prepared statements.
func newCountingStore(t *testing.T) (*Store, *countingConnector) {
	t.Helper()
	var c countingConnector
	c.driver = &sqlite.Driver{}
	c.dsn = filepath.Join(t.TempDir(), "test.db")
	db := sql.OpenDB(&c)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("creating schema: %v", err)
	}
	// The schema bootstrap goes through the prepared path on the wrapped conn;
	// reset so only the write-under-test is measured.
	c.prepares.Store(0)
	return &Store{db: db}, &c
}

// TestWriteFilesUsesSinglePreparedStatement verifies the file batch goes through
// a single prepared upsert (plus one prepared stale-row delete).
func TestWriteFilesUsesSinglePreparedStatement(t *testing.T) {
	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()

	files := map[string]string{
		"one.go":   "package p\n",
		"two.go":   "package p\n",
		"three.go": "package p\n",
	}
	if err := s.WriteFilesRepo(files, nil); err != nil {
		t.Fatalf("WriteFilesRepo: %v", err)
	}
	if got := c.prepares.Load(); got != 2 {
		t.Fatalf("expected 2 prepared statements (upsert + stale delete) for the file batch, got %d", got)
	}
}

// TestWriteFilesMidBatchFailureVisible verifies that a failure partway through
// the file set leaves zero files visible after the transaction rolls back.
func TestWriteFilesMidBatchFailureVisible(t *testing.T) {
	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()

	files := map[string]string{
		"one.go":   "package p\n",
		"two.go":   "package p\n",
		"three.go": "package p\n",
	}
	s.testHook = func(stage, key string) error {
		if stage == "files" && strings.HasSuffix(key, "two.go") {
			return errors.New("injected mid-batch failure")
		}
		return nil
	}
	err := s.WriteFilesRepo(files, nil)
	s.testHook = nil
	if err == nil {
		t.Fatal("expected injected mid-batch failure")
	}
	if got := c.prepares.Load(); got != 1 {
		t.Fatalf("expected single prepared upsert before the injected failure, got %d", got)
	}
	if n := countFiles(t, s); n != 0 {
		t.Fatalf("expected zero files visible after rollback, got %d", n)
	}
}

// TestSetRepoIndexedAtAtomic verifies the per-repo and global timestamps move
// together: an error on the second statement changes neither.
func TestSetRepoIndexedAtAtomic(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	spec := RepoSpec{ModulePath: "example.com/r", Dir: "/r"}
	if _, err := s.EnsureRepo(spec); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := s.SetRepoIndexedAt(spec.ModulePath, old); err != nil {
		t.Fatalf("initial SetRepoIndexedAt: %v", err)
	}

	s.testHook = func(stage, key string) error {
		if stage == "repo.meta" {
			return errors.New("injected meta failure")
		}
		return nil
	}
	err = s.SetRepoIndexedAt(spec.ModulePath, time.Now())
	s.testHook = nil
	if err == nil {
		t.Fatal("expected injected failure on second statement")
	}

	repoAt, ok, err := s.RepoIndexedAt(spec.ModulePath)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !repoAt.Equal(old) {
		t.Fatalf("repo timestamp changed despite failure: %v", repoAt)
	}
	globalAt, err := s.IndexedAt()
	if err != nil {
		t.Fatal(err)
	}
	if !globalAt.Equal(old) {
		t.Fatalf("global timestamp changed despite failure: %v", globalAt)
	}
}

// TestWriteContractsAllAtomic verifies a failed contract write leaves both the
// rows and the analysis clock unchanged.
func TestWriteContractsAllAtomic(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	first := []Contract{{
		FromRef: "a.A", ToRef: "b.B",
		Direction: ContractDirectionShared,
		Severity:  ContractSeverityCompatible,
	}}
	if err := s.WriteContractsAll(first, nil, nil, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("initial WriteContractsAll: %v", err)
	}
	before, ok, err := s.ContractAnalysisTime()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || before.IsZero() {
		t.Fatal("expected contract analysis clock set after successful write")
	}

	replacement := []Contract{{
		FromRef: "a.New", ToRef: "b.New",
		Direction: ContractDirectionShared,
		Severity:  ContractSeverityBreaking,
	}}
	s.testHook = func(stage, key string) error {
		if stage == "contract.meta" {
			return errors.New("injected clock failure")
		}
		return nil
	}
	err = s.WriteContractsAll(replacement, nil, nil, "2026-02-01T00:00:00Z")
	s.testHook = nil
	if err == nil {
		t.Fatal("expected injected clock failure")
	}

	rows, err := s.QueryContracts(ContractFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].FromRef != "a.A" {
		t.Fatalf("contract rows changed despite rollback: %+v", rows)
	}
	after, _, err := s.ContractAnalysisTime()
	if err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("analysis clock changed despite rollback: %v -> %v", before, after)
	}
}

// TestWriteReplacesStaleRows verifies a single-repo re-index atomically swaps
// the previous index: deleting a source file removes its symbols from the
// symbol table and its content from search_text.
func TestWriteReplacesStaleRows(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"gone.go": "package ftstest\nfunc Gone() {}\n",
		"keep.go": "package ftstest\nfunc Keep() {}\n",
	})
	res1, files1 := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res1, files1, nil); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	if _, err := s.SymbolByName("atomic.example.Gone"); err != nil {
		t.Fatalf("expected Gone before re-index: %v", err)
	}
	gonePath := filepath.Join(dir, "gone.go")
	if _, err := s.FileContent(gonePath); err != nil {
		t.Fatalf("expected gone.go content before re-index: %v", err)
	}

	// Delete the file and re-index; Gone must disappear from symbols and FTS.
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	res2, files2 := parseModule(t, dir)
	if err := s.Write(res2, files2, nil); err != nil {
		t.Fatalf("re-index: %v", err)
	}

	if _, err := s.SymbolByName("atomic.example.Gone"); err == nil {
		t.Fatal("stale symbol Gone still present after re-index")
	}
	if _, err := s.FileContent(gonePath); err == nil {
		t.Fatal("stale file content still present after re-index")
	}
	matches, err := s.SearchFileContent("Gone", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("search_text still finds deleted file: %+v", matches)
	}

	// The retained file and symbol remain queryable.
	if _, err := s.SymbolByName("atomic.example.Keep"); err != nil {
		t.Fatalf("retained symbol lost after re-index: %v", err)
	}
	matches, err = s.SearchFileContent("Keep", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("retained file content no longer searchable")
	}
}
