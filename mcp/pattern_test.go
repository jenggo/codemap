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

func newPatternTestServer(t *testing.T) *Server {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fixtures/pattern")
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
	if err := st.Write(resolveResult, parse.FileContents(parseResult), nil); err != nil {
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

func TestPatternToolFindsMatch(t *testing.T) {
	s := newPatternTestServer(t)
	out, isErr := s.handleTool("pattern", map[string]any{"pattern": "defer $CALL"})
	if isErr {
		t.Fatalf("pattern tool failed: %s", out)
	}
	if !strings.Contains(out, "example.com/pattern.Handle") {
		t.Errorf("output must name the enclosing symbol, got:\n%s", out)
	}
	if !strings.Contains(out, `defer fmt.Println("done")`) {
		t.Errorf("output must include the matched source, got:\n%s", out)
	}
}

func TestPatternToolRejectsInvalidPattern(t *testing.T) {
	s := newPatternTestServer(t)
	out, isErr := s.handleTool("pattern", map[string]any{"pattern": "if err != nil {"})
	if !isErr {
		t.Fatalf("invalid pattern must be an error, got: %s", out)
	}
	if !strings.Contains(out, "invalid pattern") {
		t.Errorf("error must explain the compile failure, got: %s", out)
	}
}

func TestPatternToolRequiresPattern(t *testing.T) {
	s := newPatternTestServer(t)
	if out, isErr := s.handleTool("pattern", map[string]any{}); !isErr {
		t.Fatalf("missing pattern must be an error, got: %s", out)
	}
}

func TestPatternToolIsListed(t *testing.T) {
	for _, tool := range buildToolsList() {
		if tool["name"] == "pattern" {
			return
		}
	}
	t.Fatal("the pattern tool must be listed in tools/list")
}
