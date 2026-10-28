package query_test

import (
	"path/filepath"
	"strings"
	"testing"

	"codemap/query"
)

// A receiver name is only unique inside one package. Looking methods up by bare
// name reported every "Service" method in the index for each implementer.
//
// Only the interface's own methods belong in the report: a.Service also has
// Stop and Reset, which a.Runner does not declare and which therefore must not
// appear as implementing methods.
func TestInterfaceImplementationsScopesMethodsToImplementer(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-impls")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	impls, err := query.InterfaceImplementations(s, "example.com/iface-impls/a.Runner")
	if err != nil {
		t.Fatalf("InterfaceImplementations: %v", err)
	}

	byImpl := make(map[string][]string)
	for _, impl := range impls {
		byImpl[impl.ImplType] = append(byImpl[impl.ImplType], impl.ImplMethod)
	}

	service := byImpl["example.com/iface-impls/a.Service"]
	if len(service) != 1 || service[0] != "example.com/iface-impls/a.Service.Run" {
		t.Fatalf("a.Service methods = %v, want exactly a.Service.Run", service)
	}

	single := byImpl["example.com/iface-impls/a.Single"]
	if len(single) != 1 || single[0] != "example.com/iface-impls/a.Single.Run" {
		t.Fatalf("a.Single methods = %v, want exactly a.Single.Run", single)
	}

	for _, impl := range impls {
		if strings.Contains(impl.ImplMethod, "example.com/iface-impls/b.Service.") {
			t.Errorf("b.Service method leaked into the result: %s", impl.ImplMethod)
		}
	}
}

// An interface whose only method is unexported still defines a contract. Before
// the extractor indexed those declarations, the declared-method set came back
// empty and the query fell back to the implementer's whole method set.
func TestInterfaceImplementationsUnexportedMethod(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-unexported")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	impls, err := query.InterfaceImplementations(s, "example.com/iface-unexported.task")
	if err != nil {
		t.Fatalf("InterfaceImplementations: %v", err)
	}

	if len(impls) != 1 {
		t.Fatalf("rows = %d (%v), want exactly 1 (Job.getIDs)", len(impls), impls)
	}
	if got := impls[0].ImplMethod; got != "example.com/iface-unexported.Job.getIDs" {
		t.Errorf("ImplMethod = %q, want Job.getIDs", got)
	}
}

// A partially visible interface reports both methods and nothing else.
func TestInterfaceImplementationsPartialVisibility(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-unexported")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	impls, err := query.InterfaceImplementations(s, "example.com/iface-unexported.Store")
	if err != nil {
		t.Fatalf("InterfaceImplementations: %v", err)
	}

	got := make(map[string]bool, len(impls))
	for _, impl := range impls {
		got[impl.ImplMethod] = true
	}
	if len(impls) != 2 {
		t.Fatalf("rows = %d (%v), want exactly 2 (Save, reset)", len(impls), got)
	}
	for _, want := range []string{
		"example.com/iface-unexported.Cache.Save",
		"example.com/iface-unexported.Cache.reset",
	} {
		if !got[want] {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	if got["example.com/iface-unexported.Cache.Flush"] {
		t.Error("Cache.Flush is not declared by Store and must not be reported")
	}
}

// A union-only constraint declares no methods, so there is nothing to
// implement: the answer is empty, not the implementer's method set.
func TestInterfaceImplementationsUnionOnlyConstraint(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-unexported")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	impls, err := query.InterfaceImplementations(s, "example.com/iface-unexported.Number")
	if err != nil {
		t.Fatalf("InterfaceImplementations: %v", err)
	}
	if len(impls) != 0 {
		t.Fatalf("rows = %d (%v), want 0 for a method-less constraint", len(impls), impls)
	}
}

// An interface method is a declaration, not a callable: it has no callers by
// construction, so reachability alone would list it as dead code.
func TestUnusedExcludesInterfaceMethods(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/fixtures/iface-unexported")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	unused, err := query.UnusedSymbols(s)
	if err != nil {
		t.Fatalf("UnusedSymbols: %v", err)
	}
	for _, u := range unused {
		if strings.HasSuffix(u.QualifiedName, ".task.getIDs") ||
			strings.HasSuffix(u.QualifiedName, ".Store.reset") {
			t.Errorf("interface method reported as dead code: %s", u.QualifiedName)
		}
	}
}
