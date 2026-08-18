package resolve

import (
	"path/filepath"
	"strconv"
	"testing"

	"codemap/parse"
)

// TestNoCallReferencesDoubling verifies the direct-call scenario: a callee
// expression inside a call emits only `calls`, while a method value and field
// access still emit `references`.
func TestNoCallReferencesDoubling(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "dedup"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	key := func(to, ty string, line int) string {
		return to + "|" + ty + "|" + strconv.Itoa(line)
	}
	keys := map[string]bool{}
	for _, e := range res.Edges {
		keys[key(e.Edge.ToRef, e.Edge.EdgeType, e.Edge.Pos.Line)] = true
	}

	const caller = "samepair.Caller"

	// Direct calls emit only `calls` — no `references` for the same callee/line.
	for _, tc := range []struct {
		to   string
		line int
	}{
		{"samepair.NewService", 4},
		{"samepair.Service.Get", 5},
		{"samepair.Global", 9},
	} {
		if !keys[key(tc.to, "calls", tc.line)] {
			t.Errorf("expected calls edge for %s at line %d", tc.to, tc.line)
		}
		if keys[key(tc.to, "references", tc.line)] {
			t.Errorf("direct call at line %d must not emit a references edge for %s", tc.line, tc.to)
		}
	}

	// Method value `f := svc.Get` (line 6) still emits `references`.
	if !keys[key("samepair.Service.Get", "references", 6)] {
		t.Error("method value (line 6) should emit a references edge")
	}

	// Unrelated field access `svc.x` (line 8) still emits `references`.
	if !keys[key("samepair.x", "references", 8)] {
		t.Error("field access (line 8) should emit a references edge")
	}
	if keys[key("samepair.x", "calls", 8)] {
		t.Error("field access must not emit a calls edge")
	}

	// The Caller function must not double-emit any edge pair at the same site.
	seen := map[string]bool{}
	for _, e := range res.Edges {
		if e.Edge.FromRef != caller {
			continue
		}
		k := key(e.Edge.ToRef, e.Edge.EdgeType, e.Edge.Pos.Line)
		if seen[k] {
			t.Errorf("duplicate edge emitted: %s", k)
		}
		seen[k] = true
	}
}

// TestMethodValueAndFieldAccessStillReferenced ensures non-call selector uses
// are untouched by the dedup change: the fixture's reference edges carry no
// calls counterpart.
func TestMethodValueAndFieldAccessStillReferenced(t *testing.T) {
	pr, err := parse.Run(filepath.Join("..", "testdata", "fixtures", "dedup"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(pr)

	var methodValueRefs, fieldRefs int
	for _, e := range res.Edges {
		if e.Edge.EdgeType != "references" {
			continue
		}
		if e.Edge.ToRef == "samepair.Service.Get" {
			methodValueRefs++
		}
		if e.Edge.ToRef == "samepair.x" {
			fieldRefs++
		}
	}
	if methodValueRefs != 1 {
		t.Errorf("expected 1 method-value reference edge (use.go:6 `f := svc.Get`), got %d", methodValueRefs)
	}
	if fieldRefs != 2 {
		t.Errorf("expected 2 field-access reference edges (lib.go:7 + use.go:8), got %d", fieldRefs)
	}
}
