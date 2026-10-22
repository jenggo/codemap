package mcp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

// newFieldsServer builds a server over the struct-field fixture, so `show`,
// `package`, and `search` can be exercised end to end.
func newFieldsServer(t *testing.T) *Server {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fixtures/fields")
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

func TestShowToolIncludesFieldsForStructs(t *testing.T) {
	s := newFieldsServer(t)
	out, isErr := s.handleTool("show", map[string]any{"qualified_name": "example.com/fields.Tagged"})
	if isErr {
		t.Fatalf("show failed: %s", out)
	}
	for _, want := range []string{"go_name", "wire_name", "go_type", "user_id", "cbor_name"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}

	out, isErr = s.handleTool("show", map[string]any{"qualified_name": "example.com/fields.F"})
	if isErr {
		t.Fatalf("show failed: %s", out)
	}
	if strings.Contains(out, "wire_name") {
		t.Errorf("show for a function must omit fields:\n%s", out)
	}
}

func TestPackageAndSearchToolFieldsOptIn(t *testing.T) {
	s := newFieldsServer(t)
	pkgPath := "example.com/fields"

	for _, args := range []map[string]any{
		{"path": pkgPath},
		{"path": pkgPath, keyIncludeFields: true},
	} {
		out, isErr := s.handleTool(keyToolPackage, args)
		if isErr {
			t.Fatalf("package %v failed: %s", args, out)
		}
		want := args[keyIncludeFields] == true
		if got := strings.Contains(out, "wire_name"); got != want {
			t.Errorf("package %v: fields present = %v, want %v", args, got, want)
		}
	}

	out, isErr := s.handleTool("search", map[string]any{"pattern": "Tagged"})
	if isErr {
		t.Fatalf("search failed: %s", out)
	}
	if strings.Contains(out, "wire_name") {
		t.Errorf("search without include_fields must omit fields:\n%s", out)
	}

	out, isErr = s.handleTool("search", map[string]any{"pattern": "Tagged", keyIncludeFields: true})
	if isErr {
		t.Fatalf("search with fields failed: %s", out)
	}
	if !strings.Contains(out, "wire_name") {
		t.Errorf("search with include_fields must carry fields:\n%s", out)
	}
}

func TestSchemaDocumentsFields(t *testing.T) {
	out, isErr := newFieldsServer(t).handleTool("schema", map[string]any{})
	if isErr {
		t.Fatalf("schema failed: %s", out)
	}
	var schemas map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &schemas); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schemas["SymbolDetail"]["fields"]; !ok {
		t.Error("SymbolDetail schema must document fields")
	}
	fieldSchema, ok := schemas["SymbolField"]
	if !ok {
		t.Fatal("schema must document SymbolField")
	}
	for _, key := range []string{"go_name", "wire_name", "go_type"} {
		if _, ok := fieldSchema[key]; !ok {
			t.Errorf("SymbolField schema must document %s", key)
		}
	}
}

func TestToolDefsDocumentIncludeFields(t *testing.T) {
	for _, tool := range buildToolsList() {
		name, _ := tool["name"].(string)
		if name != keyToolPackage && name != "search" {
			continue
		}
		props, ok := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties", name)
		}
		if _, ok := props[keyIncludeFields]; !ok {
			t.Errorf("%s tool definition must declare include_fields", name)
		}
	}
}
