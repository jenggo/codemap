package render

import (
	"strings"
	"testing"

	"codemap/query"
	"codemap/store"
)

func showFixture() *query.ShowResult {
	return &query.ShowResult{
		Symbol: query.SymbolDetail{
			QualifiedName: "example.com/model.Tagged",
			Kind:          "type",
			Signature:     "struct { UserID string; Name string }",
			PosFile:       "model.go",
			PosLine:       5,
			Exported:      true,
			Fields: []store.SymbolField{
				{GoName: "UserID", WireName: "user_id", GoType: "string"},
				{GoName: "Name", WireName: "name", GoType: "string"},
			},
		},
		IncomingEdges: []query.EdgeDetail{},
		OutgoingEdges: []query.EdgeDetail{},
	}
}

func functionShowFixture() *query.ShowResult {
	return &query.ShowResult{
		Symbol: query.SymbolDetail{
			QualifiedName: "example.com/model.F",
			Kind:          "function",
			Signature:     "func ()",
			PosFile:       "model.go",
			PosLine:       60,
			Exported:      true,
		},
		IncomingEdges: []query.EdgeDetail{},
		OutgoingEdges: []query.EdgeDetail{},
	}
}

func TestRenderShowFieldsAllFormats(t *testing.T) {
	structOut := map[Format]string{}
	for _, f := range []Format{FormatTOON, FormatJSON, FormatText, FormatCompact} {
		structOut[f] = RenderShow(showFixture(), WithFormat(f))
		if !strings.Contains(structOut[f], "user_id") {
			t.Errorf("format %v must carry wire names, got %s", f, structOut[f])
		}
	}

	for _, f := range []Format{FormatTOON, FormatJSON, FormatText, FormatCompact} {
		out := RenderShow(functionShowFixture(), WithFormat(f))
		if strings.Contains(out, `"fields"`) || strings.Contains(out, "fields:") || strings.Contains(out, "wire=") {
			t.Errorf("format %v must omit fields for a function, got %s", f, out)
		}
	}
}

func TestRenderShowEmptyStructOmitsFields(t *testing.T) {
	empty := &query.ShowResult{
		Symbol: query.SymbolDetail{
			QualifiedName: "example.com/model.Empty",
			Kind:          "type",
			Signature:     "struct{}",
			Exported:      true,
		},
	}
	for _, f := range []Format{FormatTOON, FormatJSON, FormatText, FormatCompact} {
		out := RenderShow(empty, WithFormat(f))
		if strings.Contains(out, `"fields"`) || strings.Contains(out, "fields:") || strings.Contains(out, "wire=") {
			t.Errorf("format %v must omit fields for an empty struct, got %s", f, out)
		}
	}
}

func TestRenderPackageAndSearchFieldsWhenPopulated(t *testing.T) {
	pkg := &query.PackageResult{
		Path: "example.com/model",
		Name: "model",
		Symbols: []query.SymbolDetail{
			{QualifiedName: "example.com/model.Tagged", Kind: "type", Exported: true, Fields: []store.SymbolField{
				{GoName: "UserID", WireName: "user_id", GoType: "string"},
			}},
			{QualifiedName: "example.com/model.F", Kind: "function", Exported: true},
		},
	}

	text := RenderPackage(pkg, WithFormat(FormatText))
	if !strings.Contains(text, "user_id") {
		t.Errorf("package text must carry fields, got %s", text)
	}

	jsonOut := RenderPackage(pkg, WithFormat(FormatJSON))
	if !strings.Contains(jsonOut, `"fields"`) {
		t.Errorf("package JSON must carry fields, got %s", jsonOut)
	}

	toonOut := RenderPackage(pkg, WithFormat(FormatTOON))
	if !strings.Contains(toonOut, "user_id") {
		t.Errorf("package TOON must carry fields, got %s", toonOut)
	}

	results := []query.SearchResult{
		{QualifiedName: "example.com/model.Tagged", Kind: "type", Exported: true, Fields: []store.SymbolField{
			{GoName: "UserID", WireName: "user_id", GoType: "string"},
		}},
		{QualifiedName: "example.com/model.F", Kind: "function", Exported: true},
	}
	searchText := RenderSearch(results, WithFormat(FormatText))
	if !strings.Contains(searchText, "user_id") {
		t.Errorf("search text must carry fields, got %s", searchText)
	}
	searchJSON := RenderSearch(results, WithFormat(FormatJSON))
	if !strings.Contains(searchJSON, `"fields"`) {
		t.Errorf("search JSON must carry fields, got %s", searchJSON)
	}
	searchTOON := RenderSearch(results, WithFormat(FormatTOON))
	if !strings.Contains(searchTOON, "user_id") {
		t.Errorf("search TOON must carry fields, got %s", searchTOON)
	}
}

