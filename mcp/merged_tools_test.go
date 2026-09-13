package mcp

import (
	"strings"
	"testing"
)

func TestImportsOfDirectionFlags(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"out default", map[string]any{"package_path": "codemap/testdata/simple"}},
		{"in", map[string]any{"package_path": "codemap/testdata/simple", "direction": "in"}},
		{"transitive", map[string]any{"package_path": "codemap/testdata/simple", "transitive": true}},
	} {
		out, isErr := s.handleTool("imports_of", tc.args)
		if isErr {
			t.Errorf("%s: imports_of failed: %s", tc.name, out)
		}
	}
	out, isErr := s.handleTool("imports_of", map[string]any{"package_path": "codemap/testdata/simple", "direction": "both"})
	if !isErr || !strings.Contains(out, "direction must be one of [in, out]") {
		t.Errorf("expected direction validation error, got isErr=%v out=%q", isErr, out)
	}
}

func TestSearchMergedModes(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("search", map[string]any{"pattern": "Parse", "mode": "method"})
	if isErr || !strings.Contains(out, "Parse") {
		t.Errorf("method mode: isErr=%v out=%q", isErr, out)
	}
	out, isErr = s.handleTool("search", map[string]any{"pattern": "codemap/testdata/simple.", "mode": "prefix"})
	if isErr || !strings.Contains(out, "NewParser") {
		t.Errorf("prefix mode: isErr=%v out=%q", isErr, out)
	}
	out, isErr = s.handleTool("search", map[string]any{"pattern": "Parser"})
	if isErr || !strings.Contains(out, "NewParser") {
		t.Errorf("substring default: isErr=%v out=%q", isErr, out)
	}
}

func TestShowSourceMode(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("show", map[string]any{"qualified_name": "codemap/testdata/simple.NewParser", "source": true})
	if isErr || !strings.Contains(out, "func NewParser") {
		t.Fatalf("source mode: isErr=%v out=%q", isErr, out)
	}
	if !strings.Contains(out, "NewParser creates a new Parser.") {
		t.Errorf("source mode default include_doc=true should carry the doc comment, got: %s", out)
	}
	out, isErr = s.handleTool("show", map[string]any{"qualified_name": "codemap/testdata/simple.NewParser", "source": true, "include_doc": false})
	if isErr || strings.Contains(out, "NewParser creates") {
		t.Errorf("include_doc=false: isErr=%v out=%q", isErr, out)
	}
}

func TestPackageWithoutPathListsPackages(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("package", map[string]any{})
	if isErr || !strings.Contains(out, "codemap/testdata/simple") {
		t.Errorf("list mode: isErr=%v out=%q", isErr, out)
	}
	out, isErr = s.handleTool("package", map[string]any{"path": "codemap/testdata/simple"})
	if isErr || !strings.Contains(out, "NewParser") {
		t.Errorf("package mode: isErr=%v out=%q", isErr, out)
	}
}

func TestAllEdgesOptionalTypeFilter(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("all_edges", map[string]any{})
	if isErr {
		t.Errorf("unfiltered: %s", out)
	}
	out, isErr = s.handleTool("all_edges", map[string]any{"edge_type": "references"})
	if isErr {
		t.Errorf("filtered: %s", out)
	}
}

func TestHotspotsRankModes(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("get_hotspots", map[string]any{"mode": "pagerank"})
	if isErr {
		t.Errorf("pagerank mode: %s", out)
	}
	out, isErr = s.handleTool("get_hotspots", map[string]any{})
	if isErr {
		t.Errorf("churn default: %s", out)
	}
}

func TestDependencyFlowLayersFlag(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("dependency_flow", map[string]any{"layers": true})
	if isErr {
		t.Errorf("layers mode: %s", out)
	}
	out, isErr = s.handleTool("dependency_flow", map[string]any{"package_path": "codemap/testdata/simple"})
	if isErr {
		t.Errorf("flow mode: %s", out)
	}
	out, isErr = s.handleTool("dependency_flow", map[string]any{})
	if !isErr || !strings.Contains(out, "package_path is required") {
		t.Errorf("flow without path: isErr=%v out=%q", isErr, out)
	}
}
