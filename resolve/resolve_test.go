package resolve

import (
	"path/filepath"
	"testing"

	"codemap/parse"
)

// TestLoadExportIndexResolvesDependencies verifies the batched export lookup
// finds compiled export data for a real dependency, so type-checking does not
// fall back to one `go list` process per import.
func TestLoadExportIndexResolvesDependencies(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/simple")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(absPath)
	if err != nil {
		t.Fatal(err)
	}
	exports := loadExportIndex(pr)
	if len(exports) == 0 {
		t.Fatal("expected the batched lookup to resolve at least one dependency")
	}
	for path, file := range exports {
		if file == "" {
			t.Errorf("dependency %q has no export data", path)
		}
	}
}
