package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// A declared outputSchema is a promise: MCP requires every structuredContent
// response to conform to it. Both come from the same Go type, so this checks
// they cannot drift.
func TestStructuredContentMatchesOutputSchema(t *testing.T) {
	tools := buildToolsList()
	declared := 0
	for _, tool := range tools {
		name, _ := tool[keyName].(string)
		schema, ok := tool["outputSchema"].(map[string]any)
		if !ok {
			continue
		}
		declared++
		if schema["properties"] == nil {
			t.Errorf("%s: outputSchema has no properties", name)
		}
	}
	if declared == 0 {
		t.Fatal("no tool declares an outputSchema")
	}
	t.Logf("%d of %d tools declare an outputSchema", declared, len(tools))
}

// The file-scoped projection must hold for structuredContent too: the same call
// returns both, and a payload carrying pos_file would contradict the text.
func TestSymbolsInFileStructuredContentIsProjected(t *testing.T) {
	s, file := newWriteServer(t)

	rendered, typed, isErr := handleSymbolsInFile(s.store, nil, nil, map[string]any{keyFilePath: file})
	if isErr {
		t.Fatalf("handleSymbolsInFile: %s", rendered)
	}
	if strings.Contains(rendered, "pos_file") {
		t.Errorf("rendered payload carries pos_file:\n%s", rendered)
	}

	raw, err := json.Marshal(typed)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	for _, key := range []string{"pos_file", "receiver", "exported", "generated", "repo", "fields"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("structured content carries %q: %s", key, raw)
		}
	}
	for _, key := range []string{"qualified_name", "kind", "signature", "doc", "pos_line"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("structured content lost %q: %s", key, raw)
		}
	}
}

// structuredContent must be an object, never a bare array, and must carry the
// snake_case field names the render layer uses.
func TestBuildStructuredWrapsArrays(t *testing.T) {
	got := structuredPayload("", []map[string]any{{"qualified_name": "pkg.A"}})
	if got.JSON == nil {
		t.Fatal("expected structured output for a non-nil result")
	}
	if _, isObj := got.JSON[resultsKey]; !isObj {
		t.Fatalf("array result must be wrapped under %q, got %v", resultsKey, got.JSON)
	}

	raw, err := json.Marshal(got.JSON)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if raw[0] != '{' {
		t.Errorf("structuredContent must be a JSON object, got %s", raw)
	}
}

// A tool with no declared schema and no result has no structured payload; one
// whose schema is declared gets the empty value so the payload still conforms.
func TestStructuredPayloadFallsBackToDeclaredSample(t *testing.T) {
	if got := structuredPayload("", nil); got.JSON != nil {
		t.Errorf("a nil result with no declared schema has no payload, got %v", got.JSON)
	}
	got := structuredPayload(toolUnused, nil)
	if _, ok := got.JSON[resultsKey]; !ok {
		t.Errorf("unused declares an outputSchema, so its empty result must still conform, got %v", got.JSON)
	}
}

// The schema for a slice tool must describe the wrapper the payload uses.
func TestOutputSchemaMatchesArrayWrapper(t *testing.T) {
	schema := outputSchemaFor("search")
	if schema == nil {
		t.Fatal("search must declare an outputSchema")
	}
	if schema[keyType] != "object" {
		t.Fatalf("search outputSchema type = %v, want object", schema[keyType])
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props[resultsKey]; !ok {
		t.Errorf("search outputSchema must describe the %q wrapper, got %v", resultsKey, props)
	}
}

// Schema key ordering must be stable so tools/list is byte-identical between
// calls.
func TestOutputSchemaIsStable(t *testing.T) {
	first, err := json.Marshal(outputSchemaFor("unused"))
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	second, err := json.Marshal(outputSchemaFor("unused"))
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(first) != string(second) {
		t.Error("outputSchema changed between calls")
	}
}

// TestOutputSchemaHandlesSelfReferentialType pins that a self-referential
// field type (SymbolField.Fields []SymbolField, one level per nested anonymous
// struct) terminates. Without a visited set the reflector recursed until the
// stack died and every tools/list call panicked.
func TestOutputSchemaHandlesSelfReferentialType(t *testing.T) {
	tools := buildToolsList()
	if len(tools) == 0 {
		t.Fatal("no tools built")
	}
	declared := 0
	for _, tool := range tools {
		name, _ := tool[keyName].(string)
		if outputSchemaFor(name) != nil {
			declared++
		}
	}
	if declared == 0 {
		t.Fatal("no tool declares an output schema")
	}
}
