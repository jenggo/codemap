package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeModuleNamed writes a temporary Go module with the given module path.
func writeModuleNamed(t *testing.T, module string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+module+"\n\ngo 1.26\n"), 0o644); err != nil {
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

// TestSelectiveFTSRebuild verifies the content-FTS rebuild trigger: the first
// index always rebuilds, an unchanged re-index skips the rebuild, a content
// change forces one, and a removed file forces one.
func TestSelectiveFTSRebuild(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a.go": "package ftstest\nfunc A() {}\n",
		"b.go": "package ftstest\nfunc B() {}\n",
	})
	res, files := parseModule(t, dir)

	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()

	// First index always rebuilds the content FTS.
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got := c.ftsContent.Load(); got != 1 {
		t.Fatalf("first index: expected 1 content FTS rebuild, got %d", got)
	}
	if matches, err := s.SearchFileContent("func A", "", false, 0); err != nil || len(matches) != 1 {
		t.Fatalf("first index search failed: err=%v matches=%d", err, len(matches))
	}

	// Unchanged re-index skips the content FTS rebuild.
	c.ftsContent.Store(0)
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("unchanged re-write: %v", err)
	}
	if got := c.ftsContent.Load(); got != 0 {
		t.Fatalf("unchanged re-index: expected 0 content FTS rebuilds, got %d", got)
	}
	if matches, err := s.SearchFileContent("func A", "", false, 0); err != nil || len(matches) != 1 {
		t.Fatalf("search after skipped rebuild failed: err=%v matches=%d", err, len(matches))
	}

	// Changed file content forces a rebuild.
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package ftstest\nfunc A() {} // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resChanged, filesChanged := parseModule(t, dir)
	c.ftsContent.Store(0)
	if err := s.Write(resChanged, filesChanged, nil); err != nil {
		t.Fatalf("changed write: %v", err)
	}
	if got := c.ftsContent.Load(); got != 1 {
		t.Fatalf("changed re-index: expected 1 content FTS rebuild, got %d", got)
	}
	if matches, err := s.SearchFileContent("changed", "", false, 0); err != nil || len(matches) != 1 {
		t.Fatalf("changed content not searchable: err=%v matches=%d", err, len(matches))
	}

	// Removing a file forces a rebuild and unindexes its content.
	if err := os.Remove(filepath.Join(dir, "b.go")); err != nil {
		t.Fatal(err)
	}
	resReduced, filesReduced := parseModule(t, dir)
	c.ftsContent.Store(0)
	if err := s.Write(resReduced, filesReduced, nil); err != nil {
		t.Fatalf("reduced write: %v", err)
	}
	if got := c.ftsContent.Load(); got != 1 {
		t.Fatalf("file-removal re-index: expected 1 content FTS rebuild, got %d", got)
	}
	if matches, err := s.SearchFileContent("func B", "", false, 0); err != nil || len(matches) != 0 {
		t.Fatalf("removed file still searchable: err=%v matches=%d", err, len(matches))
	}
}

// TestReplaceRepoSelectiveFTS verifies the spec's headline scenario: a
// ReplaceRepo whose file contents are byte-identical to the prior index does
// not rebuild the file-content FTS, while a content change does.
func TestReplaceRepoSelectiveFTS(t *testing.T) {
	dir := writeModuleNamed(t, "example.com/r", map[string]string{
		"one.go": "package r\nfunc One() {}\n",
	})
	res, files := parseModule(t, dir)

	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()

	repos := []RepoSpec{{ModulePath: "example.com/r", Dir: dir}}
	if err := s.WriteWorkspace(res, files, nil, repos); err != nil {
		t.Fatalf("workspace write: %v", err)
	}
	c.ftsContent.Store(0)
	if err := s.ReplaceRepo(res, files, nil, "example.com/r"); err != nil {
		t.Fatalf("unchanged ReplaceRepo: %v", err)
	}
	if got := c.ftsContent.Load(); got != 0 {
		t.Fatalf("unchanged ReplaceRepo: expected 0 content FTS rebuilds, got %d", got)
	}
	if matches, err := s.SearchFileContent("func One", "", false, 0); err != nil || len(matches) != 1 {
		t.Fatalf("search after skipped ReplaceRepo rebuild failed: err=%v matches=%d", err, len(matches))
	}

	// Changed content forces the rebuild.
	if err := os.WriteFile(filepath.Join(dir, "one.go"), []byte("package r\nfunc One() {} // new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resChanged, filesChanged := parseModule(t, dir)
	c.ftsContent.Store(0)
	if err := s.ReplaceRepo(resChanged, filesChanged, nil, "example.com/r"); err != nil {
		t.Fatalf("changed ReplaceRepo: %v", err)
	}
	if got := c.ftsContent.Load(); got != 1 {
		t.Fatalf("changed ReplaceRepo: expected 1 content FTS rebuild, got %d", got)
	}
	if matches, err := s.SearchFileContent("new", "", false, 0); err != nil || len(matches) != 1 {
		t.Fatalf("replaced content not searchable: err=%v matches=%d", err, len(matches))
	}
}

// TestReplaceRepoUnchangedKeepsFiles verifies the actual guarantee behind the
// skipped rebuild: unchanged file rows keep their identity, so search results
// remain correct after touching every write path.
func TestReplaceRepoUnchangedKeepsFiles(t *testing.T) {
	dir := writeModuleNamed(t, "example.com/r", map[string]string{
		"one.go": "package r\nfunc One() {}\n",
		"two.go": "package r\nfunc Two() {}\n",
	})
	res, files := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	repos := []RepoSpec{{ModulePath: "example.com/r", Dir: dir}}
	if err := s.WriteWorkspace(res, files, nil, repos); err != nil {
		t.Fatalf("workspace write: %v", err)
	}
	var fileCount int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM files").Scan(&fileCount); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		if err := s.ReplaceRepo(res, files, nil, "example.com/r"); err != nil {
			t.Fatalf("ReplaceRepo pass %d: %v", i, err)
		}
		var after int
		if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM files").Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != fileCount {
			t.Fatalf("pass %d: file count %d -> %d", i, fileCount, after)
		}
	}

	// Both files still searchable after repeated unchanged ReplaceRepos.
	for _, term := range []string{"func One", "func Two"} {
		matches, err := s.SearchFileContent(term, "", false, 0)
		if err != nil || len(matches) != 1 {
			t.Fatalf("search %q: err=%v matches=%d", term, err, len(matches))
		}
	}
}
