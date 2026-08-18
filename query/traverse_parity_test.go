package query

import (
	"sort"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

// legacyTraverse reproduces the pre-batching per-node traversal exactly:
// one EdgesTo/EdgesFrom fetch per dequeued node, in queue order.
func legacyTraverse(s *store.Store, start string, outgoing bool, depth int, allowTypes []string) ([]store.Edge, error) {
	if depth <= 0 {
		depth = 1
	}
	visited := make(map[string]bool)
	result := []store.Edge{}
	queue := []string{start}
	for d := 0; d < depth && len(queue) > 0; d++ {
		nextQueue := []string{}
		for _, qn := range queue {
			if visited[qn] {
				continue
			}
			visited[qn] = true

			var edges []store.Edge
			var err error
			if outgoing {
				edges, err = s.EdgesFrom(qn)
			} else {
				edges, err = s.EdgesTo(qn)
			}
			if err != nil {
				return nil, err
			}
			for _, e := range edges {
				if edgeTypeAllowed(e.EdgeType, allowTypes) {
					result = append(result, e)
					next := e.ToRef
					if !outgoing {
						next = e.FromRef
					}
					nextQueue = append(nextQueue, next)
				}
			}
		}
		queue = nextQueue
	}
	return result, nil
}

func edgeSet(edges []store.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.FromRef+"|"+e.ToRef+"|"+e.EdgeType)
	}
	sort.Strings(out)
	return out
}

// TestTraverseMatchesLegacyPerNode verifies the level-batched traversal returns
// the identical edge set to the previous per-node implementation on a deep,
// fanout-rich fixture.
func TestTraverseMatchesLegacyPerNode(t *testing.T) {
	fixtureDirs := []string{"dedup", "multisite"}
	for _, dir := range fixtureDirs {
		pr, err := parse.Run("../testdata/fixtures/" + dir)
		if err != nil {
			t.Fatalf("%s: parse: %v", dir, err)
		}
		res := resolve.Run(pr)

		s, err := store.Create(t.TempDir() + "/test.db")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if err := s.Write(res, nil, nil); err != nil {
			t.Fatal(err)
		}

		// Exercise callers, callees, shallow and deep, and edge-type filtering.
		cases := []struct {
			start      string
			outgoing   bool
			depth      int
			allowTypes []string
		}{
			{"multisite.Target", false, 1, nil},
			{"multisite.Helper", true, 2, nil},
			{"samepair.Caller", false, 3, []string{"calls"}},
			{"samepair.NewService", true, 4, []string{"calls", "references"}},
		}
		for _, c := range cases {
			got, err := traverse(s, c.start, c.outgoing, c.depth, c.allowTypes)
			if err != nil {
				t.Fatalf("traverse(%s): %v", c.start, err)
			}
			want, err := legacyTraverse(s, c.start, c.outgoing, c.depth, c.allowTypes)
			if err != nil {
				t.Fatalf("legacyTraverse(%s): %v", c.start, err)
			}
			gotSet := edgeSet(edgesFromDetails(got))
			wantSet := edgeSet(want)
			if len(gotSet) != len(wantSet) {
				t.Fatalf("%s traverse depth=%d outgoing=%v: %d edges, legacy %d", c.start, c.depth, c.outgoing, len(gotSet), len(wantSet))
			}
			for i := range gotSet {
				if gotSet[i] != wantSet[i] {
					t.Fatalf("%s traverse depth=%d: mismatch at %d: got %q want %q", c.start, c.depth, i, gotSet[i], wantSet[i])
				}
			}
		}
	}
}

func edgesFromDetails(details []EdgeDetail) []store.Edge {
	out := make([]store.Edge, 0, len(details))
	for _, d := range details {
		out = append(out, store.Edge{
			FromRef:   d.FromRef,
			ToRef:     d.ToRef,
			EdgeType:  d.EdgeType,
			PosFile:   d.PosFile,
			PosLine:   d.PosLine,
			SiteCount: d.SiteCount,
		})
	}
	return out
}
