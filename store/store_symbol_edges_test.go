package store

import (
	"path/filepath"
	"testing"
)

// TestSymbolEdges verifies that SymbolEdges returns only edges whose endpoints
// are indexed symbols, restricted to the requested edge types, each attributed
// with its repo.
func TestSymbolEdges(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := t.Context()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	exec(`INSERT INTO repos (id, module_path, dir) VALUES (1, 'example.com/a', '/a'), (2, 'example.com/b', '/b')`)
	exec(`INSERT INTO packages (id, path, name, dir, repo_id) VALUES (1, 'example.com/a', 'a', '/a', 1), (2, 'example.com/b', 'b', '/b', 2)`)
	exec(`INSERT INTO symbols (id, qualified_name, package_id, name, kind, pos_file, pos_line, exported, repo_id) VALUES
		(1, 'example.com/a.Foo', 1, 'Foo', 'func', 'a.go', 1, 1, 1),
		(2, 'example.com/a.Bar', 1, 'Bar', 'func', 'a.go', 2, 1, 1),
		(3, 'example.com/b.Baz', 2, 'Baz', 'func', 'b.go', 1, 1, 2)`)
	exec(`INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, repo_id) VALUES
		('example.com/a.Foo', 'example.com/a.Bar', 'calls', 'a.go', 3, 1),
		('example.com/a.Bar', 'example.com/a.Foo', 'references', 'a.go', 4, 1),
		('example.com/b.Baz', 'example.com/a.Foo', 'satisfies', 'b.go', 2, 2),
		('example.com/a.Foo', 'example.com/b.Baz', 'imports', 'a.go', 5, 1),
		('example.com/a.Foo', 'missing.sym', 'calls', 'a.go', 6, 1),
		('missing.sym', 'example.com/a.Foo', 'calls', 'a.go', 7, 1)`)

	edges, err := s.SymbolEdges([]string{"calls", "references", "satisfies"})
	if err != nil {
		t.Fatalf("SymbolEdges: %v", err)
	}

	// imports edge excluded by type; endpoints referring to 'missing.sym'
	// excluded by the symbol-existence filter.
	want := map[string]bool{
		"example.com/a.Foo->example.com/a.Bar:calls":      true,
		"example.com/a.Bar->example.com/a.Foo:references": true,
		"example.com/b.Baz->example.com/a.Foo:satisfies":  true,
	}
	if len(edges) != len(want) {
		t.Fatalf("expected %d edges, got %d: %+v", len(want), len(edges), edges)
	}
	for _, e := range edges {
		key := e.FromRef + "->" + e.ToRef + ":" + e.EdgeType
		if !want[key] {
			t.Errorf("unexpected edge %q", key)
		}
		if e.Repo == "" {
			t.Errorf("edge %q expected repo attribution", key)
		}
	}

	got, err := s.SymbolEdges([]string{"calls"})
	if err != nil {
		t.Fatalf("SymbolEdges calls: %v", err)
	}
	// Only Foo->Bar qualifies; edges touching 'missing.sym' are filtered out.
	if len(got) != 1 || got[0].FromRef != "example.com/a.Foo" || got[0].ToRef != "example.com/a.Bar" {
		t.Errorf("expected only Foo->Bar call edge, got %+v", got)
	}

	empty, err := s.SymbolEdges(nil)
	if err != nil {
		t.Fatalf("SymbolEdges(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("SymbolEdges(nil) should return non-nil empty slice, got %v", empty)
	}
}
