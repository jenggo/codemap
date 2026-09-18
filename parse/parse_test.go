package parse

import (
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pkgByPath(t *testing.T, pr *Result, importPath string) PackageInfo {
	t.Helper()
	for _, p := range pr.Packages {
		if p.ImportPath == importPath {
			return p
		}
	}
	t.Fatalf("package %q not found in %d packages", importPath, len(pr.Packages))
	return PackageInfo{}
}

// TestModuleModeSplitsExternalTests verifies that a module-mode directory with
// lib.go (package foo), an internal test (package foo) and an external test
// (package foo_test) yields two packages: the main package `foo` holding the
// internal test, and the external-test package `foo_test` (IsTest) holding the
// external test file.
func TestModuleModeSplitsExternalTests(t *testing.T) {
	pr, err := Run(filepath.Join("..", "testdata", "fixtures", "sep"))
	if err != nil {
		t.Fatal(err)
	}

	mainPkg := pkgByPath(t, pr, "example.com/sep")
	if mainPkg.IsTest {
		t.Fatalf("main package %q must not be IsTest", mainPkg.ImportPath)
	}
	if len(mainPkg.Files) != 2 {
		t.Fatalf("expected 2 files in main package (lib + internal test), got %v", mainPkg.Files)
	}
	if !hasFile(t, mainPkg.Files, "foo.go") || !hasFile(t, mainPkg.Files, "internal_test.go") {
		t.Fatalf("main package files not as expected: %v", mainPkg.Files)
	}

	testPkg := pkgByPath(t, pr, "example.com/sep_test")
	if !testPkg.IsTest {
		t.Fatalf("external test package %q must be IsTest", testPkg.ImportPath)
	}
	if testPkg.Name != "foo_test" {
		t.Fatalf("expected test package name foo_test, got %q", testPkg.Name)
	}
	if len(testPkg.Files) != 1 || !hasFile(t, testPkg.Files, "external_test.go") {
		t.Fatalf("external test package must contain only external_test.go, got %v", testPkg.Files)
	}
}

// TestDirWalkFallbackClassifiesByClause verifies the directory-walk fallback
// (no go list available) classifies internal vs external test files by their
// declared package clause.
func TestDirWalkFallbackClassifiesByClause(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("foo.go", "package walkpkg\ntype S struct{}\n")
	write("internal_test.go", "package walkpkg\nfunc helper() int { return 1 }\n")
	write("external_test.go", "package walkpkg_test\nfunc Make() walkpkg.S { return walkpkg.S{} }\n")

	pr, err := Run(dir)
	if err != nil {
		t.Fatal(err)
	}

	base := filepath.Base(dir)
	mainPkg := pkgByPath(t, pr, base)
	if len(mainPkg.Files) != 2 || !hasFile(t, mainPkg.Files, "foo.go") || !hasFile(t, mainPkg.Files, "internal_test.go") {
		t.Fatalf("dir-walk main package should hold lib + internal test, got %v", mainPkg.Files)
	}

	testPkg := pkgByPath(t, pr, base+"_test")
	if !testPkg.IsTest {
		t.Fatalf("dir-walk external test package must be IsTest")
	}
	if len(testPkg.Files) != 1 || !hasFile(t, testPkg.Files, "external_test.go") {
		t.Fatalf("dir-walk external test package should hold external_test.go, got %v", testPkg.Files)
	}
}

// TestDirWalkFallbackSetsFlag verifies that when go list fails and the
// dir-walk fallback is used, Result.Fallback is set to true.
func TestDirWalkFallbackSetsFlag(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("foo.go", "package flagtest\ntype T struct{}\n")

	pr, err := Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Fallback {
		t.Fatal("expected Fallback=true for dir-walk path, got false")
	}
}

// TestDirWalkSkipsNestedGoMod verifies that the dir-walk fallback skips
// directories containing a go.mod (nested module boundaries).
func TestDirWalkSkipsNestedGoMod(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("root.go", "package root\ntype Root struct{}\n")
	// Create a nested module boundary
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	write("nested/go.mod", "module example.com/nested\n")
	write("nested/nested.go", "package nested\ntype Nested struct{}\n")

	pr, err := Run(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The nested module should be skipped — only root.go should be parsed.
	for _, pkg := range pr.Packages {
		if pkg.ImportPath == "example.com/nested" || strings.Contains(pkg.ImportPath, "nested") {
			t.Fatalf("expected nested module to be skipped, but found package %q", pkg.ImportPath)
		}
	}
	if len(pr.Packages) != 1 {
		t.Fatalf("expected 1 package (root), got %d: %v", len(pr.Packages), pr.Packages)
	}
}

// TestDirWalkSiblingDisambiguation verifies that sibling directories with the
// same base name but in different module scopes get different import paths.
func TestDirWalkSiblingDisambiguation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Two sibling lib/ dirs under different module roots
	write("a/go.mod", "module example.com/a\n")
	write("a/lib/lib.go", "package lib\ntype A struct{}\n")

	write("b/go.mod", "module example.com/b\n")
	write("b/lib/lib.go", "package lib\ntype B struct{}\n")

	// Test nearestModulePath directly on each lib/ dir.
	aLib := filepath.Join(dir, "a", "lib")
	bLib := filepath.Join(dir, "b", "lib")

	aPath := nearestModulePath(aLib)
	bPath := nearestModulePath(bLib)

	if aPath != "example.com/a/lib" {
		t.Fatalf("expected example.com/a/lib, got %q", aPath)
	}
	if bPath != "example.com/b/lib" {
		t.Fatalf("expected example.com/b/lib, got %q", bPath)
	}
	if aPath == bPath {
		t.Fatalf("sibling lib/ dirs should get different import paths, both got %q", aPath)
	}
}

func hasFile(t *testing.T, files []string, base string) bool {
	t.Helper()
	for _, f := range files {
		if filepath.Base(f) == base {
			return true
		}
	}
	return false
}

// writeGoFiles creates dir (under a fresh temp root) and writes each name/src
// pair into it, returning the absolute directory.
func writeGoFiles(t *testing.T, dirName string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestGeneratedMarkerDetection verifies that the canonical leading
// "Code generated ... DO NOT EDIT." marker classifies a file as generated.
func TestGeneratedMarkerDetection(t *testing.T) {
	dir := writeGoFiles(t, "marker", map[string]string{
		"gen.go": "// Code generated by protoc-gen-go. DO NOT EDIT.\n\npackage marker\n\nfunc Generated() {}\n",
	})

	pr, err := Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.GeneratedFiles[filepath.Join(dir, "gen.go")] {
		t.Fatal("file with a leading Code generated marker must be classified generated")
	}
	if !pkgByPath(t, pr, "marker").IsGenerated {
		t.Fatal("package whose only file is generated must be IsGenerated")
	}
}

// TestGeneratedFilenameConventions verifies every generated-output filename
// convention the indexer recognizes.
func TestGeneratedFilenameConventions(t *testing.T) {
	names := []string{
		"foo.pb.go",
		"foo_string.go",
		"mock_store.go",
		"store_mock.go",
		"zz_generated_deepcopy.go",
	}
	files := make(map[string]string, len(names)+1)
	for _, name := range names {
		files[name] = "package conventions\n"
	}
	files["handwritten.go"] = "// Handwritten from a template, but maintained by hand.\n\npackage conventions\n"

	dir := writeGoFiles(t, "conventions", files)

	pr, err := Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !pr.GeneratedFiles[filepath.Join(dir, name)] {
			t.Errorf("%s must be classified generated by filename convention", name)
		}
	}
	if pr.GeneratedFiles[filepath.Join(dir, "handwritten.go")] {
		t.Error("handwritten.go must not be classified generated")
	}
	if pkgByPath(t, pr, "conventions").IsGenerated {
		t.Error("a package mixing generated and hand-written files must not be IsGenerated")
	}
}

// TestGeneratedMarkerMustLeadAndBeCanonical verifies that the marker rule
// requires the canonical form in the leading comment block: a comment after the
// package clause, a missing DO NOT EDIT. suffix, or incidental use of the word
// "generated" must not classify a file as generated.
func TestGeneratedMarkerMustLeadAndBeCanonical(t *testing.T) {
	dir := writeGoFiles(t, "notgen", map[string]string{
		"late.go":       "package notgen\n\n// Code generated by a tool. DO NOT EDIT.\nfunc Late() {}\n",
		"nosuffix.go":   "// Code generated by a tool.\n\npackage notgen\n\nfunc NoSuffix() {}\n",
		"incidental.go": "// Package notgen documents generated output that is hand-maintained.\npackage notgen\n",
	})

	pr, err := Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"late.go", "nosuffix.go", "incidental.go"} {
		if pr.GeneratedFiles[filepath.Join(dir, name)] {
			t.Errorf("%s must not be classified generated", name)
		}
	}
	if pkgByPath(t, pr, "notgen").IsGenerated {
		t.Error("package with only hand-written files must not be IsGenerated")
	}
}

func TestFileParsesSingleSource(t *testing.T) {
	fset := token.NewFileSet()
	f, err := File(fset, "sample.go", []byte("package sample\n\n// F does nothing.\nfunc F() {}\n"))
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if f.Name.Name != "sample" || len(f.Decls) != 1 {
		t.Fatalf("parsed name=%q decls=%d, want sample/1", f.Name.Name, len(f.Decls))
	}
	if len(f.Comments) == 0 {
		t.Error("File must parse comments (parser.ParseComments)")
	}
}

func TestFilePropagatesParseError(t *testing.T) {
	if _, err := File(token.NewFileSet(), "broken.go", []byte("package broken\nfunc {\n")); err == nil {
		t.Fatal("expected a parse error for malformed source")
	}
}
