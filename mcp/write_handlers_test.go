package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

// newWriteServer builds a write-enabled server over a copy of the simple
// fixture, so a test can mutate source files without touching testdata.
func newWriteServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "simple")
	if err := copyDir(dir); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	parseResult, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(resolveResult, parse.FileContents(parseResult), nil); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := st.SetIndexedAt(time.Now()); err != nil {
		t.Fatalf("set indexed_at: %v", err)
	}
	if err := st.SetRepoMeta(dir, "", len(resolveResult.Packages), len(resolveResult.Symbols)); err != nil {
		t.Fatalf("set repo meta: %v", err)
	}
	s := &Server{store: st, dbPath: dbPath, repoPath: dir, usage: newUsageStats()}
	return s.WithWrite(), filepath.Join(dir, "parser.go")
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeToolCalls() []struct {
	name string
	args map[string]any
} {
	qn := "codemap/testdata/simple.helper"
	return []struct {
		name string
		args map[string]any
	}{
		{toolReplaceSymbolBody, map[string]any{keyQualifiedName: qn, keyBody: "func helper(s string) string {\n\treturn s\n}"}},
		{toolInsertBefore, map[string]any{keyQualifiedName: qn, keyText: "func extra() {}"}},
		{toolInsertAfter, map[string]any{keyQualifiedName: qn, keyText: "func extra() {}"}},
		{toolSafeDelete, map[string]any{keyQualifiedName: qn}},
	}
}

// TestWriteToolsRefusedWithoutWriteMode asserts every write tool refuses and
// that no file is touched, which is the whole read-only guarantee.
func TestWriteToolsRefusedWithoutWriteMode(t *testing.T) {
	s, file := newWriteServer(t)
	s.writeEnabled = false
	before := readFileString(t, file)
	beforeInfo, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	for _, call := range writeToolCalls() {
		out, _, isErr := s.handleTool(call.name, call.args)
		if !isErr {
			t.Errorf("%s must be refused while write mode is off", call.name)
		}
		if !strings.Contains(out, "--allow-write") {
			t.Errorf("%s refusal must name --allow-write, got %q", call.name, out)
		}
	}

	if after := readFileString(t, file); after != before {
		t.Error("a refused write modified the file")
	}
	afterInfo, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Error("a refused write changed the file mtime")
	}
}

// TestWriteToolCatalogMarksMutations asserts the four tools are listed and each
// description says it modifies files.
func TestWriteToolCatalogMarksMutations(t *testing.T) {
	found := make(map[string]bool)
	for _, tool := range buildToolsList() {
		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)
		if !writeTools[name] {
			continue
		}
		found[name] = true
		if !strings.Contains(desc, "modifies files") {
			t.Errorf("%s description must state that it modifies files, got %q", name, desc)
		}
	}
	for _, name := range []string{toolReplaceSymbolBody, toolInsertBefore, toolInsertAfter, toolSafeDelete} {
		if !found[name] {
			t.Errorf("%s is missing from the tool catalog", name)
		}
	}
}

// TestWriteRefreshesIndex asserts a query after a write sees the new code.
func TestWriteRefreshesIndex(t *testing.T) {
	s, file := newWriteServer(t)
	out, _, isErr := s.handleTool(toolReplaceSymbolBody, map[string]any{
		keyQualifiedName: "codemap/testdata/simple.helper",
		keyBody:          "func helper(s string) string {\n\treturn s\n}\n\nfunc Added(s string) string {\n\treturn s\n}",
	})
	if isErr {
		t.Fatalf("replace failed: %s", out)
	}
	if strings.Contains(out, "reindex_error") {
		t.Fatalf("reindex failed: %s", out)
	}
	if !strings.Contains(readFileString(t, file), "func Added") {
		t.Fatal("write did not land in the file")
	}

	// The index must now describe the new declaration, not the old file.
	syms, err := s.store.SearchSymbolsByFile("parser.go", "", nil, false, store.GeneratedAny)
	if err != nil {
		t.Fatalf("search symbols: %v", err)
	}
	names := make(map[string]bool, len(syms))
	for _, sym := range syms {
		names[sym.Name] = true
	}
	if !names["Added"] {
		t.Errorf("index does not reflect the write; symbols = %v", names)
	}
}

