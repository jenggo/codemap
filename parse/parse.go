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
	// ModulePath is the module path this package is attributed to. Empty in
	// single-repo mode; set by workspace indexing to the nearest enclosing
	// go.mod's module path.
	ModulePath string
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
	Fallback bool // true when go list failed and the dir-walk fallback was used
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
	result, err := runDirWalk(absPattern)
	if err != nil {
		return nil, err
	}
	result.Fallback = true
	return result, nil
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

// WorkspaceRoot is one module tree to index in workspace mode. ModulePath is
// the canonical repo identity (nearest-enclosing go.mod's module path).
type WorkspaceRoot struct {
	Dir        string
	ModulePath string
}

// RunWorkspace indexes every module root into one Result, attributing each
// package to the nearest enclosing go.mod. Nested go.mod modules that are not
// themselves roots are indexed too, so module boundaries are never crossed.
// All files share a single FileSet so cross-package references resolve.
func RunWorkspace(roots []WorkspaceRoot) (*Result, error) {
	result := &Result{
		Files: make(map[string]*ast.File),
		Fset:  token.NewFileSet(),
	}
	seen := make(map[string]bool)

	rootSet := make(map[string]bool)
	for _, r := range roots {
		abs, err := filepath.Abs(r.Dir)
		if err != nil {
			return nil, err
		}
		rootSet[abs] = true
	}

	for _, root := range roots {
		if err := runWorkspaceRoot(result, root, rootSet, seen); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func runWorkspaceRoot(result *Result, root WorkspaceRoot, rootSet, seen map[string]bool) error {
	absRoot, err := filepath.Abs(root.Dir)
	if err != nil {
		return err
	}

	nested := discoverNestedModules(absRoot)

	if err := goListInto(result, absRoot, root.ModulePath, seen); err == nil {
		for _, nd := range nested {
			if rootSet[nd] {
				continue
			}
			module := readModulePath(nd)
			if module == "" {
				continue
			}
			if err := goListInto(result, nd, module, seen); err != nil {
				result.Errors = append(result.Errors, Error{File: nd, Err: err.Error()})
			}
		}
		return nil
	}

	result.Fallback = true
	return runModuleWalk(result, absRoot, root.ModulePath)
}

// discoverNestedModules returns the absolute paths of directories under root
// that contain their own go.mod (excluding root itself), so module boundaries
// are respected during indexing.
func discoverNestedModules(absRoot string) []string {
	var nested []string
	_ = filepath.Walk(absRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() || path == absRoot {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") || info.Name() == dirVendor || info.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			nested = append(nested, path)
			return filepath.SkipDir
		}
		return nil
	})
	return nested
}

// runModuleWalk is the workspace fallback (no go toolchain): walk the module
// tree, skipping nested go.mod boundaries, attributing every package to the
// module's path. Import paths are the module path plus the package's relative
// directory, so namespacing holds without go list.
func runModuleWalk(result *Result, moduleRoot, modulePath string) error {
	err := filepath.Walk(moduleRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") || info.Name() == dirVendor {
			return filepath.SkipDir
		}
		if path != moduleRoot {
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
		}
		return processWorkspaceDir(result, path, moduleRoot, modulePath)
	})
	return err
}

func processWorkspaceDir(result *Result, dir, moduleRoot, modulePath string) error {
	bpkg, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil
	}

	rel, err := filepath.Rel(moduleRoot, dir)
	if err != nil {
		return nil
	}
	rel = filepath.ToSlash(rel)
	importPath := modulePath
	if rel != "." {
		importPath = modulePath + "/" + rel
	}

	files := make([]string, 0, len(bpkg.GoFiles)+len(bpkg.CgoFiles)+len(bpkg.TestGoFiles))
	files = append(files, bpkg.GoFiles...)
	files = append(files, bpkg.CgoFiles...)
	files = append(files, bpkg.TestGoFiles...)
	externalTestFiles := bpkg.XTestGoFiles

	if len(files) == 0 && len(externalTestFiles) == 0 {
		return nil
	}

	pkgInfo := PackageInfo{
		ImportPath: importPath,
		Name:       bpkg.Name,
		Dir:        dir,
		IsTest:     len(bpkg.GoFiles)+len(bpkg.CgoFiles) == 0,
		ModulePath: modulePath,
	}

	parseFilesInto(result, &pkgInfo, files, dir)

	if len(pkgInfo.Files) > 0 {
		result.Packages = append(result.Packages, pkgInfo)
	}

	if len(externalTestFiles) > 0 {
		processTestFilesWorkspace(result, dir, importPath, bpkg.Name, externalTestFiles, modulePath, nil)
	}

	return nil
}

// parseFilesInto parses each file into result.Files and records it on pkgInfo,
// skipping files already parsed (shared FileSet across workspace members).
func parseFilesInto(result *Result, pkgInfo *PackageInfo, files []string, dir string) {
	for _, f := range files {
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
		pkgInfo.Files = append(pkgInfo.Files, fullPath)
	}
}

func processTestFilesWorkspace(result *Result, dir, importPath, pkgName string, externalTestFiles []string, modulePath string, seen map[string]bool) {
	testPkgInfo := PackageInfo{
		ImportPath: importPath + "_test",
		Name:       pkgName + "_test",
		Dir:        dir,
		IsTest:     true,
		ModulePath: modulePath,
	}
	for _, f := range externalTestFiles {
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
		if seen == nil || !seen[testPkgInfo.ImportPath] {
			result.Packages = append(result.Packages, testPkgInfo)
			if seen != nil {
				seen[testPkgInfo.ImportPath] = true
			}
		}
	}
}

