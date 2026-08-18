package contract_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/contract"
	"codemap/store"
)

// TestReanalyzeStaleContractsPropagatesStalenessError verifies a backend read
// failure during the staleness sweep surfaces to the caller instead of
// silently marking the repo non-stale.
func TestReanalyzeStaleContractsPropagatesStalenessError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.EnsureRepo(store.RepoSpec{ModulePath: "example.com/a", Dir: filepath.Join(t.TempDir(), "a")}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoIndexedAt("example.com/a", time.Now()); err != nil {
		t.Fatal(err)
	}

	// Corrupt the member's indexed_at timestamp so RepoIndexedAt (and therefore
	// IsContractStale) fails while every other read still works.
	other, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(context.Background(), `UPDATE repos SET indexed_at = 'garbage' WHERE module_path = 'example.com/a'`); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = contract.ReanalyzeStaleContracts(s, contract.Config{})
	if err == nil {
		t.Fatal("expected the staleness-read error to propagate, not be swallowed")
	}
	if !strings.Contains(err.Error(), "staleness check") {
		t.Fatalf("expected a staleness-check error, got: %v", err)
	}
}
