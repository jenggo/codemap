package query_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"codemap/extract"
	"codemap/query"
	"codemap/store"
)

// cycleSet renders cycles as a set of canonical paths for order-independent
// comparison.
func cycleSet(cycles []query.Cycle) map[string]bool {
	set := map[string]bool{}
	for _, c := range cycles {
		set[canonicalPath(c.Path)] = true
	}
	return set
}

// canonicalPath re-encodes a cycle path with its smallest node first so the
// test can compare cycles regardless of rotation.
func canonicalPath(path []string) string {
	if len(path) < 2 {
		return ""
	}
	n := len(path) - 1
	minIdx := 0
	for i := 1; i < n; i++ {
		if path[i] < path[minIdx] {
			minIdx = i
		}
	}
	out := make([]string, 0, n+1)
	for i := range n {
		out = append(out, path[(minIdx+i)%n])
	}
	out = append(out, out[0])
	return joinPath(out)
}

func joinPath(p []string) string {
	var b strings.Builder
	for i, s := range p {
		if i > 0 {
			b.WriteString("|")
		}
		b.WriteString(s)
	}
	return b.String()
}

func cycleGraphStore(t *testing.T, edges []extract.Edge) *store.Store {
	t.Helper()
	return buildGraphStore(t, map[string]map[string]string{
		"example.com/graph": {"a.go": "package graph\n\nfunc Main() {}\n"},
	}, edges, nil)
}

// TestDetectCyclesThreeCycleSCC: an SCC with three distinct 2-cycles reports
// all three exactly once each (spec: all cycles of an SCC are reported).
func TestDetectCyclesMultipleCycles(t *testing.T) {
	s := cycleGraphStore(t, []extract.Edge{
		symEdge("a", "b", "calls"),
		symEdge("b", "a", "calls"),
		symEdge("a", "c", "calls"),
		symEdge("c", "a", "calls"),
		symEdge("b", "c", "calls"),
	})

	cycles, err := query.DetectCycles(s, "calls")
	if err != nil {
		t.Fatalf("DetectCycles: %v", err)
	}

	set := cycleSet(cycles)
	want := map[string]bool{
		"a|b|a":   true,
		"a|c|a":   true,
		"a|b|c|a": true,
	}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("cycles mismatch:\n got: %v\nwant: %v\nall: %+v", set, want, cycles)
	}
}

// TestDetectCyclesCompleteTriangle: the complete digraph on 3 nodes has five
// distinct simple cycles (three 2-cycles and two 3-cycles), all reported once.
func TestDetectCyclesCompleteTriangle(t *testing.T) {
	s := cycleGraphStore(t, []extract.Edge{
		symEdge("a", "b", "calls"), symEdge("a", "c", "calls"),
		symEdge("b", "a", "calls"), symEdge("b", "c", "calls"),
		symEdge("c", "a", "calls"), symEdge("c", "b", "calls"),
	})

	cycles, err := query.DetectCycles(s, "calls")
	if err != nil {
		t.Fatalf("DetectCycles: %v", err)
	}

	set := cycleSet(cycles)
	want := map[string]bool{
		"a|b|a":   true,
		"a|c|a":   true,
		"b|c|b":   true,
		"a|b|c|a": true,
		"a|c|b|a": true,
	}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("cycles mismatch:\n got: %v\nwant: %v", set, want)
	}
}

// TestDetectCyclesDeterministic: repeated calls return identical results.
func TestDetectCyclesDeterministic(t *testing.T) {
	s := cycleGraphStore(t, []extract.Edge{
		symEdge("a", "b", "calls"),
		symEdge("b", "c", "calls"),
		symEdge("c", "a", "calls"),
		symEdge("c", "b", "calls"),
		symEdge("b", "a", "calls"),
		symEdge("d", "a", "calls"),
	})

	first, err := query.DetectCycles(s, "calls")
	if err != nil {
		t.Fatalf("DetectCycles: %v", err)
	}
	for i := range 5 {
		again, err := query.DetectCycles(s, "calls")
		if err != nil {
			t.Fatalf("DetectCycles: %v", err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("non-deterministic result on repeat %d:\n%+v\n%+v", i, first, again)
		}
	}
}

// TestDetectCyclesSelfLoop: a self-loop A→A is reported once.
func TestDetectCyclesSelfLoop(t *testing.T) {
	s := cycleGraphStore(t, []extract.Edge{
		symEdge("a", "a", "calls"),
		symEdge("a", "b", "calls"),
	})

	cycles, err := query.DetectCycles(s, "calls")
	if err != nil {
		t.Fatalf("DetectCycles: %v", err)
	}
	if len(cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %+v", cycles)
	}
	if cycles[0].Path[0] != "a" || cycles[0].Path[1] != "a" {
		t.Errorf("expected self-loop [a a], got %+v", cycles[0].Path)
	}
}

// TestDetectCyclesAcyclic: acyclic graphs return an empty, non-null list.
func TestDetectCyclesAcyclic(t *testing.T) {
	s := cycleGraphStore(t, []extract.Edge{
		symEdge("a", "b", "calls"),
		symEdge("b", "c", "calls"),
		symEdge("c", "d", "calls"),
	})

	cycles, err := query.DetectCycles(s, "calls")
	if err != nil {
		t.Fatalf("DetectCycles: %v", err)
	}
	if cycles == nil {
		t.Fatal("expected non-nil empty cycles")
	}
	if len(cycles) != 0 {
		t.Fatalf("expected 0 cycles, got %+v", cycles)
	}
	if data, err := json.Marshal(cycles); err != nil || string(data) != "[]" {
		t.Errorf("acyclic cycles should marshal to [], got %s (err %v)", data, err)
	}
}

// TestDetectCyclesCap: a complete digraph on 7 nodes has 2365 distinct cycles;
// results are capped at 1000 and the last element carries the truncated flag.
func TestDetectCyclesCap(t *testing.T) {
	nodes := []string{"a", "b", "c", "d", "e", "f", "g"}
	var edges []extract.Edge
	for _, from := range nodes {
		for _, to := range nodes {
			if from != to {
				edges = append(edges, symEdge(from, to, "calls"))
			}
		}
	}

	s := cycleGraphStore(t, edges)
	cycles, err := query.DetectCycles(s, "calls")
	if err != nil {
		t.Fatalf("DetectCycles: %v", err)
	}
	if len(cycles) != 1000 {
		t.Errorf("expected cap of 1000 cycles, got %d", len(cycles))
	}
	if !cycles[len(cycles)-1].Truncated {
		t.Error("expected last cycle to carry the truncated flag")
	}
}
