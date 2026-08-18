package vcs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		c := exec.CommandContext(context.Background(), args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %s: %v", args, string(out), err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", args, string(out), err)
	}
}

func TestGitChangedFiles(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	writeFile(t, filepath.Join(repo, "a.go"), "package a\n")
	writeFile(t, filepath.Join(repo, "b.txt"), "text\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile(t, filepath.Join(repo, "a.go"), "package a\n// changed\n")
	writeFile(t, filepath.Join(repo, "c.go"), "package c\n")
	runGitIn(t, repo, "add", ".")

	files, err := GitChangedFiles(repo, "HEAD")
	if err != nil {
		t.Fatalf("GitChangedFiles: %v", err)
	}
	has := func(name string) bool {
		return slices.Contains(files, name)
	}
	if !has("a.go") {
		t.Errorf("expected a.go in changed files: %v", files)
	}
	if !has("c.go") {
		t.Errorf("expected c.go in changed files: %v", files)
	}
	for _, f := range files {
		if f == "b.txt" {
			t.Errorf("did not expect b.txt (non-go): %v", files)
		}
	}
}

func TestGitHunks(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	writeFile(t, filepath.Join(repo, "x.go"), "line1\nline2\nline3\nline4\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile(t, filepath.Join(repo, "x.go"), "line1\nline2-modified\nline3\nNEW-LINE\nline4\n")
	runGitIn(t, repo, "add", ".")

	hunks, err := GitHunks(repo, "HEAD", "x.go")
	if err != nil {
		t.Fatalf("GitHunks: %v", err)
	}
	if len(hunks) == 0 {
		t.Fatal("expected at least one hunk")
	}
	allAdded := []int{}
	for _, h := range hunks {
		allAdded = append(allAdded, h.Added...)
	}
	hasLine4 := false
	for _, ln := range allAdded {
		if ln == 4 {
			hasLine4 = true
		}
	}
	if !hasLine4 {
		t.Errorf("expected line 4 in added lines: %v", allAdded)
	}
}

func TestGitDeletedFiles(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	writeFile(t, filepath.Join(repo, "gone.go"), "package gone\n")
	writeFile(t, filepath.Join(repo, "stays.go"), "package stays\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	if err := os.Remove(filepath.Join(repo, "gone.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	runGitIn(t, repo, "add", "-A")

	deleted, err := GitDeletedFiles(repo, "HEAD")
	if err != nil {
		t.Fatalf("GitDeletedFiles: %v", err)
	}
	has := false
	for _, f := range deleted {
		if f == "gone.go" {
			has = true
		}
	}
	if !has {
		t.Errorf("expected gone.go in deleted: %v", deleted)
	}
}

func TestNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := GitChangedFiles(dir, "HEAD")
	if err == nil {
		t.Fatal("expected error for non-git dir")
	}
	if !strings.Contains(err.Error(), "not a git repository") && !strings.Contains(err.Error(), "fatal") {
		t.Errorf("expected git error, got: %v", err)
	}
}

func TestRefNotFound(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	_, err := GitChangedFiles(repo, "nonexistent-ref-xyz")
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
}

func TestGitFileChurn(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	writeFile(t, filepath.Join(repo, "a.go"), "package a\n")
	writeFile(t, filepath.Join(repo, "b.txt"), "text\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile(t, filepath.Join(repo, "a.go"), "package a\n// change1\n")
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n// change2\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "change a")

	churn, err := GitFileChurn(repo, "HEAD")
	if err != nil {
		t.Fatalf("GitFileChurn: %v", err)
	}
	if churn["a.go"] != 2 {
		t.Errorf("expected a.go churn=2, got %d", churn["a.go"])
	}
	if _, ok := churn["b.txt"]; ok {
		t.Errorf("b.txt should not be in churn map")
	}
}

func TestGitFileChurnNonGit(t *testing.T) {
	dir := t.TempDir()
	_, err := GitFileChurn(dir, "HEAD")
	if err == nil {
		t.Fatal("expected an error for a non-git directory, not silently empty churn")
	}
}

func TestGitFileChurnNoCommits(t *testing.T) {
	repo := t.TempDir()
	runGitIn(t, repo, "init", "-q")
	runGitIn(t, repo, "config", "user.email", "test@example.com")
	runGitIn(t, repo, "config", "user.name", "Test")
	writeFile(t, filepath.Join(repo, "a.go"), "package a\n")

	_, err := GitFileChurn(repo, "HEAD")
	if !errors.Is(err, ErrNoCommits) {
		t.Fatalf("expected ErrNoCommits for a repo with no commits, got: %v", err)
	}
}
