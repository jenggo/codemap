package store

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthRoundTrip(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetRepoMeta("/repo/path", "abc123", 5, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}

	h, err := s.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.RepoPath != "/repo/path" || h.GitHead != "abc123" || h.PackageCount != 5 || h.SymbolCount != 42 {
		t.Fatalf("unexpected health: %+v", h)
	}
	if h.IndexedAt == "" {
		t.Fatal("expected indexed_at in health")
	}
}

func TestHealthStaleOnSourceChange(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	head := initGitRepo(t, repo)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.SetRepoMeta(repo, head, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}
	s.RecordSourceFingerprint("", repo, nil)

	h, err := s.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Stale {
		t.Fatal("unchanged source must not be stale")
	}

	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err = s.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.Stale {
		t.Fatal("edited source must be stale")
	}
}

func TestCreatePreservesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoMeta("/repo/path", "abc123", 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h, err := s.Health()
	if err != nil {
		t.Fatal(err)
	}
	if h.RepoPath != "/repo/path" || h.GitHead != "abc123" {
		t.Fatalf("existing metadata lost after Create: %+v", h)
	}
}

func TestSQLiteConcurrencyConfig(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	var timeout int
	if err := s.db.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 10000 {
		t.Fatalf("busy_timeout = %d, want 10000", timeout)
	}
	var journalMode string
	if err := s.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max open connections = %d, want 1", got)
	}

	// A transaction must take the write lock at BEGIN. With deferred mode two
	// processes can both start as readers and deadlock on upgrade, which
	// busy_timeout cannot break out of; the caller sees SQLITE_BUSY.
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('txlock_probe', '1')`); err != nil {
		t.Fatalf("write inside transaction: %v", err)
	}
}

func TestCachedRegexReusesCompiledPattern(t *testing.T) {
	pattern := `^cached-regex-pattern$`
	first, err := cachedRegex(pattern)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cachedRegex(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("cachedRegex returned different compiled patterns")
	}
}

func TestRepoIdentityMismatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if _, known := repoIdentityMismatch(dbPath, "/some/repo"); known {
		t.Fatal("expected unknown identity for empty DB")
	}

	repo := filepath.Join(t.TempDir(), "repo")
	head := initGitRepo(t, repo)

	if err := s.SetRepoMeta(repo, head, 1, 1); err != nil {
		t.Fatal(err)
	}

	if mismatch, known := repoIdentityMismatch(dbPath, repo); !known || mismatch {
		t.Fatalf("expected known match for same repo, got mismatch=%v known=%v", mismatch, known)
	}
	if mismatch, known := repoIdentityMismatch(dbPath, filepath.Join(t.TempDir(), "other")); !known || !mismatch {
		t.Fatalf("expected mismatch for different repo path, got mismatch=%v known=%v", mismatch, known)
	}
}

// TestIsStaleOnHeadChange verifies freshness follows content, not commits: an
// empty commit leaves the index current, and the same source committed on a new
// HEAD is still the same source.
func TestIsStaleOnHeadChange(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	head := initGitRepo(t, repo)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoMeta(repo, head, 1, 1); err != nil {
		t.Fatal(err)
	}
	markIndexed(t, s, time.Now())
	s.RecordSourceFingerprint("", repo, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	stale, err := IsStale(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("expected fresh DB to be not stale")
	}

	git := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("commit", "-q", "--allow-empty", "-m", "second")

	stale, err = IsStale(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("an empty commit changes no source and must not invalidate the index")
	}
}

// TestIsStaleOnUncommittedChanges verifies an uncommitted .go edit is stale even
// when its mtime is unchanged (as git apply/checkout can leave it): the source
// bytes, not the timestamp, decide.
func TestIsStaleOnUncommittedChanges(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	head := initGitRepo(t, repo)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoMeta(repo, head, 1, 1); err != nil {
		t.Fatal(err)
	}
	markIndexed(t, s, time.Now())
	s.RecordSourceFingerprint("", repo, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	stale, reason, err := StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatalf("expected fresh DB, got stale: %s", reason)
	}

	file := filepath.Join(repo, "a.go")
	if err := os.WriteFile(file, []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Age the file so no mtime signal exists; only content can decide.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, past, past); err != nil {
		t.Fatal(err)
	}

	stale, reason, err = StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale after uncommitted .go edit")
	}
	if !strings.Contains(reason, "source") {
		t.Fatalf("expected reason to mention source, got %q", reason)
	}
}

// TestHealthCorruptedDB verifies a corrupted database surfaces as a health
// error instead of a healthy-looking empty index.
func TestHealthCorruptedDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	removeSQLiteSidecars(t, dbPath)
	if err := os.WriteFile(dbPath, []byte("this is not a sqlite database at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	corrupt := &Store{db: opened}
	if _, err := corrupt.Health(); err == nil {
		t.Fatal("expected error for corrupted database, not a healthy empty index")
	}
}

// TestColumnExistsPropagatesError verifies a failing PRAGMA surfaces as an
// error instead of silently reporting the column as absent.
func TestColumnExistsPropagatesError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	removeSQLiteSidecars(t, dbPath)
	if err := os.WriteFile(dbPath, []byte("garbage, definitely not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if _, err := columnExists(opened, "edges", "sites"); err == nil {
		t.Fatal("expected error when PRAGMA table_info fails")
	}
}

func removeSQLiteSidecars(t *testing.T, dbPath string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// TestBadTimestampSurfacesError verifies a corrupt stored timestamp is reported
// as an error (matching IndexedAt semantics) and staleness surfaces it as the
// reason instead of silently treating the database as "never indexed".
func TestBadTimestampSurfacesError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}

	other, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(context.Background(), `UPDATE meta SET value = 'not-a-time' WHERE key = 'indexed_at'`); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(context.Background(), `INSERT OR REPLACE INTO repos (id, module_path, dir, missing, indexed_at) VALUES (1, 'example.com/x', '/tmp/x', 0, 'also-not-a-time')`); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.IndexedAt(); err == nil {
		t.Fatal("expected error for corrupt indexed_at")
	}
	if _, _, err := s.RepoIndexedAt("example.com/x"); err == nil {
		t.Fatal("expected error for corrupt repo indexed_at")
	}

	// Staleness surfaces the parse failure as its reason (and reindexes to
	// heal) rather than silently looping on a "never indexed" reading.
	stale, reason, err := StaleReason(dbPath, filepath.Join(t.TempDir(), "norepo"))
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale for corrupt timestamp")
	}
	if !strings.Contains(reason, "unreadable") {
		t.Fatalf("expected unreadable-timestamp reason, got %q", reason)
	}
}

// TestIsStaleFingerprintPreventsReindexLoop verifies that a working tree edited
// after indexing is stale exactly once: once the index records the source
// fingerprint of those edits, the index is current until the source changes
// again. Without this, a legitimately dirty dev tree would reindex after every
// query.
func TestIsStaleFingerprintPreventsReindexLoop(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	head := initGitRepo(t, repo)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.SetRepoMeta(repo, head, 1, 1); err != nil {
		t.Fatal(err)
	}
	markIndexed(t, s, time.Now())

	file := filepath.Join(repo, "a.go")
	past := time.Now().Add(-time.Hour)
	ageFile := func() {
		t.Helper()
		if err := os.Chtimes(file, past, past); err != nil {
			t.Fatal(err)
		}
	}

	// Unseen edit, aged mtime so only content can decide.
	if err := os.WriteFile(file, []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ageFile()

	stale, reason, err := StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale for unseen edit")
	}
	if !strings.Contains(reason, "source") {
		t.Fatalf("expected source reason, got %q", reason)
	}

	// Record the fingerprint, as the rebuilt index writer now does.
	s.RecordSourceFingerprint("", repo, nil)

	stale, reason, err = StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatalf("expected fresh for already-indexed source, got %q", reason)
	}

	// The source changes -> stale again.
	if err := os.WriteFile(file, []byte("package a\n\nvar X = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ageFile()

	stale, reason, err = StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale after source changed")
	}
	if !strings.Contains(reason, "source") {
		t.Fatalf("expected source reason, got %q", reason)
	}
}

// TestIsStaleOnRename verifies the fingerprint covers paths, not only contents:
// renaming a file leaves the same bytes in the tree but changes which package
// they belong to. git is absent here, so this also covers the non-repo case.
func TestIsStaleOnRename(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	markIndexed(t, s, time.Now())
	s.RecordSourceFingerprint("", dir, nil)

	stale, reason, err := StaleReason(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatalf("expected fresh for unchanged tree, got %q", reason)
	}

	if err := os.Rename(src, filepath.Join(dir, "b.go")); err != nil {
		t.Fatal(err)
	}

	stale, reason, err = StaleReason(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale after a .go file was renamed")
	}
	if !strings.Contains(reason, "source") {
		t.Fatalf("expected source reason, got %q", reason)
	}
}

// TestIsStaleOnAddedFile verifies an added .go file makes the index stale even
// though no indexed file changed.
func TestIsStaleOnAddedFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	markIndexed(t, s, time.Now())
	s.RecordSourceFingerprint("", dir, nil)

	stale, reason, err := StaleReason(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatalf("expected fresh for unchanged tree, got %q", reason)
	}

	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package a\n\nvar Y = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stale, reason, err = StaleReason(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale after a .go file was added")
	}
	if !strings.Contains(reason, "source") {
		t.Fatalf("expected source reason, got %q", reason)
	}
}

// TestIsStaleOnTouch verifies a touched-but-unchanged file does not invalidate
// the index: mtime is not an input to the fingerprint. This is the case the old
// mtime scan got wrong, and the one formatter/editor saves produce constantly.
func TestIsStaleOnTouch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	markIndexed(t, s, time.Now())
	s.RecordSourceFingerprint("", dir, nil)

	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}

	stale, reason, err := StaleReason(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatalf("touching a file must not invalidate the index, got %q", reason)
	}
}

func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "test")

	// Age the file so tests that depend on indexed_at ordering are unaffected.
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, past, past); err != nil {
		t.Fatal(err)
	}

	git("add", ".")
	git("commit", "-q", "-m", "init")
	out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
