package query

import (
	"path/filepath"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

func TestEntryPointsMain(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/entrypoints")
	entries, err := EntryPoints(s, nil, false)
	if err != nil {
		t.Fatalf("EntryPoints: %v", err)
	}
	var mainEntry *EntryPoint
	for i, e := range entries {
		t.Logf("entry: %s reason=%s", e.QualifiedName, e.Reason)
		if e.Reason == "main" {
			mainEntry = &entries[i]
		}
	}
	if mainEntry == nil {
		t.Fatal("expected a main() entry point")
	}
}

func TestEntryPointsUncalledExported(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/entrypoints")
	entries, err := EntryPoints(s, []string{"uncalled_exported"}, false)
	if err != nil {
		t.Fatalf("EntryPoints: %v", err)
	}
	hasStray := false
	hasCalled := false
	for _, e := range entries {
		t.Logf("uncalled_exported: %s", e.QualifiedName)
		if e.QualifiedName == "util.StrayExported" {
			hasStray = true
		}
		if e.QualifiedName == "cmd.ThisExportedIsCalled" {
			hasCalled = true
		}
	}
	if !hasStray {
		t.Error("expected util.StrayExported in uncalled_exported")
	}
	if hasCalled {
		t.Error("did not expect cmd.ThisExportedIsCalled (it is called)")
	}
}

func TestEntryPointsDefaultExcludesTest(t *testing.T) {
	s := setupQueryStoreAt(t, "../testdata/entrypoints")
	entries, err := EntryPoints(s, nil, false)
	if err != nil {
		t.Fatalf("EntryPoints: %v", err)
	}
	for _, e := range entries {
		if e.Reason == "test" {
			t.Errorf("did not expect test entries without include_tests: %s", e.QualifiedName)
		}
	}
}

func TestEntryPointsHandlerSigOptIn(t *testing.T) {
	abs, _ := filepath.Abs("../testdata/entrypoints")
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
	defaults, _ := EntryPoints(st, nil, false)
	for _, e := range defaults {
		if e.Reason == "handler_sig" {
			t.Errorf("handler_sig should not be in default set: %s", e.QualifiedName)
		}
	}
	handlerOnly, _ := EntryPoints(st, []string{"handler_sig"}, false)
	for _, e := range handlerOnly {
		if e.Reason != "handler_sig" {
			t.Errorf("expected only handler_sig results, got %s with reason %s", e.QualifiedName, e.Reason)
		}
	}
}
