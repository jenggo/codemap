package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

// newTestServer builds a server backed by a populated, non-stale store so that
// tools/call dispatch reaches the handler without triggering an auto-index.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	abs, err := filepath.Abs("../testdata/simple")
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
	return &Server{store: st, dbPath: dbPath, repoPath: abs}
}

// captureStdout runs fn and returns everything written to os.Stdout.
func captureStdout(fn func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func parseResponse(t *testing.T, out string) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("invalid response %q: %v", out, err)
	}
	return resp
}

func TestPingReturnsEmptyResult(t *testing.T) {
	s := newTestServer(t)
	out := captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	})
	resp := parseResponse(t, out)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("ping returned error: %v", resp)
	}
	if _, hasResult := resp["result"]; !hasResult {
		t.Fatalf("ping missing result: %v", resp)
	}
}

func TestToolCallErrorSetsIsError(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		line string
	}{
		{"missing required arg", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"show","arguments":{}}}`},
		{"unknown tool", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope_tool","arguments":{}}}`},
	}
	for _, tc := range cases {
		out := captureStdout(func() {
			s.dispatchLine(tc.line)
		})
		resp := parseResponse(t, out)
		res, ok := resp["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no result object: %v", tc.name, resp)
		}
		if isErr, _ := res["isError"].(bool); !isErr {
			t.Errorf("%s: expected isError=true, got: %v", tc.name, res)
		}
	}
}

func TestToolCallInvalidParamsIsProtocolError(t *testing.T) {
	s := newTestServer(t)
	out := captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":"notanobject"}`)
	})
	resp := parseResponse(t, out)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected protocol error, got: %v", resp)
	}
	if code, _ := errObj["code"].(float64); code != -32602 {
		t.Errorf("expected code -32602, got %v", errObj)
	}
}

func TestToolCallSuccessHasNoIsError(t *testing.T) {
	s := newTestServer(t)
	out := captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"search","arguments":{"pattern":"Store"}}}`)
	})
	resp := parseResponse(t, out)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object: %v", resp)
	}
	if _, has := res["isError"]; has {
		t.Errorf("successful call should not set isError: %v", res)
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	s := newTestServer(t)
	out := captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("notification should produce no response, got: %q", out)
	}
}

func TestServerSurvivesOversizedLine(t *testing.T) {
	s := newTestServer(t)

	inR, inW, _ := os.Pipe()
	oldIn := os.Stdin
	os.Stdin = inR
	defer func() { os.Stdin = oldIn }()

	outR, outW, _ := os.Pipe()
	oldOut := os.Stdout
	os.Stdout = outW
	defer func() { os.Stdout = oldOut }()

	go func() {
		_, _ = inW.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
		_, _ = inW.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"pattern":"` + strings.Repeat("x", 70000) + `"}}}` + "\n")
		_, _ = inW.WriteString(`{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n")
		_ = inW.Close()
	}()

	err := s.Run()
	if err != nil {
		t.Fatalf("server exited on oversized line: %v", err)
	}
	_ = outW.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, outR)

	got := strings.Count(buf.String(), `"jsonrpc":"2.0"`)
	if got < 3 {
		t.Fatalf("expected responses for all 3 lines, got %d: %q", got, buf.String())
	}
	if !strings.Contains(buf.String(), `"id":3`) {
		t.Fatalf("no response for line after oversized input: %q", buf.String())
	}
}

// copyDir recursively copies src to dst.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

func TestSearchTextWorksAfterMCPIndex(t *testing.T) {
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
		t.Fatalf("index failed: %s", out)
	}

	out = captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":"Parser"}}}`)
	})
	if strings.Contains(out, `"isError":true`) {
		t.Fatalf("search_text failed: %s", out)
	}
	if strings.Contains(out, "No results") {
		t.Fatalf("search_text returned no results after index: %s", out)
	}
	if !strings.Contains(out, "Parser") {
		t.Fatalf("search_text did not surface the match: %s", out)
	}

	out = captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":"func [A-Z]","is_regex":true}}}`)
	})
	if strings.Contains(out, "No results") {
		t.Fatalf("search_text regex mode returned no results after index: %s", out)
	}

	out = captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":"Parser","file_pattern":"parser.go"}}}`)
	})
	if strings.Contains(out, `"isError":true`) || strings.Contains(out, "No results") {
		t.Fatalf("search_text with file_pattern failed: %s", out)
	}
}
