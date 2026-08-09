package parse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// Run resolves the Go packages under pattern. It prefers `go list` (the same
// authoritative discovery as `go build ./...`), falling back to a directory
// walk when the go toolchain is unavailable or pattern is not inside a module.
func Run(pattern string) (*Result, error) {
	absPattern, err := filepath.Abs(pattern)
	if err != nil {
		return nil, err
	}
	if result, err := goList(absPattern); err == nil {
		return result, nil
	}
	return runDirWalk(absPattern)
}

// FileContents reads the source of every parsed file, keyed by absolute path.
// It feeds the file-content search index (search_text / file_content_fts).
func FileContents(pr *Result) map[string]string {
	files := make(map[string]string, len(pr.Files))
	for path := range pr.Files {
		data, err := os.ReadFile(path)
		if err == nil {
			files[path] = string(data)
		}
	}
	return files
}

type goListPkg struct {
	Dir          string
	ImportPath   string
	Name         string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	Error        *goListError
	DepsErrors   []goListError
}

type goListError struct {
	Err string
}

// goList resolves packages with `go list -e -json ./...`. The -e flag keeps
// packages with load errors in the output (recorded in Result.Errors) instead
// of aborting the whole command.
func goList(abs string) (*Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-json", "./...")
	cmd.Dir = abs
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	result := &Result{
		Files: make(map[string]*ast.File),
		Fset:  token.NewFileSet(),
	}
	dec := json.NewDecoder(&stdout)
	for {
		var p goListPkg
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		if p.Error != nil {
			result.Errors = append(result.Errors, Error{File: p.Dir, Err: p.Error.Err})
			continue
		}
		for _, de := range p.DepsErrors {
			result.Errors = append(result.Errors, Error{File: p.Dir, Err: de.Err})
		}
		addGoListPkg(result, p)
	}
	if len(result.Packages) == 0 && len(result.Errors) > 0 {
		// go list -e tolerates failures, so a non-module directory yields a
		// zero-package stub instead of a command error. Treat that as failure
		// to let Run fall back to the directory walk.
		return nil, fmt.Errorf("go list: %s", result.Errors[0].Err)
	}
	return result, nil
}

func addGoListPkg(result *Result, p goListPkg) {
	files := make([]string, 0, len(p.GoFiles)+len(p.CgoFiles))
	files = append(files, p.GoFiles...)
	files = append(files, p.CgoFiles...)
	testFiles := make([]string, 0, len(p.TestGoFiles)+len(p.XTestGoFiles))
	testFiles = append(testFiles, p.TestGoFiles...)
	testFiles = append(testFiles, p.XTestGoFiles...)

	isTest := len(testFiles) > 0 && len(files) == 0

	allFiles := make([]string, 0, len(files)+len(testFiles))
	allFiles = append(allFiles, files...)
	allFiles = append(allFiles, testFiles...)

	if len(allFiles) == 0 {
		return
	}

	pkgInfo := PackageInfo{
		ImportPath: p.ImportPath,
		Name:       p.Name,
		Dir:        p.Dir,
		IsTest:     isTest,
	}

	for _, f := range allFiles {
		fullPath := filepath.Join(p.Dir, f)
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
		processTestFiles(result, p.Dir, p.ImportPath, p.Name, testFiles)
	}
}

func runDirWalk(pattern string) (*Result, error) {
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