func TestRenderPackageAndSearchWithoutFieldsOmitKey(t *testing.T) {
	pkg := &query.PackageResult{
		Path: "example.com/model",
		Name: "model",
		Symbols: []query.SymbolDetail{
			{QualifiedName: "example.com/model.Tagged", Kind: "type", Exported: true},
		},
	}
	for _, f := range []Format{FormatTOON, FormatJSON, FormatText, FormatCompact} {
		out := RenderPackage(pkg, WithFormat(f))
		if strings.Contains(out, `"fields"`) || strings.Contains(out, "fields:") || strings.Contains(out, "wire=") {
			t.Errorf("package format %v must omit fields when unpopulated, got %s", f, out)
		}
	}

	results := []query.SearchResult{{QualifiedName: "example.com/model.Tagged", Kind: "type", Exported: true}}
	for _, f := range []Format{FormatTOON, FormatJSON, FormatText, FormatCompact} {
		out := RenderSearch(results, WithFormat(f))
		if strings.Contains(out, `"fields"`) || strings.Contains(out, "fields:") || strings.Contains(out, "wire=") {
			t.Errorf("search format %v must omit fields when unpopulated, got %s", f, out)
		}
	}
}

func bundleFixture(sym query.SymbolDetail) *query.Bundle {
	return &query.Bundle{
		QualifiedName: sym.QualifiedName,
		Symbol:        &sym,
		Body:          "type Tagged struct {\n\tUserID string\n}",
		Callees:       []query.EdgeDetail{},
		Callers:       []query.EdgeDetail{},
		SameFile:      []query.SearchResult{},
	}
}

func TestRenderBundleCarriesFields(t *testing.T) {
	bundle := bundleFixture(showFixture().Symbol)
	for _, f := range []Format{FormatTOON, FormatJSON, FormatText, FormatCompact} {
		out := RenderBundle(bundle, WithFormat(f))
		if f == FormatCompact {
			if strings.Contains(out, "user_id") {
				t.Errorf("compact bundle has no room for fields, got %s", out)
			}
			continue
		}
		if !strings.Contains(out, "user_id") {
			t.Errorf("bundle format %v must carry wire names, got %s", f, out)
		}
	}
}

func TestRenderBundleOmitsFieldsForNonStruct(t *testing.T) {
	bundle := bundleFixture(functionShowFixture().Symbol)
	for _, f := range []Format{FormatTOON, FormatJSON, FormatText, FormatCompact} {
		out := RenderBundle(bundle, WithFormat(f))
		if strings.Contains(out, `"fields"`) || strings.Contains(out, "fields:") || strings.Contains(out, "wire=") {
			t.Errorf("bundle format %v must omit fields for a function, got %s", f, out)
		}
	}
}

// A nested array of field rows made gotoon fall back to indented records for
// the whole symbols table, so one include_fields=true call changed the shape
// that a client parses. The fields column must stay tabular.
func TestRenderPackageFieldsKeepTabularShape(t *testing.T) {
	pkg := &query.PackageResult{
		Path: "example.com/model",
		Name: "model",
		Symbols: []query.SymbolDetail{
			{QualifiedName: "example.com/model.Tagged", Kind: "type", Exported: true, Fields: []store.SymbolField{
				{GoName: "UserID", WireName: "user_id", GoType: "string"},
				{GoName: "Sentry", WireName: "sentry", GoType: "struct{...}", Fields: []store.SymbolField{
					{GoName: "URL", WireName: "url", GoType: "string"},
				}},
			}},
			{QualifiedName: "example.com/model.F", Kind: "function", Exported: true},
		},
	}

	out := RenderPackage(pkg, WithFormat(FormatTOON))
	if !strings.Contains(out, "symbols[2]{doc,exported,fields,") {
		t.Errorf("package TOON must keep one tabular header:\n%s", out)
	}
	if !strings.Contains(out, "Sentry:sentry:{URL:url:string}") {
		t.Errorf("nested struct must render inline, not as struct{}:\n%s", out)
	}
	if strings.Contains(out, "- doc:") {
		t.Errorf("package TOON must not flip to indented records:\n%s", out)
	}

	search := RenderSearch([]query.SearchResult{
		{QualifiedName: "example.com/model.Tagged", Kind: "type", Exported: true, Fields: []store.SymbolField{
			{GoName: "UserID", WireName: "user_id", GoType: "string"},
		}},
		{QualifiedName: "example.com/model.F", Kind: "function", Exported: true},
	}, WithFormat(FormatTOON))
	if !strings.HasPrefix(search, "results[2]{doc,exported,fields,") {
		t.Errorf("search TOON must keep one tabular header:\n%s", search)
	}
}
