package query_test

import (
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
