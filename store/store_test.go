package store

import (
	"context"
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

	h := s.Health()
	if h.RepoPath != "/repo/path" || h.GitHead != "abc123" || h.PackageCount != 5 || h.SymbolCount != 42 {
		t.Fatalf("unexpected health: %+v", h)
	}
	if h.IndexedAt == "" {
		t.Fatal("expected indexed_at in health")
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