// TestWriteConfinedToAllowedRoots asserts a symbol whose file resolves outside
// the allowed roots is refused before any write, including when the file inside
// the root is a symlink pointing out of it.
func TestWriteConfinedToAllowedRoots(t *testing.T) {
	s, file := newWriteServer(t)
	outside := filepath.Join(t.TempDir(), "outside.go")
	original := "package simple\n\nfunc helper(s string) string {\n\treturn s\n}\n"
	if err := os.WriteFile(outside, []byte(original), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// Replace the indexed file with a symlink that escapes the served root.
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := s.allowedWriteTarget(file); err == nil {
		t.Fatal("a symlink escaping the allowed roots must be refused")
	}

	out, _, isErr := s.handleTool(toolReplaceSymbolBody, map[string]any{
		keyQualifiedName: "codemap/testdata/simple.helper",
		keyBody:          "func helper(s string) string {\n\treturn \"escaped\"\n}",
	})
	if !isErr {
		t.Fatalf("write through an escaping symlink must be refused, got %s", out)
	}
	if !strings.Contains(out, "outside the allowed index roots") {
		t.Errorf("refusal must name the containment failure, got %q", out)
	}
	if after := readFileString(t, outside); after != original {
		t.Errorf("the escaped target was modified:\n%s", after)
	}
}

// TestSafeDeleteRefreshesIndex asserts a deletion removes the symbol from the
// index, not just from the file, so the next query does not answer from a
// stale row.
func TestSafeDeleteRefreshesIndex(t *testing.T) {
	s, _ := newWriteServer(t)
	out, _, isErr := s.handleTool(toolSafeDelete, map[string]any{keyQualifiedName: "codemap/testdata/simple.helper"})
	if isErr {
		t.Fatalf("delete failed: %s", out)
	}

	syms, err := s.store.SearchSymbolsByFile("parser.go", "", nil, false, store.GeneratedAny)
	if err != nil {
		t.Fatalf("search symbols: %v", err)
	}
	for _, sym := range syms {
		if sym.Name == "helper" {
			t.Fatalf("index still lists the deleted symbol %s", sym.QualifiedName)
		}
	}
}

// TestWriteReportsReindexFailure asserts a failed post-write reindex is
// surfaced on the result without failing the write itself.
func TestWriteReportsReindexFailure(t *testing.T) {
	s, file := newWriteServer(t)
	s.stalenessCheck = func(string, string) (bool, error) { return false, nil }
	s.indexFn = func(string) error { return errors.New("reindex exploded") }

	out, _, isErr := s.handleTool(toolReplaceSymbolBody, map[string]any{
		keyQualifiedName: "codemap/testdata/simple.helper",
		keyBody:          "func helper(s string) string {\n\treturn s + s\n}",
	})
	if isErr {
		t.Fatalf("the write itself succeeded, so the call must not fail: %s", out)
	}
	if !strings.Contains(out, "reindex_error") {
		t.Fatalf("a failed reindex must be reported, got %q", out)
	}
	if !strings.Contains(out, "reindex exploded") {
		t.Errorf("the report must name the reindex failure, got %q", out)
	}
	if !strings.Contains(readFileString(t, file), "return s + s") {
		t.Error("the write must still be applied when only the reindex failed")
	}
}

// TestSafeDeleteRefusesExported asserts the exported refusal surfaces through
// the tool with the override named.
func TestSafeDeleteRefusesExported(t *testing.T) {
	s, file := newWriteServer(t)
	out, _, isErr := s.handleTool(toolSafeDelete, map[string]any{keyQualifiedName: "codemap/testdata/simple.NewParser"})
	if !isErr {
		t.Fatalf("expected an exported-symbol refusal, got %s", out)
	}
	if !strings.Contains(out, "allow_exported") {
		t.Errorf("refusal must name allow_exported, got %q", out)
	}
	if !strings.Contains(readFileString(t, file), "func NewParser") {
		t.Error("exported symbol must survive a refused delete")
	}
}

// newRenameServer builds a write-enabled server over the rename fixture, which
// spans three packages so a rename has cross-package sites to rewrite.
func newRenameServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rename")
	if err := copyTreeDir("../testdata/fixtures/rename", dir); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	parseResult, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(resolveResult, parse.FileContents(parseResult), nil); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := st.SetIndexedAt(time.Now()); err != nil {
		t.Fatalf("set indexed_at: %v", err)
	}
	if err := st.SetRepoMeta(dir, "", len(resolveResult.Packages), len(resolveResult.Symbols)); err != nil {
		t.Fatalf("set repo meta: %v", err)
	}
	s := &Server{store: st, dbPath: dbPath, repoPath: dir, usage: newUsageStats()}
	return s.WithWrite(), dir
}

func copyTreeDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

// TestRenameSymbolRefreshesIndex asserts the rename tool rewrites files across
// packages and leaves the index resolving only the new name.
func TestRenameSymbolRefreshesIndex(t *testing.T) {
	s, dir := newRenameServer(t)
	out, _, isErr := s.handleTool(toolRenameSymbol, map[string]any{
		keyQualifiedName: "renamefixture/alpha.Target",
		"new_name":       "NewTarget",
	})
	if isErr {
		t.Fatalf("rename failed: %s", out)
	}
	if strings.Contains(out, "reindex_error") {
		t.Fatalf("reindex failed: %s", out)
	}
	if !strings.Contains(readFileString(t, filepath.Join(dir, "gamma/gamma.go")), "alpha.NewTarget()") {
		t.Error("the cross-package site was not rewritten")
	}

	if _, err := s.store.SymbolByName("renamefixture/alpha.Target"); err == nil {
		t.Error("the old qualified name still resolves after a rename")
	}
	if _, err := s.store.SymbolByName("renamefixture/alpha.NewTarget"); err != nil {
		t.Errorf("the new qualified name does not resolve after a rename: %v", err)
	}
}

// TestRenameSymbolRefusedWithoutWriteMode asserts rename is gated like every
// other mutating tool and touches no file while the gate is off.
func TestRenameSymbolRefusedWithoutWriteMode(t *testing.T) {
	s, dir := newRenameServer(t)
	s.writeEnabled = false
	before := readFileString(t, filepath.Join(dir, "alpha/alpha.go"))

	out, _, isErr := s.handleTool(toolRenameSymbol, map[string]any{
		keyQualifiedName: "renamefixture/alpha.Target",
		"new_name":       "NewTarget",
	})
	if !isErr {
		t.Fatalf("expected a refusal while write mode is off, got %s", out)
	}
	if !strings.Contains(out, "--allow-write") {
		t.Errorf("refusal must name --allow-write, got %q", out)
	}
	if after := readFileString(t, filepath.Join(dir, "alpha/alpha.go")); after != before {
		t.Error("a refused rename modified the file")
	}
}

// TestRenameSymbolConfinedToAllowedRoots asserts a rename whose site escapes the
// allowed roots is refused before any file is written.
func TestRenameSymbolConfinedToAllowedRoots(t *testing.T) {
	s, dir := newRenameServer(t)
	site := filepath.Join(dir, "gamma/gamma.go")
	original := readFileString(t, site)
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte(original), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Remove(site); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, site); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	before := readFileString(t, filepath.Join(dir, "alpha/alpha.go"))

	out, _, isErr := s.handleTool(toolRenameSymbol, map[string]any{
		keyQualifiedName: "renamefixture/alpha.Target",
		"new_name":       "NewTarget",
	})
	if !isErr {
		t.Fatalf("expected a containment refusal, got %s", out)
	}
	if !strings.Contains(out, "outside the allowed index roots") {
		t.Errorf("refusal must name the containment failure, got %q", out)
	}
	if after := readFileString(t, filepath.Join(dir, "alpha/alpha.go")); after != before {
		t.Error("a refused rename modified another file")
	}
	if after := readFileString(t, outside); after != original {
		t.Error("the escaped target was modified")
	}
}

// TestFindDeclarationAvailableWithoutWriteMode asserts the read-only position
// lookup works on a server with write mode off, which is the whole read-only
// guarantee for the new tool.
func TestFindDeclarationAvailableWithoutWriteMode(t *testing.T) {
	s, dir := newRenameServer(t)
	s.writeEnabled = false

	out, _, isErr := s.handleTool(toolFindDeclaration, map[string]any{
		keyFilePath: filepath.Join(dir, "alpha/alpha.go"),
		keyLine:     float64(21),
	})
	if isErr {
		t.Fatalf("find_declaration must work without write mode: %s", out)
	}
	if !strings.Contains(out, "renamefixture/alpha.SameLine") {
		t.Errorf("expected the enclosing function, got %s", out)
	}
}
