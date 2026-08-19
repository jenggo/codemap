package resolve

import (
	"codemap/parse"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAliasSymbolKind(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "alias")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	found := false
	for _, sym := range result.Symbols {
		if sym.Symbol.Name == "Helloer" && sym.Symbol.Kind == "alias" {
			found = true
			break
		}
	}
	if !found {
		t.Error("missing alias symbol Helloer with kind=alias")
	}
}

func TestAliasSatisfiesInterface(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "alias")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	found := false
	for _, e := range result.Edges {
		if e.Edge.EdgeType == "satisfies" &&
			e.Edge.FromRef == "example.com/alias.Base" &&
			e.Edge.ToRef == "example.com/alias.Greeter" {
			found = true
			break
		}
	}
	if !found {
		t.Error("missing satisfies edge: Base satisfies Greeter")
	}
}

func TestAliasStructField(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "alias")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	found := false
	for _, sym := range result.Symbols {
		if sym.Symbol.Name == "Container" && sym.Symbol.Kind == "type" {
			for _, f := range sym.Symbol.Fields {
				if f.GoName == "Name" {
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Error("missing Container.Name field")
	}
}

// TestAliasStructFieldCapture verifies that an alias to a named struct
// (type Outer = Inner) inherits the target's field shape.
func TestAliasStructFieldCapture(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "alias")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	found := false
	for _, sym := range result.Symbols {
		if sym.Symbol.Name == "Outer" && sym.Symbol.Kind == "alias" {
			found = true
			names := make(map[string]bool)
			for _, f := range sym.Symbol.Fields {
				names[f.GoName] = true
			}
			if !names["Name"] || !names["Age"] {
				t.Errorf("alias Outer should capture Inner fields, got %v", sym.Symbol.Fields)
			}
			break
		}
	}
	if !found {
		t.Error("missing alias symbol Outer with kind=alias")
	}
}
