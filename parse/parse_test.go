package parse

import (
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
