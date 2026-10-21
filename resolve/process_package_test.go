package resolve

import (
	"path/filepath"
	"runtime"
	"testing"

	"codemap/parse"
)

// processPackage consumes and drops a package's types.Info, so a second pass
// over the same package sees nil. It must fall back to empty type information
// rather than panicking, and it must still extract symbols.
func TestProcessPackageTwiceIsSafe(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "nilpkg")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := newParseContext(pr)
	imp := newImporter(pr)
	imp.exports = loadExportIndex(pr)
	imp.onError = func(string, error) {}

	res := &Result{}
	for range 2 {
		processPackage(imp, ctx, pr.Packages[0], res)
	}
	if len(res.Symbols) == 0 {
		t.Fatal("expected symbols from both passes")
	}
}
