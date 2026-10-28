package query_test

import (
	"os"
	"path/filepath"
	"testing"

	"codemap/query"
	"codemap/store"
)

func summaryStore(t *testing.T) *store.Store {
	t.Helper()
	s := setupStore(t)
	absPath, err := filepath.Abs("../testdata/simple")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if err := s.SetRepoMeta(absPath, "", 1, 1); err != nil {
		t.Fatalf("repo meta: %v", err)
	}
	return s
}

func TestSummaryReportsModuleAndPackages(t *testing.T) {
	s := summaryStore(t)
	result, err := query.Summary(s)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if result.Module != "codemap/testdata/simple" {
		t.Errorf("module = %q, want codemap/testdata/simple", result.Module)
	}
	if len(result.Packages) == 0 {
		t.Fatal("expected at least one package")
	}
	if result.TotalSymbols == 0 {
		t.Error("expected non-zero symbol count")
	}
	if result.TotalEdges == 0 {
		t.Error("expected non-zero edge count")
	}
}

func TestSummaryPackagesSortedBySize(t *testing.T) {
	s := summaryStore(t)
	result, err := query.Summary(s)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	for i := 1; i < len(result.Packages); i++ {
		if result.Packages[i-1].SymCount < result.Packages[i].SymCount {
			t.Fatalf("packages not sorted by size: %+v", result.Packages)
		}
	}
}

func TestSummaryCoversEveryPackage(t *testing.T) {
	s := summaryStore(t)
	summary, err := query.Summary(s)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	overview, err := query.Overview(s)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(summary.Packages) != len(overview.Packages) {
		t.Errorf("summary should cover every package: %d vs %d", len(summary.Packages), len(overview.Packages))
	}
	for _, p := range summary.Packages {
		if len(p.Doc) > 120 {
			t.Errorf("package doc too long for a summary: %d chars", len(p.Doc))
		}
	}
}

// TestRepoDescriptionReadsReadmeParagraph pins that summary can say what the
// repo is. Package docs are not a substitute: the main package usually has none,
// so "what is this repo" was answerable only by opening a file.
func TestRepoDescriptionReadsReadmeParagraph(t *testing.T) {
	dir := t.TempDir()
	readme := "# Widget Service\n\n[![CI](https://example.com/badge.svg)](https://example.com)\n\n" +
		"Widget Service tracks widgets across regions and exposes a small HTTP API.\n" +
		"It is deployed as a single binary.\n\n## Features\n\n- nothing\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}

	got := query.RepoDescription(dir)
	want := "Widget Service tracks widgets across regions and exposes a small HTTP API. It is deployed as a single binary."
	if got != want {
		t.Fatalf("repoDescription = %q, want %q", got, want)
	}
}

// TestRepoDescriptionMissingReadme pins that a repo without a README simply
// carries no description rather than failing the whole summary.
func TestRepoDescriptionMissingReadme(t *testing.T) {
	if got := query.RepoDescription(t.TempDir()); got != "" {
		t.Fatalf("repoDescription without README = %q, want empty", got)
	}
}
