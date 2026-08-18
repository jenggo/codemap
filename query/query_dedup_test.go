package query_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

// setupFixtureStore indexes an arbitrary fixture directory into a fresh store.
func setupFixtureStore(t *testing.T, absPath string) *store.Store {
	t.Helper()
	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

// setupMultiSiteStore indexes the multisite fixture, which calls Target from
// four lines inside Helper and once from Other.
func setupMultiSiteStore(t *testing.T) *store.Store {
	t.Helper()
	absPath, err := filepath.Abs("../testdata/fixtures/multisite")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)
	return s
}

// TestCallersOfMultiSiteCount verifies one deduplicated caller edge per caller
// with the aggregated site count and distinct sites.
func TestCallersOfMultiSiteCount(t *testing.T) {
	s := setupMultiSiteStore(t)

	callers, err := query.CallersOf(s, "multisite.Target")
	if err != nil {
		t.Fatal(err)
	}

	// Only calls edges to Target; each distinct caller => one edge.
	var helper, other *query.EdgeDetail
	for i := range callers {
		e := &callers[i]
		if e.FromRef == "multisite.Helper" && e.EdgeType == "calls" {
			helper = e
		}
		if e.FromRef == "multisite.Other" && e.EdgeType == "calls" {
			other = e
		}
	}
	if helper == nil || other == nil {
		t.Fatalf("expected caller edges from Helper and Other, got %+v", callers)
	}

	if helper.SiteCount != 4 {
		t.Errorf("Helper calls Target 4 times: expected SiteCount 4, got %d", helper.SiteCount)
	}
	if len(helper.Sites) != 4 {
		t.Errorf("expected 4 aggregated sites, got %d: %+v", len(helper.Sites), helper.Sites)
	}
	if other.SiteCount != 1 {
		t.Errorf("Other calls Target once: expected SiteCount 1, got %d", other.SiteCount)
	}
	if len(other.Sites) != 1 {
		t.Errorf("expected 1 site for Other, got %d", len(other.Sites))
	}
	// Each site carries the file and a distinct line.
	seen := map[int]bool{}
	for _, s := range helper.Sites {
		if s.File == "" {
			t.Error("site missing file")
		}
		if seen[s.Line] {
			t.Errorf("duplicate site line %d", s.Line)
		}
		seen[s.Line] = true
	}
	// Primary position equals the first site.
	if len(helper.Sites) > 0 && helper.PosFile == "" {
		t.Error("expected primary pos_file set to first site")
	}
}

// TestEdgeJSONShapeIncludesSites verifies the marshalled JSON exposes sites and
// site_count alongside the primary position fields.
func TestEdgeJSONShapeIncludesSites(t *testing.T) {
	s := setupMultiSiteStore(t)

	callees, err := query.CalleesOf(s, "multisite.Helper")
	if err != nil {
		t.Fatal(err)
	}
	var target *query.EdgeDetail
	for i := range callees {
		if callees[i].ToRef == "multisite.Target" {
			target = &callees[i]
		}
	}
	if target == nil {
		t.Fatalf("expected callee edge to Target, got %+v", callees)
	}

	data, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	sites, ok := decoded["sites"].([]any)
	if !ok {
		t.Fatalf("expected sites array in JSON, got %v", decoded["sites"])
	}
	if len(sites) != 4 {
		t.Errorf("expected 4 sites in JSON, got %d", len(sites))
	}
	sc, ok := decoded["site_count"].(float64)
	if !ok || int(sc) != 4 {
		t.Errorf("expected site_count 4 in JSON, got %v", decoded["site_count"])
	}
	if decoded["pos_line"] == nil {
		t.Error("expected primary pos_line in JSON")
	}
}

// TestBlastRadiusDeduplicated verifies blast radius counts unique edges, not
// raw occurrences: Helper's four call sites count once.
func TestBlastRadiusDeduplicated(t *testing.T) {
	s := setupMultiSiteStore(t)

	br, err := query.GetBlastRadius(s, "multisite.Target", 3)
	if err != nil {
		t.Fatal(err)
	}
	if br.DirectCallers != 2 {
		t.Errorf("expected 2 direct callers (Helper + Other), got %d", br.DirectCallers)
	}
	if br.TransitiveCallers < 2 {
		t.Errorf("expected transitive callers >= 2, got %d", br.TransitiveCallers)
	}
}

// TestShowReturnsSites verifies Show exposes sites/site_count on edges.
func TestShowReturnsSites(t *testing.T) {
	s := setupMultiSiteStore(t)

	res, err := query.Show(s, "multisite.Target")
	if err != nil {
		t.Fatal(err)
	}
	var helperEdge *query.EdgeDetail
	for i := range res.IncomingEdges {
		if res.IncomingEdges[i].FromRef == "multisite.Helper" {
			helperEdge = &res.IncomingEdges[i]
		}
	}
	if helperEdge == nil {
		t.Fatal("expected incoming edge from Helper")
	}
	if helperEdge.SiteCount != 4 {
		t.Errorf("expected SiteCount 4 on show edge, got %d", helperEdge.SiteCount)
	}
	if len(helperEdge.Sites) != 4 {
		t.Errorf("expected 4 sites on show edge, got %d", len(helperEdge.Sites))
	}
}

// TestEdgesByTypeReturnsSites verifies edges_by_type rows expose sites.
func TestEdgesByTypeReturnsSites(t *testing.T) {
	s := setupMultiSiteStore(t)

	edges, err := query.EdgesByType(s, "calls")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.FromRef == "multisite.Helper" && e.ToRef == "multisite.Target" {
			if e.SiteCount != 4 {
				t.Errorf("expected SiteCount 4 via EdgesByType, got %d", e.SiteCount)
			}
		}
	}
}
