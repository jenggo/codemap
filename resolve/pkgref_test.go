package resolve

import (
	"codemap/parse"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPackageLevelCallEdges(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "pkgref")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	found := false
	for _, e := range result.Edges {
		if e.Edge.EdgeType == "calls" &&
			e.Edge.FromRef == "example.com/pkgref.Default" &&
			e.Edge.ToRef == "example.com/pkgref.Helper" {
			found = true
			break
		}
	}
	if !found {
		t.Error("missing calls edge: Default -> Helper")
	}
}

func TestPackageLevelReferenceEdges(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "pkgref")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	found := false
	for _, e := range result.Edges {
		if e.Edge.EdgeType == "references" &&
			e.Edge.FromRef == "example.com/pkgref.Make" &&
			e.Edge.ToRef == "example.com/pkgref.Config" {
			found = true
			break
		}
	}
	if !found {
		t.Error("missing references edge: Make -> Config")
	}
}
