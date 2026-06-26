package query

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

func setupGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		c := exec.CommandContext(context.Background(), args[0], args[1:]...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %s: %v", args, string(out), err)
		}
	}
	return repo
}

func writeFile0644(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.CommandContext(context.Background(), "git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", args, string(out), err)
	}
}

func indexRepo(t *testing.T, root string) *store.Store {
	t.Helper()
	parseResult, err := parse.Run(root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

func TestChangedSymbolsModified(t *testing.T) {
	repo := setupGitRepo(t)
	srcDir := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(srcDir, "lib.go"), "package pkg\n\n// Greet returns hi.\nfunc Greet() string { return \"hi\" }\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(srcDir, "lib.go"), "package pkg\n\n// Greet returns hi there.\nfunc Greet() string { return \"hello\" }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if result.Summary.Modified == 0 {
		t.Errorf("expected at least one modified symbol, got %+v", result.Summary)
	}
	hasGreet := false
	for _, sym := range result.Symbols {
		t.Logf("symbol: %s type=%s", sym.QualifiedName, sym.ChangeType)
		if strings.Contains(sym.QualifiedName, "Greet") {
			hasGreet = true
			if sym.ChangeType != "modified" {
				t.Errorf("expected Greet change_type=modified, got %s", sym.ChangeType)
			}
		}
	}
	if !hasGreet {
		t.Errorf("expected Greet in result symbols")
	}
}

func TestChangedSymbolsAdded(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "b.go"), "package main\n\nfunc beta() {}\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if result.Summary.Added == 0 {
		t.Errorf("expected at least one added symbol, got %+v", result.Summary)
	}
	hasBeta := false
	for _, sym := range result.Symbols {
		if strings.Contains(sym.QualifiedName, "beta") {
			hasBeta = true
			if sym.ChangeType != "added" {
				t.Errorf("expected beta change_type=added, got %s", sym.ChangeType)
			}
		}
	}
	if !hasBeta {
		t.Errorf("expected beta in result")
	}
}

func TestChangedSymbolsDeleted(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	writeFile0644(t, filepath.Join(repo, "z.go"), "package main\n\nfunc zeta() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	if err := os.Remove(filepath.Join(repo, "z.go")); err != nil {
		t.Fatalf("rm: %v", err)
	}
	runGitIn(t, repo, "add", "-A")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if result.Summary.Removed == 0 {
		t.Errorf("expected at least one removed symbol, got %+v", result.Summary)
	}
}

func TestChangedSymbolsIncludeBodies(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, true, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	hasBody := false
	for _, sym := range result.Symbols {
		if sym.Body != "" {
			hasBody = true
		}
	}
	if !hasBody {
		t.Errorf("expected non-empty body when include_bodies=true")
	}
}

func TestChangedSymbolsWithBlastDisabled(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	for _, sym := range result.Symbols {
		if sym.BlastRadius != nil {
			t.Errorf("expected nil BlastRadius when with_blast_radius=false, got %+v", sym.BlastRadius)
		}
	}
}

func TestChangedSymbolsNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	writeFile0644(t, filepath.Join(dir, "a.go"), "package main\n")
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = s.Close() }()
	_, err = ChangedSymbols(s, dir, "HEAD", false, false, false)
	if err == nil {
		t.Fatal("expected error for non-git repo")
	}
}

func TestChangedSymbolsRefNotFound(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	_, err := ChangedSymbols(s, repo, "nonexistent-ref-xyz", false, false, false)
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
}
