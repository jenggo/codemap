package query_test

import (
	"path/filepath"
	"strings"
	"testing"

	"codemap/query"
)

// A receiver name is only unique inside one package. Looking methods up by bare
// name reported every "Service" method in the index for each implementer.
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
	if len(service) != 3 {
		t.Fatalf("a.Service methods = %v, want exactly Run, Stop, Reset", service)
	}
	for _, m := range service {
		if !strings.HasPrefix(m, "example.com/iface-impls/a.Service.") {
			t.Errorf("method %q does not belong to a.Service", m)
		}
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
