package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/query"
	"codemap/render"
	"codemap/store"
)

// toolResult dispatches a raw line through a fresh dispatch state and returns
// the parsed JSON-RPC response.
func toolResult(t *testing.T, s *Server, line string) map[string]any {
	t.Helper()
	out := captureStdout(func() {
		s.dispatchLine(line)
	})
	return parseResponse(t, out)
}

// toolResultRun dispatches a raw line through the panic-guarded path and
// returns the parsed JSON-RPC response.
func toolResultRun(t *testing.T, s *Server, line string) map[string]any {
	t.Helper()
	out := captureStdout(func() {
		s.runLine(line)
	})
	t.Logf("runLine output: %s", out)
	return parseResponse(t, out)
}

// resultText extracts the tool result's text content from a tools/call
// response, or "" when the response carries no result object.
func resultText(t *testing.T, resp map[string]any) string {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		return ""
	}
	content, _ := res["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			if txt, ok := m["text"].(string); ok {
				return txt
			}
		}
	}
	return ""
}

// --- 1. Panic isolation ---

func TestHandlerPanicReturnsErrorAndServerSurvives(t *testing.T) {
	s := newTestServer(t)
	s.handlers = queryHandlers()
	s.handlers["boom"] = func(*store.Store, []query.Option, []render.Option, map[string]any) (string, bool) {
		panic("kaboom")
	}

	resp := toolResultRun(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom","arguments":{}}}`)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error after handler panic, got: %v", resp)
	}
	if code, _ := errObj["code"].(float64); code != -32603 {
		t.Fatalf("expected error code -32603, got %v", errObj)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "kaboom") {
		t.Fatalf("expected panic value in error message, got %q", msg)
	}

	// A subsequent ping must still succeed: the loop survived.
	resp = toolResult(t, s, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if _, hasResult := resp["result"]; !hasResult {
		t.Fatalf("ping after panic failed: %v", resp)
	}
}

