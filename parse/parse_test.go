package parse

import (
	"os"
	"path/filepath"
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

func hasFile(t *testing.T, files []string, base string) bool {
	t.Helper()
	for _, f := range files {
		if filepath.Base(f) == base {
			return true
		}
	}
	return false
}
