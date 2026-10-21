package resolve

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codemap/parse"
)

func ifaceImplsEdges(t *testing.T) map[string]bool {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "iface-impls")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	edges := make(map[string]bool)
	for _, e := range Run(pr).Edges {
		if e.Edge.EdgeType == "satisfies" {
			edges[e.Edge.FromRef+" -> "+e.Edge.ToRef] = true
		}
	}
	return edges
}

// Unexported implementers must be indexed: without them interface_impls
// answers "no implementations" for interfaces that plainly have one.
func TestSatisfiesIncludesUnexportedImplementers(t *testing.T) {
	edges := ifaceImplsEdges(t)

	for _, want := range []string{
		"example.com/iface-impls/store.publicHandler -> example.com/iface-impls/store.Handler",
		"example.com/iface-impls/store.secretHandler -> example.com/iface-impls/store.Handler",
		"example.com/iface-impls/api.localHandler -> example.com/iface-impls/store.Handler",
	} {
		if !edges[want] {
			t.Errorf("missing satisfies edge %q\ngot: %v", want, edges)
		}
	}
}

// An alias of an interface is the same type. Reporting it as a second edge
// double-counts every implementer in interface_impls and blast_radius.
func TestSatisfiesDedupesAliasedInterfaces(t *testing.T) {
	edges := ifaceImplsEdges(t)

	if !edges["example.com/iface-impls/store.PublicHandler -> example.com/iface-impls/store.Handler"] {
		t.Errorf("missing satisfies edge for the exported implementer\ngot: %v", edges)
	}
	for edge := range edges {
		if strings.HasSuffix(edge, " -> example.com/iface-impls/api.Handler") {
			t.Errorf("alias produced a duplicate edge: %q", edge)
		}
	}
	if !edges["example.com/iface-impls/store.PublicStore -> example.com/iface-impls/store.Store"] {
		t.Errorf("lost the exported implementer edge\ngot: %v", edges)
	}
}

// An alias of a type outside the index has no definition site to point at, so
// deduping must not drop the edge entirely.
func TestSatisfiesKeepsExternalAliasEdges(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "alias-external")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := "example.com/alias-external.MyHandler -> example.com/alias-external.Handler"
	for _, e := range Run(pr).Edges {
		if e.Edge.EdgeType == "satisfies" && e.Edge.FromRef+" -> "+e.Edge.ToRef == want {
			return
		}
	}
	t.Errorf("missing satisfies edge %q", want)
}

// A type embedding something the importer could not load has an unknown method
// set. go/types reports it as implementing every interface, which produced
// satisfies edges for completely unrelated types.
func TestSatisfiesSkipsUnresolvableEmbeds(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "unresolvable-embed")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range Run(pr).Edges {
		if e.Edge.EdgeType == "satisfies" {
			t.Errorf("false positive satisfies edge: %s -> %s", e.Edge.FromRef, e.Edge.ToRef)
		}
	}
}

// The interface side needs its own guard: an interface that embeds an
// unresolvable type reports Empty, and types.Implements then accepts every
// candidate. Covering only the implementer side would leave this path open.
func TestSatisfiesSkipsInterfacesWithUnresolvableEmbeds(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "unresolvable-embed")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const iface = "example.com/unresolvable.ForeignIface"
	for _, e := range Run(pr).Edges {
		if e.Edge.EdgeType == "satisfies" && e.Edge.ToRef == iface {
			t.Errorf("interface with an unresolvable embed accepted %s", e.Edge.FromRef)
		}
	}
}

// A package imported by another is checked on the import path before the
// top-level pass reaches it. Two distinct *types.Package instances for the same
// source made types.Identical reject the match, silently dropping the edge.
func TestSatisfiesAcrossPackageBoundary(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "crosspkg-identity")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := "example.com/crosspkg-identity/impl.Writer -> example.com/crosspkg-identity/iface.Writer"
	for _, e := range Run(pr).Edges {
		if e.Edge.EdgeType == "satisfies" && e.Edge.FromRef+" -> "+e.Edge.ToRef == want {
			return
		}
	}
	t.Errorf("missing satisfies edge %q", want)
}
