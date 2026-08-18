package store

import (
	"context"
	"fmt"
	"testing"
)

// seedChainEdges builds a linear chain n0 -> n1 -> ... -> nDepth plus per-node
// fanout edges nK -> fK.0..fK.(fanout-1).
func seedChainEdges(t *testing.T, s *Store, depth, fanout int) {
	t.Helper()
	ctx := context.Background()
	for i := range depth {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, sites) VALUES (?, ?, 'calls', 'f.go', 1, '[]')`,
			fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1)); err != nil {
			t.Fatal(err)
		}
		for f := range fanout {
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, sites) VALUES (?, ?, 'calls', 'f.go', 1, '[]')`,
				fmt.Sprintf("n%d", i), fmt.Sprintf("f%d.%d", i, f)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestEdgesForNodesChunkedBounded verifies a frontier larger than the IN-chunk
// size is served by ceil(N/500) queries and returns every matching edge.
func TestEdgesForNodesChunkedBounded(t *testing.T) {
	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	const n = 1200
	for i := range n {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO edges (from_ref, to_ref, edge_type, pos_file, pos_line, sites) VALUES (?, 'target', 'calls', 'f.go', 1, '[]')`,
			fmt.Sprintf("caller%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	refs := make([]string, n)
	for i := range refs {
		refs[i] = fmt.Sprintf("caller%d", i)
	}

	c.prepares.Store(0)
	edges, err := s.EdgesForNodes(refs, nil, true) // outgoing from each caller
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != n {
		t.Fatalf("expected %d edges, got %d", n, len(edges))
	}
	if got := c.prepares.Load(); got != 3 {
		t.Fatalf("expected 3 chunked queries for %d refs (chunk 500), got %d", n, got)
	}
}

// TestEdgesForNodesMatchesEdgesFrom verifies the batched helper returns exactly
// the same edges as one EdgesFrom call per ref.
func TestEdgesForNodesMatchesEdgesFrom(t *testing.T) {
	s, _ := newCountingStore(t)
	defer func() { _ = s.Close() }()

	seedChainEdges(t, s, 4, 2)

	refs := []string{"n0", "n2", "n3"}
	batched, err := s.EdgesForNodes(refs, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, e := range batched {
		got[e.FromRef+":"+e.ToRef+":"+e.EdgeType] = true
	}

	want := make(map[string]bool)
	for _, r := range refs {
		perNode, err := s.EdgesFrom(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range perNode {
			want[e.FromRef+":"+e.ToRef+":"+e.EdgeType] = true
		}
	}

	for k := range want {
		if !got[k] {
			t.Errorf("batched missing edge %s", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("batched returned extra edge %s", k)
		}
	}
}

// TestTraversalQueryCountIsDepthBounded runs the level-batched traversal loop
// used by query.traverse and asserts the number of queries issued is O(depth),
// independent of graph fanout.
func TestTraversalQueryCountIsDepthBounded(t *testing.T) {
	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()

	// 8 levels; each node fans out to 50 extra callees. A per-node traversal
	// would issue 8*51 queries; the level-batched one issues 8.
	seedChainEdges(t, s, 8, 50)

	c.prepares.Store(0)

	const depth = 8
	visited := make(map[string]bool)
	queue := []string{"n0"}
	edgeCount := 0
	for d := 0; d < depth && len(queue) > 0; d++ {
		frontier := make([]string, 0, len(queue))
		for _, qn := range queue {
			if visited[qn] {
				continue
			}
			visited[qn] = true
			frontier = append(frontier, qn)
		}
		if len(frontier) == 0 {
			break
		}
		edges, err := s.EdgesForNodes(frontier, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		edgeCount += len(edges)
		for _, e := range edges {
			if !visited[e.ToRef] {
				queue = append(queue, e.ToRef)
			}
		}
	}

	if got := c.prepares.Load(); got != depth {
		t.Fatalf("level-batched traversal issued %d queries for depth %d, want %d (O(depth))", got, depth, depth)
	}
	// Every chain hop and every fanout edge was fetched exactly once.
	if want := 8 + 8*50; edgeCount != want {
		t.Fatalf("traversal fetched %d edges, want %d", edgeCount, want)
	}
}
