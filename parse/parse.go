package parse

import (
	"bufio"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type PackageInfo struct {
	ImportPath string
	Name       string
	Dir        string
	Files      []string
	IsTest     bool
}

type Error struct {
	File string
	Err  string
}

type Result struct {
	Packages []PackageInfo
	Files    map[string]*ast.File
	Fset     *token.FileSet
	Errors   []Error
}

func Run(pattern string) (*Result, error) {
	result := &Result{
		Files: make(map[string]*ast.File),
		Fset:  token.NewFileSet(),
	}

	absPattern, err := filepath.Abs(pattern)
	if err != nil {
		return nil, err
	}

	err = filepath.Walk(absPattern, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") || info.Name() == "vendor" {
			return filepath.SkipDir
		}
		return processDir(result, path)
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

func processDir(result *Result, dir string) error {
	bpkg, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil
	}

	importPath := bpkg.ImportPath
	if importPath == "." {
		importPath = readModulePath(dir)
		if importPath == "" {
			importPath = filepath.Base(dir)
		}
	}

	files := make([]string, 0, len(bpkg.GoFiles)+len(bpkg.CgoFiles))
	files = append(files, bpkg.GoFiles...)
	files = append(files, bpkg.CgoFiles...)
	testFiles := bpkg.TestGoFiles

	isTest := len(testFiles) > 0 && len(files) == 0

	allFiles := make([]string, 0, len(files)+len(testFiles))
	allFiles = append(allFiles, files...)
	allFiles = append(allFiles, testFiles...)

	if len(allFiles) == 0 {
		return nil
	}

	pkgInfo := PackageInfo{
		ImportPath: importPath,
		Name:       bpkg.Name,
		Dir:        dir,
		IsTest:     isTest,
	}

	for _, f := range allFiles {
		fullPath := filepath.Join(dir, f)
		astFile, err := parser.ParseFile(result.Fset, fullPath, nil, parser.ParseComments)
		if err != nil {
			result.Errors = append(result.Errors, Error{
				File: fullPath,
				Err:  err.Error(),
			})
			continue
		}
		result.Files[fullPath] = astFile
		pkgInfo.Files = append(pkgInfo.Files, fullPath)
	}

	if len(pkgInfo.Files) > 0 {
		result.Packages = append(result.Packages, pkgInfo)
	}

	if len(testFiles) > 0 && !isTest {
		processTestFiles(result, dir, importPath, bpkg.Name, testFiles)
	}

	return nil
}

func processTestFiles(result *Result, dir, importPath, pkgName string, testFiles []string) {
	testPkgInfo := PackageInfo{
		ImportPath: importPath + "_test",
		Name:       pkgName + "_test",
		Dir:        dir,
		IsTest:     true,
	}
	for _, f := range testFiles {
		fullPath := filepath.Join(dir, f)
		if _, exists := result.Files[fullPath]; exists {
			continue
		}
		astFile, err := parser.ParseFile(result.Fset, fullPath, nil, parser.ParseComments)
		if err != nil {
			result.Errors = append(result.Errors, Error{
				File: fullPath,
				Err:  err.Error(),
			})
			continue
		}
		result.Files[fullPath] = astFile
		testPkgInfo.Files = append(testPkgInfo.Files, fullPath)
	}
	if len(testPkgInfo.Files) > 0 {
		result.Packages = append(result.Packages, testPkgInfo)
	}
}

func readModulePath(dir string) string {
	gomod := filepath.Join(dir, "go.mod")
	f, err := os.Open(gomod)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}
