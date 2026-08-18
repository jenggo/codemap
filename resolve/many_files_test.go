package resolve

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"codemap/parse"
)

// manyFileResult builds a parse.Result with fileCount files in one package,
// where every function after the first calls the function in file 0, so the
// resolve walk produces cross-file edges.
func manyFileResult(t *testing.T, fileCount int) *parse.Result {
	t.Helper()
	fset := token.NewFileSet()
	pr := &parse.Result{
		Packages: []parse.PackageInfo{{
			ImportPath: "manypkg",
			Name:       "manypkg",
			Dir:        "/repo/manypkg",
			Files:      make([]string, fileCount),
		}},
		Files: make(map[string]*ast.File, fileCount),
		Fset:  fset,
	}
	for i := range fileCount {
		path := fmt.Sprintf("/repo/manypkg/f%d.go", i)
		var src string
		if i == 0 {
			src = "package manypkg\nfunc F0() int { return 0 }\n"
		} else {
			src = fmt.Sprintf("package manypkg\nfunc F%d() int { F0(); return 0 }\n", i)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		pr.Files[path] = f
		pr.Packages[0].Files[i] = path
	}
	return pr
}

// TestResolveManyFilesAttribution verifies the one-pass file→package build
// still produces the identical symbol and edge sets for a many-file package:
// one symbol per file, each attributed to its own file, with the expected
// cross-file call edges.
func TestResolveManyFilesAttribution(t *testing.T) {
	const fileCount = 100
	pr := manyFileResult(t, fileCount)
	res := Run(pr)

	if got := len(res.Symbols); got != fileCount {
		t.Fatalf("expected %d symbols (one per file), got %d", fileCount, got)
	}
	byFile := map[string]int{}
	for _, s := range res.Symbols {
		byFile[s.Symbol.Pos.File]++
	}
	for i := range fileCount {
		path := fmt.Sprintf("/repo/manypkg/f%d.go", i)
		if byFile[path] != 1 {
			t.Errorf("file %s contributed %d symbols, want 1", path, byFile[path])
		}
	}

	// A cross-file call edge exists from every later function into F0.
	callCount := 0
	for _, e := range res.Edges {
		if e.Edge.EdgeType == "calls" && e.Edge.ToRef == "manypkg.F0" {
			callCount++
		}
	}
	if callCount != fileCount-1 {
		t.Errorf("expected %d cross-file calls into F0, got %d", fileCount-1, callCount)
	}
}

// TestParseContextOnePassMaps verifies newParseContext builds the file→dir and
// AST→path maps in one pass, in the exact sizes the walkers rely on.
func TestParseContextOnePassMaps(t *testing.T) {
	pr := manyFileResult(t, 50)
	ctx := newParseContext(pr)

	if len(ctx.pkgFiles) != 1 || len(ctx.pkgFiles["manypkg"]) != 50 {
		t.Fatalf("pkgFiles has %d files in %d packages, want 50/1", len(ctx.pkgFiles["manypkg"]), len(ctx.pkgFiles))
	}
	if len(ctx.pathByAST) != 50 {
		t.Fatalf("pathByAST has %d entries, want 50", len(ctx.pathByAST))
	}
	for i := range 50 {
		path := fmt.Sprintf("/repo/manypkg/f%d.go", i)
		if ctx.fileToDir[path] != "/repo/manypkg" {
			t.Errorf("fileToDir[%s] = %q, want /repo/manypkg", path, ctx.fileToDir[path])
		}
		if ctx.pathByAST[ctx.files[path]] != path {
			t.Errorf("pathByAST round-trip failed for %s", path)
		}
	}
}
