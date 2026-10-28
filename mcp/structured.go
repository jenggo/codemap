package mcp

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"codemap/query"
	"codemap/store"
)

// structuredOutput is the tool result payload a handler produced, rendered as a
// JSON object. MCP requires structuredContent to be an object, so a handler's
// slice result is wrapped under one key.
type structuredOutput struct {
	JSON map[string]any
}

const (
	resultsKey       = "results"
	schemaTypeObject = "object"
	schemaTypeString = "string"
	keyProperties    = "properties"
)

// structuredPayload builds the structuredContent for a tool call. It round-trips
// the handler's typed result through JSON so struct tags and encoders apply
// exactly as they do on the wire, and the object a client receives is the object
// described by the tool's outputSchema.
//
// A handler that returns nil found nothing. When the tool declares an
// outputSchema, the empty value of the declared type stands in, because MCP
// requires every structuredContent to conform to the schema the client was
// promised; "nothing found" is an empty result, not a missing one. A tool that
// declares no schema and returns nothing has no structured payload.
func structuredPayload(tool string, typed any) structuredOutput {
	if typed == nil {
		typed = outputSample(tool)
	}
	if typed == nil {
		return structuredOutput{}
	}
	raw, err := json.Marshal(typed)
	if err != nil {
		return structuredOutput{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return structuredOutput{}
	}
	if obj, ok := value.(map[string]any); ok {
		return structuredOutput{JSON: obj}
	}
	return structuredOutput{JSON: map[string]any{resultsKey: value}}
}

// outputSample describes the value each tool returns, so tools/list can declare
// an outputSchema before any call has happened. A nil sample means the tool
// returns no structured content.
//
// Only tools with one fixed result shape are listed. A tool whose shape depends
// on its arguments is omitted rather than declaring a schema that would be
// wrong half the time — MCP requires a declared schema to hold for every
// response. That covers show (source=true returns SymbolBodyResult), package
// (no path returns []store.Package), dependency_flow (layers=true returns
// LayersResult), and get_hotspots (mode=pagerank returns importance entries).
// symbols_in_file is omitted too: its file-scoped projection drops the fields
// the SearchResult sample would advertise.
var toolOutputSamples = map[string]any{
	toolOverview:         &query.OverviewResult{},
	toolSummary:          &query.SummaryResult{},
	toolBlastRadius:      &query.BlastRadius{},
	toolChangedSymbols:   &query.ChangedSymbolsResult{},
	toolAffectedTests:    &query.AffectedTestsResult{},
	toolFindDeclaration:  &query.DeclarationResult{},
	toolContextBundle:    &query.Bundle{},
	toolSearch:           []query.SearchResult{},
	toolTypeUsage:        []query.SearchResult{},
	toolCallersOf:        []query.EdgeDetail{},
	toolCalleesOf:        []query.EdgeDetail{},
	toolImportsOf:        []query.EdgeDetail{},
	toolAllEdges:         []query.EdgeDetail{},
	toolMethodsOf:        []query.SymbolDetail{},
	toolDiagnostics:      []store.Diagnostic{},
	toolFindPath:         []query.PathStep{},
	toolInterfaceImpls:   []query.InterfaceImpl{},
	toolUnused:           []query.UnusedSymbol{},
	toolCycles:           []query.Cycle{},
	toolEntryPoints:      []query.EntryPoint{},
	toolSearchText:       []store.FileMatch{},
	keyPattern:           []query.PatternMatch{},
	toolContracts:        []store.Contract{},
	toolContractDrift:    []store.DriftReport{},
	toolRuntimeContracts: []store.RuntimeContract{},
}

// outputSample returns the zero value of a tool's result type, or nil when the
// tool declares no outputSchema.
//
// Only tools with one fixed result shape are listed. A tool whose shape depends
// on its arguments (show with source=true returns SymbolBodyResult) is omitted
// rather than declaring a schema that would be wrong half the time — MCP
// requires a declared schema to hold for every response.
func outputSample(tool string) any {
	return toolOutputSamples[tool]
}

// outputSchemaFor returns the outputSchema object for a tool, or nil when the
// tool declares none.
func outputSchemaFor(tool string) map[string]any {
	sample := outputSample(tool)
	if sample == nil {
		return nil
	}
	schema := schemaForType(reflect.TypeOf(sample))
	if schema[keyType] == schemaTypeObject {
		return schema
	}
	return map[string]any{
		keyType:       schemaTypeObject,
		keyProperties: map[string]any{resultsKey: schema},
		"required":    []string{resultsKey},
	}
}

// schemaForType describes a Go type as a JSON Schema fragment, using the same
// json tags that produce the wire shape.
func schemaForType(t reflect.Type) map[string]any {
	return schemaForSeen(t, make(map[reflect.Type]bool))
}

// schemaForSeen carries the types already on the current path. A self-
// referential type (SymbolField.Fields []SymbolField, one level per nested
// anonymous struct) would otherwise recurse until the stack dies.
func schemaForSeen(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if seen[t] {
		return map[string]any{keyType: schemaTypeObject}
	}

	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{keyType: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return map[string]any{keyType: "number"}
	case reflect.String:
		return map[string]any{keyType: schemaTypeString}
	case reflect.Slice, reflect.Array:
		return map[string]any{keyType: "array", "items": schemaForSeen(t.Elem(), seen)}
	case reflect.Map:
		return map[string]any{keyType: schemaTypeObject}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		seen[t] = true
		schema := structSchema(t, seen)
		delete(seen, t)
		return schema
	case reflect.Invalid, reflect.Uintptr, reflect.Complex64, reflect.Complex128,
		reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Pointer:
		return map[string]any{}
	}
	return map[string]any{}
}

func structSchema(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	props := make(map[string]any, t.NumField())
	required := make([]string, 0, t.NumField())
	for field := range t.Fields() {
		if !field.IsExported() {
			continue
		}
		name, optional := jsonFieldName(field)
		if name == "-" {
			continue
		}
		props[name] = schemaForSeen(field.Type, seen)
		if !optional {
			required = append(required, name)
		}
	}
	slices.Sort(required)
	return map[string]any{
		keyType:       schemaTypeObject,
		keyProperties: props,
		"required":    required,
	}
}

// jsonFieldName returns the wire name for a struct field and whether the field
// may be absent, reading encoding/json's tag rules.
func jsonFieldName(field reflect.StructField) (name string, optional bool) {
	name = field.Name
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return name, false
	}
	parts := strings.Split(tag, ",")
	if parts[0] != "" {
		name = parts[0]
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			optional = true
		}
	}
	return name, optional
}
