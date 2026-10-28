package store

import (
	"crypto/sha256"
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hashSource hashes the inputs an index is built from: the build context,
// go.mod, and every .go file keyed by its path relative to dir. Paths are hashed
// too, so an added, deleted or renamed file invalidates the index. known supplies
// contents the caller already holds (the files just indexed), so recording a
// fresh index hashes the bytes that were parsed rather than re-reading the disk.
//
// Every .go file is hashed, not only those the current build tags select: the
// staleness check must not run `go list` to find out what changed.
func hashSource(dir string, known map[string]string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	paths, err := goSourcePaths(abs)
	if err != nil {
		return "", err
	}
	// A file the indexer read but the walk no longer lists was deleted between
	// parsing and recording. Including it makes the next check mismatch and
	// rebuild, instead of leaving the deleted file's symbols in the index.
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		seen[path] = true
	}
	for path := range known {
		if !seen[path] && !skippedPath(abs, path) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	var b strings.Builder
	fmt.Fprintf(&b, "codemap-source-v1 %s %s %t %s\n",
		build.Default.GOOS, build.Default.GOARCH, build.Default.CgoEnabled,
		strings.Join(build.Default.BuildTags, ","))
	if mod, err := os.ReadFile(filepath.Join(abs, "go.mod")); err == nil {
		hashPart(&b, "go.mod", string(mod))
	}
	for _, path := range paths {
		content, ok := known[path]
		if !ok {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			content = string(data)
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return "", err
		}
		hashPart(&b, filepath.ToSlash(rel), content)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum), nil
}

func hashPart(b *strings.Builder, name, content string) {
	b.WriteString(name)
	b.WriteString("\x00")
	b.WriteString(content)
	b.WriteString("\x00")
}

// skippedPath reports whether path lies outside root or inside a directory the
// walk skips, so a caller-supplied file set cannot add paths the walk excludes.
func skippedPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	dir := root
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		if skipSourceDir(dir, part) {
			return true
		}
	}
	return false
}

// goSourcePaths lists every .go file under root, applying the go tool's
// directory rules: dot, underscore, testdata, vendor and nested-module
// directories are skipped.
func goSourcePaths(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipSourceDir(path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func skipSourceDir(path, name string) bool {
	if name == "vendor" || name == "node_modules" || name == "testdata" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil
}
