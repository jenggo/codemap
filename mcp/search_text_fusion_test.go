package mcp

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestSearchTextCorrectionSurfacedInResponse verifies the handler-level
// contract: a misspelled FTS-mode query re-runs with corrected terms and the
// response carries a corrected_terms note, while regex mode never does.
func TestSearchTextCorrectionSurfacedInResponse(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "simple")
	if err := copyDir("../testdata/simple", dst); err != nil {
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

	// Misspelled symbol name: "NwParser" is one edit from exactly one
	// lexicon entry ("newparser"), so the correction pass must run once.
	out = captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":"NwParser"}}}`)
	})
	if strings.Contains(out, `"isError":true`) {
		t.Fatalf("search_text failed: %s", out)
	}
	if !strings.Contains(out, "corrected_terms:") {
		t.Fatalf("response missing corrected_terms note: %s", out)
	}
	if !strings.Contains(out, "NwParser") || !strings.Contains(out, "newparser") {
		t.Fatalf("response missing the applied correction: %s", out)
	}
	if !strings.Contains(out, "NewParser") {
		t.Fatalf("corrected search did not surface the symbol: %s", out)
	}

	// Sufficient results: no correction note may appear.
	out = captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":"Parser"}}}`)
	})
	if strings.Contains(out, "corrected_terms:") {
		t.Fatalf("correction ran despite sufficient results: %s", out)
	}

	// Regex mode: never corrected.
	out = captureStdout(func() {
		s.dispatchLine(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_text","arguments":{"pattern":"func [A-Z]","is_regex":true}}}`)
	})
	if strings.Contains(out, "corrected_terms:") {
		t.Fatalf("regex mode must not be corrected: %s", out)
	}
	if strings.Contains(out, "No results") {
		t.Fatalf("regex mode returned no results: %s", out)
	}
}
