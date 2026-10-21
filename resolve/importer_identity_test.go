package resolve

import (
	"path/filepath"
	"runtime"
	"testing"

	"codemap/parse"
)

// A package imported by another is checked on the import path before the
// top-level pass reaches it. Both paths must hand back the same instance, or
// types.Identical rejects matches across the boundary.
func TestImporterReusesCheckedPackage(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "crosspkg-identity")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := newParseContext(pr)
	imp := newImporter(pr)
	imp.exports = loadExportIndex(pr)
	imp.onError = func(string, error) {}

	const shared = "example.com/crosspkg-identity/shared"
	imported, _, err := imp.packageFor(shared)
	if err != nil {
		t.Fatalf("packageFor(%s): %v", shared, err)
	}
	viaImport, err := imp.Import(shared)
	if err != nil {
		t.Fatalf("Import(%s): %v", shared, err)
	}
	if imported != viaImport {
		t.Error("Import returned a different *types.Package than packageFor for the same path")
	}

	for _, pkgInfo := range pr.Packages {
		if pkgInfo.ImportPath != shared {
			continue
		}
		if got := processPackage(imp, ctx, pkgInfo, &Result{}); got != imported {
			t.Error("the top-level pass checked the package again instead of reusing the imported instance")
		}
	}
}
