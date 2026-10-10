package store

import (
	"path/filepath"
	"testing"
)

// TestReachableFromEdgeCases covers the empty and unknown-root paths: the CTE
// seeds its result from the root list, so a root is always in its own
// reachable set whether or not the graph contains it.
func TestReachableFromEdgeCases(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.ReachableFrom(nil, []string{"calls"})
	if err != nil {
		t.Fatalf("nil roots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil roots = %v, want empty", got)
	}

	got, err = s.ReachableFrom([]string{"pkg.a"}, nil)
	if err != nil {
		t.Fatalf("nil edge types: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil edge types = %v, want empty", got)
	}

	got, err = s.ReachableFrom([]string{"pkg.nope"}, []string{"calls"})
	if err != nil {
		t.Fatalf("unknown root: %v", err)
	}
	if !got["pkg.nope"] || len(got) != 1 {
		t.Errorf("unknown root = %v, want only the root itself", got)
	}
}
