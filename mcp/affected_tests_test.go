package mcp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestAffectedTestsEmitsJSON pins the D8 rule: the MCP text content is JSON,
// not TOON, because the result is nested.
func TestAffectedTestsEmitsJSON(t *testing.T) {
	s := newTestServer(t)
	abs, err := filepath.Abs("../testdata/simple/parser.go")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	resp := toolResult(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"affected_tests","arguments":{"files":["`+abs+`"]}}}`)
	text := resultText(t, resp)
	// The server may prepend a stale-index notice; the JSON body follows it.
	if i := strings.Index(text, "{"); i >= 0 {
		text = text[i:]
	}

	var got struct {
		Files []struct {
			File         string   `json:"file"`
			Reason       string   `json:"reason"`
			Via          []string `json:"via"`
			ViaTruncated bool     `json:"via_truncated"`
		} `json:"files"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("affected_tests text is not JSON: %v (%q)", err, text)
	}
	if got.Files == nil {
		t.Fatalf("expected a files array, got %q", text)
	}
	if got.Note == "" {
		t.Fatalf("expected the lower-bound note, got %q", text)
	}
}

// TestAffectedTestsNoFilesOutsideGit pins the error path: without files the
// query needs a git ref, so a non-git directory must error rather than return
// an empty list that reads like "no tests affected".
func TestAffectedTestsNoFilesOutsideGit(t *testing.T) {
	s := newTestServer(t)
	resp := toolResult(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"affected_tests","arguments":{"repo":"`+t.TempDir()+`"}}}`)
	text := resultText(t, resp)
	if !strings.Contains(text, "Error:") {
		t.Fatalf("expected an error for a non-git directory, got %q", text)
	}
}
