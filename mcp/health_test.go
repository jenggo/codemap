package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/store"
	"codemap/vcs"
)

// initHealthRepo creates a git repo with one tracked .go file, returning HEAD
// so health meta can point at it.
func initHealthRepo(t *testing.T, dir string) string {
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
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
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

func TestHandleHealthDirtySuffix(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	head := initHealthRepo(t, repo)

	st, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetRepoMeta(repo, head, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}

	out, _, isErr := handleHealth(st, nil, nil, map[string]any{})
	if isErr {
		t.Fatalf("health failed: %s", out)
	}
	if strings.Contains(out, "dirty=") {
		t.Fatalf("clean tree must have no dirty suffix: %q", out)
	}

	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, isErr = handleHealth(st, nil, nil, map[string]any{})
	if isErr {
		t.Fatalf("health failed: %s", out)
	}
	if !strings.Contains(out, "dirty=true (stale)") {
		t.Errorf("want dirty=true (stale), got %q", out)
	}

	_, fp, derr := vcs.GitDirtyDiff(repo, "HEAD")
	if derr != nil {
		t.Fatal(derr)
	}
	if err := st.SetDirtyFingerprint("", fp); err != nil {
		t.Fatal(err)
	}
	out, _, isErr = handleHealth(st, nil, nil, map[string]any{})
	if isErr {
		t.Fatalf("health failed: %s", out)
	}
	if !strings.Contains(out, "dirty=true (indexed)") {
		t.Errorf("want dirty=true (indexed), got %q", out)
	}
}