func TestPanicInNotificationGetsNoResponse(t *testing.T) {
	s := newTestServer(t)
	s.handlers = queryHandlers()
	s.handlers["boom"] = func(*store.Store, []query.Option, []render.Option, map[string]any) (string, bool) {
		panic("kaboom")
	}
	out := captureStdout(func() {
		s.runLine(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"boom","arguments":{}}}`)
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("notification panic should produce no response, got %q", out)
	}
}

func TestMalformedMethodJSONReturnsParseError(t *testing.T) {
	s := newTestServer(t)
	resp := toolResult(t, s, `{"jsonrpc":"2.0","id":1,"method":5}`)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected parse error, got: %v", resp)
	}
	if code, _ := errObj["code"].(float64); code != -32700 {
		t.Fatalf("expected error code -32700, got %v", errObj)
	}
}

// --- 2. Freshness TTL cache ---

func TestFreshnessCacheWithinTTL(t *testing.T) {
	s := newTestServer(t)
	var checks int
	s.stalenessCheck = func(dbPath, repoPath string) (bool, error) {
		checks++
		return false, nil
	}
	s.freshnessTTL = time.Minute

	if _, err := s.getStore(); err != nil {
		t.Fatalf("first getStore: %v", err)
	}
	if _, err := s.getStore(); err != nil {
		t.Fatalf("second getStore: %v", err)
	}
	if checks != 1 {
		t.Fatalf("expected a single staleness check within the TTL, got %d", checks)
	}

	// Expire the cached entry and confirm the next call rechecks.
	s.freshness["single:"+s.repoPath] = freshnessEntry{checkedAt: time.Now().Add(-2 * time.Minute)}
	if _, err := s.getStore(); err != nil {
		t.Fatalf("getStore after TTL expiry: %v", err)
	}
	if checks != 2 {
		t.Fatalf("expected recheck after TTL expiry, got %d checks", checks)
	}
}

func TestStalenessErrorPropagates(t *testing.T) {
	s := newTestServer(t)
	s.stalenessCheck = func(dbPath, repoPath string) (bool, error) {
		return false, errors.New("db corrupt")
	}
	_, err := s.getStore()
	if err == nil || !strings.Contains(err.Error(), "db corrupt") {
		t.Fatalf("expected staleness error to propagate, got: %v", err)
	}
}

// --- 3. Argument validation ---

func TestArgumentValidationRejectsBadInput(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		line string
		want string
	}{
		{"depth too large", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"callers_of","arguments":{"qualified_name":"x","depth":1000000}}}`, "depth must be between 1 and 100"},
		{"max_depth too large", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"find_path","arguments":{"from":"a","to":"b","max_depth":1000000}}}`, "max_depth must be between 1 and 100"},
		{"negative context_lines", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":"x","context_lines":-1}}}`, "context_lines must be between 0 and 500"},
		{"empty search_text pattern", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":""}}}`, "pattern is required"},
		{"symbol body negative context", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_symbol_body","arguments":{"qualified_name":"fmt.Println","context_lines":-5}}}`, "context_lines must be between 0 and 500"},
		{"bad token_budget", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_context_bundle","arguments":{"qualified_name":"fmt.Println","token_budget":0}}}`, "token_budget must be between 1 and 1000000"},
		{"top_n too large", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_hotspots","arguments":{"top_n":10000}}}`, "top_n must be between 1 and 500"},
		{"min_confidence out of range", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"contracts","arguments":{"min_confidence":2}}}`, "min_confidence must be between 0 and 1"},
		{"unknown severity", `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"contracts","arguments":{"severity":"catastrophic"}}}`, "severity must be one of"},
		{"unknown direction", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"contracts","arguments":{"direction":"both"}}}`, "direction must be one of"},
		{"unknown edge_type", `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"edges_by_type","arguments":{"edge_type":"bogus"}}}`, "edge_type must be one of"},
		{"unknown symbol kind", `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"search","arguments":{"pattern":"x","kind":"banana"}}}`, "kind must be one of"},
		{"unknown runtime kind", `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"runtime_contracts","arguments":{"kind":"kafka"}}}`, "kind must be one of"},
		{"unknown heuristic", `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"entry_points","arguments":{"heuristics":["shadow"]}}}`, "heuristics must be one of"},
		{"bad edge_types entry", `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"callers_of","arguments":{"qualified_name":"x","edge_types":["calls","nope"]}}}`, "edge_types must be one of"},
	}
	for _, tc := range cases {
		resp := toolResult(t, s, tc.line)
		text := resultText(t, resp)
		if !strings.Contains(text, "Error:") {
			t.Errorf("%s: expected an error result, got %q", tc.name, text)
			continue
		}
		if !strings.Contains(text, tc.want) {
			t.Errorf("%s: expected %q in error, got %q", tc.name, tc.want, text)
		}
	}
}

func TestArgumentValidationStillAllowsValidInput(t *testing.T) {
	s := newTestServer(t)
	// Boundary top_n (max) and depth (max) pass validation.
	resp := toolResult(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_hotspots","arguments":{"top_n":500}}}`)
	if text := resultText(t, resp); strings.Contains(text, "must be between") {
		t.Fatalf("valid top_n rejected: %q", text)
	}
}

// --- 4. Filesystem allowlist ---

func TestIndexRejectsOutsideAllowlist(t *testing.T) {
	s := newTestServer(t)
	out := captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"index","arguments":{"path":"/etc"}}}`)
	})
	resp := parseResponse(t, out)
	text := resultText(t, resp)
	if !strings.Contains(text, "outside the allowed index roots") {
		t.Fatalf("expected allowlist rejection, got %q", text)
	}
	// Rejection happens before any parse/walk/write, so nothing is created.
	if _, err := os.Stat(filepath.Join("/etc", ".codemap")); !os.IsNotExist(err) {
		t.Fatalf("index must not create anything under a rejected path: %v", err)
	}
}

func TestChangedSymbolsRejectsOutsideAllowlist(t *testing.T) {
	s := newTestServer(t)
	resp := toolResult(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"changed_symbols","arguments":{"repo_dir":"/etc"}}}`)
	text := resultText(t, resp)
	if !strings.Contains(text, "outside the allowed index roots") {
		t.Fatalf("expected allowlist rejection for repo_dir, got %q", text)
	}
}

