package mcp

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

// newGeneratedTestServer indexes the generated-code fixture so the search tool's
// generated ranking and filter can be exercised through the tool surface.
func newGeneratedTestServer(t *testing.T) *Server {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fixtures/generated")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	parseResult, err := parse.Run(abs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := st.SetIndexedAt(time.Now()); err != nil {
		t.Fatalf("set indexed_at: %v", err)
	}
	if err := st.SetRepoMeta(abs, "", len(resolveResult.Packages), len(resolveResult.Symbols)); err != nil {
		t.Fatalf("set repo meta: %v", err)
	}
	return &Server{store: st, dbPath: dbPath, repoPath: abs, usage: newUsageStats()}
}

func TestSearchToolGeneratedArgument(t *testing.T) {
	s := newGeneratedTestServer(t)
	pattern := "example.com/generated"

	only, isErr := s.handleTool("search", map[string]any{"pattern": pattern, "generated": "only"})
	if isErr {
		t.Fatalf("generated=only search failed: %s", only)
	}
	if !strings.Contains(only, "AlphaAuto") {
		t.Errorf("generated=only must return the generated symbol, got %s", only)
	}
	if strings.Contains(only, "Zulu") {
		t.Errorf("generated=only must drop hand-written symbols, got %s", only)
	}

	excluded, isErr := s.handleTool("search", map[string]any{"pattern": pattern, "generated": "exclude"})
	if isErr {
		t.Fatalf("generated=exclude search failed: %s", excluded)
	}
	if strings.Contains(excluded, "AlphaAuto") {
		t.Errorf("generated=exclude must drop the generated symbol, got %s", excluded)
	}
	if !strings.Contains(excluded, "Zulu") {
		t.Errorf("generated=exclude must keep hand-written symbols, got %s", excluded)
	}
}

// TestSearchToolRanksGeneratedLast verifies the default search keeps generated
// symbols (down-ranked, not hidden) and exposes the generated marker.
func TestSearchToolRanksGeneratedLast(t *testing.T) {
	s := newGeneratedTestServer(t)

	out, isErr := s.handleTool("search", map[string]any{"pattern": "example.com/generated"})
	if isErr {
		t.Fatalf("default search failed: %s", out)
	}
	zulu := strings.Index(out, "Zulu")
	alpha := strings.Index(out, "AlphaAuto")
	if zulu < 0 || alpha < 0 {
		t.Fatalf("default search must return both symbols, got %s", out)
	}
	if zulu > alpha {
		t.Errorf("hand-written Zulu must rank before generated AlphaAuto, got %s", out)
	}
	if !strings.Contains(out, "Generated") {
		t.Errorf("search output must expose the generated flag, got %s", out)
	}
}
