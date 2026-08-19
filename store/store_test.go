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

	"codemap/vcs"
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
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeout)
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
		t.Fatalf("expected known match for same repo+head, got mismatch=%v known=%v", mismatch, known)
	}
	if mismatch, known := repoIdentityMismatch(dbPath, filepath.Join(t.TempDir(), "other")); !known || !mismatch {
		t.Fatalf("expected mismatch for different repo path, got mismatch=%v known=%v", mismatch, known)
	}

	if err := s.SetRepoMeta(repo, "deadbeef", 1, 1); err != nil {
		t.Fatal(err)
	}
	if mismatch, known := repoIdentityMismatch(dbPath, repo); !known || !mismatch {
		t.Fatalf("expected mismatch for different head, got mismatch=%v known=%v", mismatch, known)
	}
}

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
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}
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
	if !stale {
		t.Fatal("expected stale after HEAD change")
	}
}

// TestIsStaleOnUncommittedChanges verifies the git-diff staleness signal: an
// uncommitted .go edit that preserves its mtime (as git apply/checkout can)
// must still mark the database stale, because mtime-only checks would miss it.
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
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}
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
	// Age the file so the mtime check cannot be the deciding signal; only the
	// git diff against HEAD can catch this edit.
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
	if !strings.Contains(reason, "uncommitted") {
		t.Fatalf("expected reason to mention uncommitted changes, got %q", reason)
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

// TestIsStaleFingerprintPreventsReindexLoop verifies that a working tree with
// uncommitted .go changes is only stale while its dirty state is unseen: once
// the index records the exact dirty-state fingerprint, the index is current
// until the working tree changes again. Without this, a legitimately dirty
// dev tree would trigger a reindex after every query.
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
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(repo, "a.go")
	past := time.Now().Add(-time.Hour)
	ageFile := func() {
		t.Helper()
		if err := os.Chtimes(file, past, past); err != nil {
			t.Fatal(err)
		}
	}

	// Unseen uncommitted edit (aged mtime so only the git-diff signal fires).
	if err := os.WriteFile(file, []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ageFile()

	stale, reason, err := StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale for unseen uncommitted edit")
	}
	if !strings.Contains(reason, "uncommitted") {
		t.Fatalf("expected uncommitted reason, got %q", reason)
	}

	// Record the fingerprint, as the rebuilt index writer now does.
	_, fp, derr := vcs.GitDirtyDiff(repo, "HEAD")
	if derr != nil {
		t.Fatal(derr)
	}
	if err := s.SetDirtyFingerprint("", fp); err != nil {
		t.Fatal(err)
	}

	stale, reason, err = StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatalf("expected fresh for already-indexed dirty state, got %q", reason)
	}

	// The dirty state changes -> stale again.
	if err := os.WriteFile(file, []byte("package a\n\nvar X = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ageFile()

	stale, reason, err = StaleReason(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale after dirty state changed")
	}
	if !strings.Contains(reason, "uncommitted") {
		t.Fatalf("expected uncommitted reason, got %q", reason)
	}
}

// TestIsStaleOnNewerMtime verifies the mtime fallback still fires when git is
// unavailable or clean but a .go file is newer than the last index.
func TestIsStaleOnNewerMtime(t *testing.T) {
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
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Bump the file's mtime into the future, strictly after the recorded
	// indexed_at, so only the mtime signal can decide staleness.
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}

	stale, reason, err := StaleReason(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale when .go mtime is newer than the index")
	}
	if !strings.Contains(reason, "never indexed") && !strings.Contains(reason, "newer than the last index") {
		t.Fatalf("expected a meaningful staleness reason, got %q", reason)
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

	// Age the file so the RFC3339 (second-precision) indexed_at is never older
	// than its mtime, isolating the test to the git-HEAD staleness path.
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