// goListInto runs `go list -e -json ./...` in abs and merges the packages into
// result, attributing them to modulePath. seen dedupes packages by import path
// so nested or overlapping module runs don't double-index.
func goListInto(result *Result, abs, modulePath string, seen map[string]bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-json", "./...")
	cmd.Dir = abs
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go list: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	dec := json.NewDecoder(&stdout)
	for {
		var p goListPkg
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decoding go list output: %w", err)
		}
		if p.Error != nil {
			result.Errors = append(result.Errors, Error{File: p.Dir, Err: p.Error.Err})
			continue
		}
		for _, de := range p.DepsErrors {
			result.Errors = append(result.Errors, Error{File: p.Dir, Err: de.Err})
		}
		addGoListPkgInto(result, p, modulePath, seen)
	}
	if len(result.Packages) == 0 && len(result.Errors) > 0 {
		return fmt.Errorf("go list: %s", result.Errors[0].Err)
	}
	return nil
}

func addGoListPkgInto(result *Result, p goListPkg, modulePath string, seen map[string]bool) {
	files := make([]string, 0, len(p.GoFiles)+len(p.CgoFiles)+len(p.TestGoFiles))
	files = append(files, p.GoFiles...)
	files = append(files, p.CgoFiles...)
	files = append(files, p.TestGoFiles...)

	if len(files) == 0 && len(p.XTestGoFiles) == 0 {
		return
	}

	pkgInfo := PackageInfo{
		ImportPath: p.ImportPath,
		Name:       p.Name,
		Dir:        p.Dir,
		IsTest:     len(p.GoFiles)+len(p.CgoFiles) == 0,
		ModulePath: modulePath,
	}

	for _, f := range files {
		fullPath := filepath.Join(p.Dir, f)
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
		pkgInfo.Files = append(pkgInfo.Files, fullPath)
	}

	if len(pkgInfo.Files) > 0 {
		if !seen[pkgInfo.ImportPath] {
			result.Packages = append(result.Packages, pkgInfo)
			seen[pkgInfo.ImportPath] = true
		}
	}

	if len(p.XTestGoFiles) > 0 {
		processTestFilesWorkspace(result, p.Dir, p.ImportPath, p.Name, p.XTestGoFiles, modulePath, seen)
	}
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
	files := make([]string, 0, len(p.GoFiles)+len(p.CgoFiles)+len(p.TestGoFiles))
	files = append(files, p.GoFiles...)
	files = append(files, p.CgoFiles...)
	files = append(files, p.TestGoFiles...)

	if len(files) == 0 && len(p.XTestGoFiles) == 0 {
		return
	}

	pkgInfo := PackageInfo{
		ImportPath: p.ImportPath,
		Name:       p.Name,
		Dir:        p.Dir,
		IsTest:     len(p.GoFiles)+len(p.CgoFiles) == 0,
	}

	for _, f := range files {
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

	if len(p.XTestGoFiles) > 0 {
		processTestFiles(result, p.Dir, p.ImportPath, p.Name, p.XTestGoFiles)
	}
}

const dirVendor = "vendor"

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
		if strings.HasPrefix(info.Name(), ".") || info.Name() == dirVendor {
			return filepath.SkipDir
		}
		// Skip nested go.mod boundaries — each module is an independent
		// compilation unit and must be indexed via its own Run call or
		// through the workspace path, not as part of a parent walk.
		if path != absPattern {
			if _, serr := os.Stat(filepath.Join(path, "go.mod")); serr == nil {
				return filepath.SkipDir
			}
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
		// When build.ImportDir cannot determine the import path, walk up
		// to the nearest enclosing go.mod to find the module path, then
		// append the relative directory. This disambiguates sibling
		// directories with the same basename (e.g. lib/ vs vendor/lib/).
		importPath = nearestModulePath(dir)
		if importPath == "" {
			importPath = filepath.Base(dir)
		}
	}

	files := make([]string, 0, len(bpkg.GoFiles)+len(bpkg.CgoFiles)+len(bpkg.TestGoFiles))
	files = append(files, bpkg.GoFiles...)
	files = append(files, bpkg.CgoFiles...)
	files = append(files, bpkg.TestGoFiles...)
	externalTestFiles := bpkg.XTestGoFiles

	if len(files) == 0 && len(externalTestFiles) == 0 {
		return nil
	}

	pkgInfo := PackageInfo{
		ImportPath: importPath,
		Name:       bpkg.Name,
		Dir:        dir,
		IsTest:     len(bpkg.GoFiles)+len(bpkg.CgoFiles) == 0,
	}

	for _, f := range files {
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

	if len(externalTestFiles) > 0 {
		processTestFiles(result, dir, importPath, bpkg.Name, externalTestFiles)
	}

	return nil
}

func processTestFiles(result *Result, dir, importPath, pkgName string, externalTestFiles []string) {
	testPkgInfo := PackageInfo{
		ImportPath: importPath + "_test",
		Name:       pkgName + "_test",
		Dir:        dir,
		IsTest:     true,
	}
	for _, f := range externalTestFiles {
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

// nearestModulePath walks up from dir to find the nearest enclosing go.mod,
// reads its module path, and appends the relative directory. This
// disambiguates sibling directories with the same basename (e.g. lib/ in
// different modules). Returns "" when no enclosing go.mod is found.
func nearestModulePath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	candidate := abs
	for {
		mod := readModulePath(candidate)
		if mod != "" {
			rel, rerr := filepath.Rel(candidate, abs)
			if rerr != nil {
				return mod
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return mod
			}
			return mod + "/" + rel
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
		candidate = parent
	}
	return ""
}
