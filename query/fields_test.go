package query_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

func setupFieldsStore(t *testing.T) *store.Store {
	t.Helper()
	abs, err := filepath.Abs("../testdata/fixtures/fields")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return setupFixtureStore(t, abs)
}

func setupFixtureStoreAt(t *testing.T, absPath, dbPath string) *store.Store {
	t.Helper()
	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.Write(resolve.Run(parseResult), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

func fieldByName(fields []store.SymbolField, goName string) (store.SymbolField, bool) {
	for _, f := range fields {
		if f.GoName == goName {
			return f, true
		}
	}
	return store.SymbolField{}, false
}

func TestShowStructFields(t *testing.T) {
	s := setupFieldsStore(t)
	result, err := query.Show(s, "example.com/fields.Tagged")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}

	cases := []store.SymbolField{
		{GoName: "UserID", WireName: "user_id", GoType: "string"},
		{GoName: "Name", WireName: "name", GoType: "string"},
		{GoName: "Both", WireName: "cbor_name", GoType: "string"},
		{GoName: "Skipped", WireName: "Skipped", GoType: "string"},
		{GoName: "Plain", WireName: "Plain", GoType: "int"},
		{GoName: "Embedded", WireName: "Embedded", GoType: "Embedded"},
	}
	for _, want := range cases {
		got, ok := fieldByName(result.Symbol.Fields, want.GoName)
		if !ok {
			t.Fatalf("missing field %s in %v", want.GoName, result.Symbol.Fields)
		}
		if got != want {
			t.Errorf("field %s = %+v, want %+v", want.GoName, got, want)
		}
	}
}

func TestShowAliasSamePackageFields(t *testing.T) {
	s := setupFieldsStore(t)
	result, err := query.Show(s, "example.com/fields.LocalAlias")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got, ok := fieldByName(result.Symbol.Fields, "X"); !ok || got.GoType != "int" {
		t.Errorf("LocalAlias fields = %v, want X int", result.Symbol.Fields)
	}
}

func TestShowAliasCrossPackageHasNoFields(t *testing.T) {
	s := setupFieldsStore(t)
	result, err := query.Show(s, "example.com/fields.RemoteAlias")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(result.Symbol.Fields) != 0 {
		t.Errorf("cross-package alias fields = %v, want none", result.Symbol.Fields)
	}
}

func TestShowNonStructHasNoFields(t *testing.T) {
	s := setupFieldsStore(t)
	for _, qn := range []string{
		"example.com/fields.F",
		"example.com/fields.Tagged.Method",
		"example.com/fields.Greeter",
		"example.com/fields.C",
		"example.com/fields.V",
		"example.com/fields.Empty",
	} {
		result, err := query.Show(s, qn)
		if err != nil {
			t.Fatalf("Show(%s): %v", qn, err)
		}
		if len(result.Symbol.Fields) != 0 {
			t.Errorf("Show(%s) fields = %v, want none", qn, result.Symbol.Fields)
		}
		data, err := json.Marshal(result.Symbol)
		if err != nil {
			t.Fatalf("marshal %s: %v", qn, err)
		}
		if strings.Contains(string(data), `"fields"`) {
			t.Errorf("Show(%s) JSON carries a fields key: %s", qn, data)
		}
	}
}

func TestPackageAndSearchFieldsOptIn(t *testing.T) {
	s := setupFieldsStore(t)

	pkgDefault, err := query.Package(s, "example.com/fields")
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	for _, sym := range pkgDefault.ExportedSymbols {
		if len(sym.Fields) != 0 {
			t.Errorf("default package result %s carries fields", sym.QualifiedName)
		}
	}

	pkgWith, err := query.Package(s, "example.com/fields", query.WithFields())
	if err != nil {
		t.Fatalf("Package WithFields: %v", err)
	}
	var sawStruct, sawNonStruct bool
	for _, sym := range pkgWith.ExportedSymbols {
		switch sym.QualifiedName {
		case "example.com/fields.Tagged":
			sawStruct = len(sym.Fields) > 0
		case "example.com/fields.F":
			sawNonStruct = len(sym.Fields) == 0
		}
	}
	if !sawStruct {
		t.Error("Package WithFields must attach fields to struct symbols")
	}
	if !sawNonStruct {
		t.Error("Package WithFields must not attach fields to non-struct symbols")
	}

	searchDefault, err := query.Search(s, "Tagged")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range searchDefault {
		if len(r.Fields) != 0 {
			t.Errorf("default search result %s carries fields", r.QualifiedName)
		}
	}

	searchWith, err := query.Search(s, "Tagged", query.WithFields())
	if err != nil {
		t.Fatalf("Search WithFields: %v", err)
	}
	found := false
	for _, r := range searchWith {
		if r.QualifiedName == "example.com/fields.Tagged" && len(r.Fields) > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("Search WithFields must attach struct fields, got %v", searchWith)
	}
}

// Fields come from the fields_json column already persisted on the symbol row,
// so reopening an existing database returns them without a reindex.
func TestExistingIndexReturnsFieldsWithoutReindex(t *testing.T) {
	abs, err := filepath.Abs("../testdata/fixtures/fields")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "fields.db")
	built := setupFixtureStoreAt(t, abs, dbPath)

	stored, err := built.SymbolByName("example.com/fields.Tagged")
	if err != nil {
		t.Fatalf("SymbolByName: %v", err)
	}
	if len(stored.Fields) == 0 {
		t.Fatal("fixture index has no stored fields")
	}
	if err := built.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open existing index: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	result, err := query.Show(reopened, "example.com/fields.Tagged")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(result.Symbol.Fields) != len(stored.Fields) {
		t.Errorf("show fields = %d, stored = %d", len(result.Symbol.Fields), len(stored.Fields))
	}
}
