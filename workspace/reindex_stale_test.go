package workspace_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/store"
	"codemap/workspace"
)

func writeModuleFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestReindexStaleIsolatesBrokenRepo verifies a member that fails to reindex
// does not block the rest: the remaining stale repos are still reindexed and
// the failure is reported via the returned error.
func TestReindexStaleIsolatesBrokenRepo(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ws.db")

	broken := t.TempDir()
	writeModuleFile(t, filepath.Join(broken, "go.mod"), "module example.com/broken\n\ngo 1.21\n")
	writeModuleFile(t, filepath.Join(broken, "a.go"), "package broken\n")

	goodB := t.TempDir()
	writeModuleFile(t, filepath.Join(goodB, "go.mod"), "module example.com/b\n\ngo 1.21\n")
	writeModuleFile(t, filepath.Join(goodB, "b.go"), "package b\n\nfunc B() {}\n")

	goodC := t.TempDir()
	writeModuleFile(t, filepath.Join(goodC, "go.mod"), "module example.com/c\n\ngo 1.21\n")
	writeModuleFile(t, filepath.Join(goodC, "c.go"), "package c\n\nfunc C() {}\n")

	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, spec := range []store.RepoSpec{
		{ModulePath: "example.com/broken", Dir: broken},
		{ModulePath: "example.com/b", Dir: goodB},
		{ModulePath: "example.com/c", Dir: goodC},
	} {
		if _, err := s.EnsureRepo(spec); err != nil {
			t.Fatal(err)
		}
		if err := s.SetRepoIndexedAt(spec.ModulePath, old); err != nil {
			t.Fatal(err)
		}
	}
	// Corrupt the first member's indexed_at so its staleness check (and
	// therefore its reindex) fails while the others remain reindexable.
	other, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(context.Background(), `UPDATE repos SET indexed_at = 'garbage' WHERE module_path = 'example.com/broken'`); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reindexed, err := workspace.ReindexStale(dbPath)
	if err == nil {
		t.Fatal("expected a joined error for the broken repo")
	}
	if !strings.Contains(err.Error(), "example.com/broken") {
		t.Fatalf("expected error to name the broken repo, got: %v", err)
	}

	got := make(map[string]bool)
	for _, r := range reindexed {
		got[r] = true
	}
	if !got["example.com/b"] || !got["example.com/c"] {
		t.Fatalf("expected the healthy repos to still be reindexed, got %v", reindexed)
	}
}