func TestChangedSymbolsDefaultRepoDirAllowed(t *testing.T) {
	s := newTestServer(t)
	// Default repo_dir "." resolves to the served repo root, which is allowed
	// (in production the server's CWD is the served repo; here the test CWD is
	// the mcp package, so register it explicitly to model that).
	s.allowedPaths = []string{"."}
	resp := toolResult(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"changed_symbols","arguments":{}}}`)
	text := resultText(t, resp)
	if strings.Contains(text, "outside the allowed index roots") {
		t.Fatalf("default repo_dir should resolve to an allowed root, got %q", text)
	}
}

func TestIndexAcceptsAllowedTarget(t *testing.T) {
	src := "../testdata/simple"
	dst := filepath.Join(t.TempDir(), "simple")
	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	s := NewLazy("")
	s.allowedPaths = []string{dst}
	out := captureStdout(func() {
		s.dispatchLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"index","arguments":{"path":%q}}}`, dst))
	})
	if strings.Contains(out, `"isError":true`) {
		t.Fatalf("index of an allowed target failed: %s", out)
	}
	// Workspace member-root acceptance: the served repo root is always allowed.
	s2 := newTestServer(t)
	ok := false
	root, err := s2.allowedTarget(s2.repoPath)
	if err == nil && root != "" {
		ok = true
	}
	if !ok {
		t.Fatalf("served repo root must be an allowed target: %v", err)
	}
}

// --- 5. Failed-index memoization ---

func TestAutoReindexNoticeOnSuccess(t *testing.T) {
	s := newTestServer(t)
	indexed := false
	s.stalenessCheck = func(dbPath, repoPath string) (bool, error) {
		return true, nil
	}
	s.indexFn = func(absPath string) error {
		indexed = true
		return nil
	}

	if _, err := s.getStore(); err != nil {
		t.Fatalf("getStore: %v", err)
	}
	if !indexed {
		t.Fatal("expected the stale database to trigger an auto-reindex")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.warnings) != 1 {
		t.Fatalf("expected exactly one notice after auto-reindex, got %v", s.warnings)
	}
	notice := s.warnings[0]
	if !strings.Contains(notice, "auto-rebuilt") || !strings.Contains(notice, "results below reflect the latest code") {
		t.Fatalf("expected a past-tense success notice, got %q", notice)
	}
}

func TestAutoReindexNoMisleadingNoticeOnFailure(t *testing.T) {
	s := newTestServer(t)
	s.stalenessCheck = func(dbPath, repoPath string) (bool, error) {
		return true, nil
	}
	s.indexFn = func(absPath string) error {
		return errors.New("reindex failed")
	}

	if _, err := s.getStore(); err == nil {
		t.Fatal("expected the failing reindex to propagate")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.warnings {
		if strings.Contains(w, "auto-rebuilt") || strings.Contains(w, "reflect the latest code") {
			t.Fatalf("must not emit a rebuild-success notice when the reindex failed, got %q", w)
		}
	}
}

func TestFailedIndexMemoization(t *testing.T) {
	s := NewLazy("")
	var calls int
	fail := true
	s.indexFn = func(absPath string) error {
		calls++
		if fail {
			return errors.New("go list failed")
		}
		return nil
	}
	s.failedIndexBackoff = time.Minute

	if _, err := s.getStore(); err == nil {
		t.Fatal("expected the first index attempt to fail")
	}
	// Immediate repeat must fail fast without re-running the index.
	_, err := s.getStore()
	if err == nil {
		t.Fatal("expected the repeat getStore to return the cached failure")
	}
	if calls != 1 {
		t.Fatalf("expected no re-run of go list within backoff, got %d attempts", calls)
	}
	if !strings.Contains(err.Error(), "previously failed") {
		t.Fatalf("expected the repeat error to reference the earlier failure, got: %v", err)
	}

	// Expire the backoff window, then confirm success runs a fresh attempt and
	// clears the memoized failure.
	key, _ := filepath.Abs(".")
	s.mu.Lock()
	s.failedIndexEntries()[key] = failedIndex{at: time.Now().Add(-2 * time.Minute), err: "go list failed"}
	s.mu.Unlock()
	fail = false
	if _, err := s.getStore(); err != nil {
		t.Fatalf("expected index to succeed after clearing, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected a fresh attempt after backoff, got %d calls", calls)
	}
	if len(s.failedIndexEntries()) != 0 {
		t.Fatalf("expected the failure entry to be cleared on success, got %v", s.failedIndexes)
	}
}
