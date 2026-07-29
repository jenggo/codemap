package mcp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

func TestToolsListIncludesNewTools(t *testing.T) {
	abs, err := filepath.Abs("../testdata/simple")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	parseResult, err := parse.Run(abs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	st, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	tools := buildToolsList()
	names := make(map[string]bool, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names[name] = true
	}
	required := []string{
		"get_symbol_body",
		"changed_symbols",
		"dependency_layers",
		"dependency_flow",
		"entry_points",
		"search_text",
		"get_context_bundle",
		"get_hotspots",
		"get_symbol_importance",
	}
	for _, r := range required {
		if !names[r] {
			t.Errorf("expected tool %q in tools/list, got: %v", r, names)
		}
	}
}

func TestSchemaIncludesNewEntries(t *testing.T) {
	schemaJSON := handleSchema()
	var schema map[string]any
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		t.Fatalf("schema unmarshal: %v", err)
	}
	required := []string{
		"SymbolBodyResult",
		"ChangedSymbol",
		"ChangedSymbolsSummary",
		"ChangedSymbolsResult",
		"Layer",
		"Hub",
		"LayersResult",
		"FlowResult",
		"EntryPoint",
		"FileMatch",
		"Bundle",
		"Hotspot",
		"ImportanceEntry",
	}
	for _, r := range required {
		if _, ok := schema[r]; !ok {
			t.Errorf("expected schema entry %q, got keys: %v", r, keys(schema))
		}
	}
	_ = strings.Join
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
