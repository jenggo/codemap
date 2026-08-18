package store

import (
	"fmt"
	"testing"
)

// TestImportFrontierBFSCountsEachEdgeOnce verifies the shared BFS examines each
// adjacency edge exactly once, even on a dense graph where a per-node rescan of
// the full edge list would visit O(V·E) entries.
func TestImportFrontierBFSCountsEachEdgeOnce(t *testing.T) {
	const n = 200
	edges := make([]Edge, 0, n*(n-1)/2)
	for i := range n {
		// Chain edge guarantees every later node is reachable from p0.
		edges = append(edges, Edge{
			FromRef:  fmt.Sprintf("p%d", i),
			ToRef:    fmt.Sprintf("p%d", i+1),
			EdgeType: edgeTypeImports,
		})
		// Fan-out to every later node makes the graph dense.
		for j := i + 2; j < n; j++ {
			edges = append(edges, Edge{
				FromRef:  fmt.Sprintf("p%d", i),
				ToRef:    fmt.Sprintf("p%d", j),
				EdgeType: edgeTypeImports,
			})
		}
	}

	adj := BuildImportAdjacency(edges)

	visits := 0
	result := ImportFrontierBFS(adj, "p0", func(e Edge, _ map[string]bool) string {
		visits++
		return e.ToRef
	})

	if visits != len(edges) {
		t.Fatalf("BFS examined each edge %d times worth of entries: %d visits for %d edges; want exactly one visit per edge",
			visits/len(edges), visits, len(edges))
	}
	// Every edge is reachable via the chain, so every edge is in the result.
	if len(result) != len(edges) {
		t.Fatalf("BFS returned %d edges, want %d", len(result), len(edges))
	}
}

// TestImportFrontierBFSNonImportsExcluded verifies the adjacency builder drops
// non-import edges so the walk never crosses calls/references.
func TestImportFrontierBFSNonImportsExcluded(t *testing.T) {
	edges := []Edge{
		{FromRef: "a", ToRef: "b", EdgeType: edgeTypeImports},
		{FromRef: "a", ToRef: "c", EdgeType: "calls"},
	}
	adj := BuildImportAdjacency(edges)
	result := ImportFrontierBFS(adj, "a", func(e Edge, _ map[string]bool) string { return e.ToRef })
	if len(result) != 1 || result[0].ToRef != "b" {
		t.Fatalf("expected only the imports edge a->b, got %+v", result)
	}
}
