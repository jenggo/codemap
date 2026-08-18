package query_test

import (
	"os"
	"path/filepath"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

func setupSepStore(t *testing.T) *store.Store {
	t.Helper()
	absPath, err := filepath.Abs("../testdata/fixtures/sep")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(absPath)
	if err != nil {
		t.Fatal(err)
	}
	res := resolve.Run(pr)

	files := make(map[string]string)
	for p := range pr.Files {
		if data, err := os.ReadFile(p); err == nil {
			files[filepath.Base(p)] = string(data)
		}
	}

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(res, files, nil); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIncludeTestsFiltersExternalTestPackage(t *testing.T) {
	s := setupSepStore(t)

	pkgs, err := query.ListPackages(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		if p.Path == "example.com/sep_test" {
			t.Fatalf("default ListPackages must exclude the _test package, found %q", p.Path)
		}
	}

	pkgsWith, err := query.ListPackages(s, query.WithTests())
	if err != nil {
		t.Fatal(err)
	}
	foundTestPkg := false
	for _, p := range pkgsWith {
		if p.Path == "example.com/sep_test" {
			foundTestPkg = true
			if !p.IsTest {
				t.Fatalf("sep_test package must carry IsTest=true")
			}
		}
	}
	if !foundTestPkg {
		t.Fatal("ListPackages with include_tests must surface the _test package")
	}

	res, err := query.Search(s, "ServiceConstructor")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.QualifiedName == "example.com/sep_test.ServiceConstructor" {
			t.Fatal("default search must exclude _test symbols")
		}
	}

	resWith, err := query.Search(s, "ServiceConstructor", query.WithTests())
	if err != nil {
		t.Fatal(err)
	}
	foundSym := false
	for _, r := range resWith {
		if r.QualifiedName == "example.com/sep_test.ServiceConstructor" {
			foundSym = true
		}
	}
	if !foundSym {
		t.Fatal("search with include_tests must surface _test symbols")
	}
}
