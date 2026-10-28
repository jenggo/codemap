package query

import (
	"os"
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
		if e.QualifiedName == "codemap/testdata/entrypoints/util.StrayExported" {
			hasStray = true
		}
		if e.QualifiedName == "codemap/testdata/entrypoints/cmd.ThisExportedIsCalled" {
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

// TestEntryPointsHandlerNameIsNameOnly pins that handler_name reports a
// lifecycle-named method whatever its signature: it is a name heuristic, and
// Run/Start/Serve are as often worker loops as HTTP handlers. handler_sig is
// the heuristic that proves a handler shape, and the two must stay distinct so
// a caller reading "handler_name" is not told it has an HTTP handler.
func TestEntryPointsHandlerNameIsNameOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/ep\n\ngo 1.21\n")
	writeFile("w.go", `package ep

import (
	"context"
	"net/http"
)

type Worker struct{}

// Run takes a context and is a worker loop, not an HTTP handler.
func (w *Worker) Run(ctx context.Context) error { return nil }

type API struct{}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {}

func (a *API) Handle(w http.ResponseWriter, r *http.Request) {}
`)
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	byName, err := EntryPoints(st, []string{"handler_name"}, false)
	if err != nil {
		t.Fatalf("EntryPoints(handler_name): %v", err)
	}
	var sawWorker bool
	for _, e := range byName {
		if e.Reason != "handler_name" {
			t.Errorf("%s: reason = %s, want handler_name only", e.QualifiedName, e.Reason)
		}
		if e.QualifiedName == "example.com/ep.Worker.Run" {
			sawWorker = true
		}
	}
	if !sawWorker {
		t.Error("handler_name should report the lifecycle-named Worker.Run")
	}

	bySig, err := EntryPoints(st, []string{"handler_sig"}, false)
	if err != nil {
		t.Fatalf("EntryPoints(handler_sig): %v", err)
	}
	for _, e := range bySig {
		if e.QualifiedName == "example.com/ep.Worker.Run" {
			t.Error("handler_sig must not report a worker loop as an HTTP handler")
		}
	}
}
