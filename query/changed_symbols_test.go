package query

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

func setupGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		c := exec.CommandContext(context.Background(), args[0], args[1:]...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %s: %v", args, string(out), err)
		}
	}
	return repo
}

func writeFile0644(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.CommandContext(context.Background(), "git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", args, string(out), err)
	}
}

func indexRepo(t *testing.T, root string) *store.Store {
	t.Helper()
	parseResult, err := parse.Run(root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

func TestChangedSymbolsModified(t *testing.T) {
	repo := setupGitRepo(t)
	srcDir := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(srcDir, "lib.go"), "package pkg\n\n// Greet returns hi.\nfunc Greet() string { return \"hi\" }\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(srcDir, "lib.go"), "package pkg\n\n// Greet returns hi there.\nfunc Greet() string { return \"hello\" }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if result.Summary.Modified == 0 {
		t.Errorf("expected at least one modified symbol, got %+v", result.Summary)
	}
	hasGreet := false
	for _, sym := range result.Symbols {
		t.Logf("symbol: %s type=%s", sym.QualifiedName, sym.ChangeType)
		if strings.Contains(sym.QualifiedName, "Greet") {
			hasGreet = true
			if sym.ChangeType != "modified" {
				t.Errorf("expected Greet change_type=modified, got %s", sym.ChangeType)
			}
		}
	}
	if !hasGreet {
		t.Errorf("expected Greet in result symbols")
	}
}

func TestChangedSymbolsAdded(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "b.go"), "package main\n\nfunc beta() {}\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if result.Summary.Added == 0 {
		t.Errorf("expected at least one added symbol, got %+v", result.Summary)
	}
	hasBeta := false
	for _, sym := range result.Symbols {
		if strings.Contains(sym.QualifiedName, "beta") {
			hasBeta = true
			if sym.ChangeType != "added" {
				t.Errorf("expected beta change_type=added, got %s", sym.ChangeType)
			}
		}
	}
	if !hasBeta {
		t.Errorf("expected beta in result")
	}
}

func TestChangedSymbolsDeleted(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	writeFile0644(t, filepath.Join(repo, "z.go"), "package main\n\nfunc zeta() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	if err := os.Remove(filepath.Join(repo, "z.go")); err != nil {
		t.Fatalf("rm: %v", err)
	}
	runGitIn(t, repo, "add", "-A")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if result.Summary.Removed == 0 {
		t.Errorf("expected at least one removed symbol, got %+v", result.Summary)
	}
}

func TestChangedSymbolsIncludeBodies(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, true, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	hasBody := false
	for _, sym := range result.Symbols {
		if sym.Body != "" {
			hasBody = true
		}
	}
	if !hasBody {
		t.Errorf("expected non-empty body when include_bodies=true")
	}
}

func TestChangedSymbolsWithBlastDisabled(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	for _, sym := range result.Symbols {
		if sym.BlastRadius != nil {
			t.Errorf("expected nil BlastRadius when with_blast_radius=false, got %+v", sym.BlastRadius)
		}
	}
}

func TestChangedSymbolsNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	writeFile0644(t, filepath.Join(dir, "a.go"), "package main\n")
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = s.Close() }()
	_, err = ChangedSymbols(s, dir, "HEAD", false, false, false)
	if err == nil {
		t.Fatal("expected error for non-git repo")
	}
}

func TestChangedSymbolsRefNotFound(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	_, err := ChangedSymbols(s, repo, "nonexistent-ref-xyz", false, false, false)
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
}

func TestChangedSymbolsDefaultBranch(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/changed\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	// git init in this environment may default to master, not main. An empty
	// ref must resolve to the repository's default branch instead of failing.
	branchOut, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err == nil && strings.TrimSpace(string(branchOut)) == "main" {
		t.Skip("default branch is main; not exercising the fallback")
	}

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() { return }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols with default ref failed: %v", err)
	}
	if result.Summary.Modified == 0 {
		t.Errorf("expected at least one modified symbol with default ref, got %+v", result.Summary)
	}
}

// TestChangedSymbolsReportsRename verifies a moved file surfaces as a rename on
// the result, not only as a modified symbol: the symbol's PosFile is the new
// path, so without this the old path callers still reference is lost.
// Fixtures carry distinct content because git pairs renames by similarity.
func TestChangedSymbolsReportsRename(t *testing.T) {
	repo := setupGitRepo(t)
	srcDir := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/rename\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(srcDir, "old.go"), "package pkg\n\nfunc Moved() string { return \"moved\" }\n")
	writeFile0644(t, filepath.Join(srcDir, "keep.go"), "package pkg\n\nfunc Keep() string { return \"keep\" }\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	runGitIn(t, repo, "mv", filepath.Join("pkg", "old.go"), filepath.Join("pkg", "new.go"))

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}

	if result.Summary.Renamed != 1 {
		t.Fatalf("expected 1 rename, got %d (renamed=%+v)", result.Summary.Renamed, result.Renamed)
	}
	got := result.Renamed[0]
	if filepath.Base(got.From) != "old.go" || filepath.Base(got.To) != "new.go" {
		t.Errorf("expected old.go -> new.go, got %q -> %q", got.From, got.To)
	}
}

// TestChangedSymbolsAppendIsAdded pins the distinction between an append and an
// edit. git reports an insertion as a zero-length old range positioned before
// the hunk, so a hunk-overlap test classified a brand-new function as modified
// and the "added" counter never moved.
func TestChangedSymbolsAppendIsAdded(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/append\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	// Append only: alpha is untouched, beta is new.
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n\nfunc beta() {}\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	got := map[string]string{}
	for _, sym := range result.Symbols {
		got[sym.QualifiedName] = sym.ChangeType
	}
	if got["example.com/append.beta"] != changeTypeAdded {
		t.Errorf("beta = %q, want added (all: %v)", got["example.com/append.beta"], got)
	}
	if ct, ok := got["example.com/append.alpha"]; ok {
		t.Errorf("alpha was not touched but reported as %q", ct)
	}
}

// TestChangedSymbolsRenameIsRenamed pins that a rename is reported as one
// renamed symbol carrying the old name, not as a modification of the new name
// plus a phantom removal of the old one.
func TestChangedSymbolsRenameIsRenamed(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/ren\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc oldName(v int) int { return v + 1 }\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc newName(v int) int { return v + 1 }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if len(result.Symbols) != 1 {
		t.Fatalf("expected exactly one changed symbol, got %+v", result.Symbols)
	}
	sym := result.Symbols[0]
	if sym.ChangeType != changeTypeRenamed {
		t.Fatalf("change_type = %q, want renamed", sym.ChangeType)
	}
	if sym.QualifiedName != "example.com/ren.newName" || sym.RenamedFrom != "example.com/ren.oldName" {
		t.Errorf("rename = %q from %q, want newName from oldName", sym.QualifiedName, sym.RenamedFrom)
	}
	if result.Summary.Renamed != 1 || result.Summary.Added != 0 || result.Summary.Removed != 0 {
		t.Errorf("summary = %+v, want one rename and nothing else", result.Summary)
	}
}

// TestChangedSymbolsRemovedSurvivesInResult pins that deleting a function
// reports it. The symbol is gone from the index, so only the ref-side parse can
// name it.
func TestChangedSymbolsRemovedSurvivesInResult(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/rm\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc keep() {}\n\nfunc gone() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc keep() {}\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	got := map[string]string{}
	for _, sym := range result.Symbols {
		got[sym.QualifiedName] = sym.ChangeType
	}
	if got["example.com/rm.gone"] != changeTypeRemoved {
		t.Errorf("gone = %q, want removed (all: %v)", got["example.com/rm.gone"], got)
	}
	if ct, ok := got["example.com/rm.keep"]; ok {
		t.Errorf("keep was not touched but reported as %q", ct)
	}
}

// TestChangedSymbolsIncludesUntrackedFile pins that a brand-new file counts.
// `git diff <ref>` never lists an untracked path, so an added file was
// invisible while its symbols were already indexed.
func TestChangedSymbolsIncludesUntrackedFile(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/newfile\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nfunc alpha() {}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "b.go"), "package main\n\nfunc brandNew() {}\n")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	got := map[string]string{}
	for _, sym := range result.Symbols {
		got[sym.QualifiedName] = sym.ChangeType
	}
	if got["example.com/newfile.brandNew"] != changeTypeAdded {
		t.Errorf("brandNew = %q, want added (all: %v)", got["example.com/newfile.brandNew"], got)
	}
}

// TestChangedSymbolsRenameWithNameInBody pins that rename detection masks the
// declared identifier by AST position: a body string literal containing the old
// name must not defeat the shape match, which text substitution did.
func TestChangedSymbolsRenameWithNameInBody(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/inbody\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nimport \"fmt\"\n\nfunc RoundFloat(v float64) float64 {\n\tfmt.Println(\"RoundFloat\")\n\treturn v\n}\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nimport \"fmt\"\n\nfunc RoundFloat2(v float64) float64 {\n\tfmt.Println(\"RoundFloat\")\n\treturn v\n}\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if len(result.Symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %d (%+v)", len(result.Symbols), result.Symbols)
	}
	sym := result.Symbols[0]
	if sym.ChangeType != changeTypeRenamed {
		t.Errorf("change_type = %q, want renamed", sym.ChangeType)
	}
	if sym.RenamedFrom != "example.com/inbody.RoundFloat" {
		t.Errorf("renamed_from = %q, want example.com/inbody.RoundFloat", sym.RenamedFrom)
	}
}

// TestChangedSymbolsGroupedConstRename pins per-spec comparison inside a
// grouped const block: both entries renamed with values unchanged is two
// renames, not two additions plus two removals.
func TestChangedSymbolsGroupedConstRename(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/grouped\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nconst (\n\tAlpha = \"alpha\"\n\tBeta  = \"beta\"\n)\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nconst (\n\tAlpha2 = \"alpha\"\n\tBeta2  = \"beta\"\n)\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if result.Summary.Renamed != 2 {
		t.Fatalf("summary.renamed = %d, want 2 (symbols: %+v)", result.Summary.Renamed, result.Symbols)
	}
	got := map[string]string{}
	from := map[string]string{}
	for _, sym := range result.Symbols {
		got[sym.QualifiedName] = sym.ChangeType
		from[sym.QualifiedName] = sym.RenamedFrom
	}
	if got["example.com/grouped.Alpha2"] != changeTypeRenamed || got["example.com/grouped.Beta2"] != changeTypeRenamed {
		t.Errorf("expected both entries renamed, got %v", got)
	}
	if from["example.com/grouped.Alpha2"] != "example.com/grouped.Alpha" || from["example.com/grouped.Beta2"] != "example.com/grouped.Beta" {
		t.Errorf("unexpected renamed_from values: %v", from)
	}
	if got["example.com/grouped.Alpha2"] == changeTypeAdded || got["example.com/grouped.Alpha2"] == changeTypeRemoved {
		t.Errorf("Alpha2 misclassified as %q", got["example.com/grouped.Alpha2"])
	}
}

// TestChangedSymbolsGroupedSiblingUntouched pins that renaming one entry of a
// grouped const block does not report its sibling as modified.
func TestChangedSymbolsGroupedSiblingUntouched(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/sibling\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nconst (\n\tAlpha = \"alpha\"\n\tBeta  = \"beta\"\n)\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nconst (\n\tAlpha = \"alpha\"\n\tBeta2  = \"beta\"\n)\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	got := map[string]string{}
	for _, sym := range result.Symbols {
		got[sym.QualifiedName] = sym.ChangeType
	}
	if got["example.com/sibling.Beta2"] != changeTypeRenamed {
		t.Errorf("Beta2 = %q, want renamed (all: %v)", got["example.com/sibling.Beta2"], got)
	}
	if ct, ok := got["example.com/sibling.Alpha"]; ok && ct != "" {
		t.Errorf("Alpha was untouched but reported as %q", ct)
	}
}

// TestChangedSymbolsValueChangeIsNotRename pins that a rename plus a value edit
// is not a rename: the masked shapes differ, so it reports the old name as
// removed and the new one as added.
func TestChangedSymbolsValueChangeIsNotRename(t *testing.T) {
	repo := setupGitRepo(t)
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/valchange\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nconst Alpha = \"alpha\"\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "a.go"), "package main\n\nconst Beta = \"beta\"\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	got := map[string]string{}
	for _, sym := range result.Symbols {
		got[sym.QualifiedName] = sym.ChangeType
	}
	if result.Summary.Renamed != 0 {
		t.Errorf("summary.renamed = %d, want 0", result.Summary.Renamed)
	}
	if got["example.com/valchange.Beta"] != changeTypeAdded {
		t.Errorf("Beta = %q, want added (all: %v)", got["example.com/valchange.Beta"], got)
	}
	if got["example.com/valchange.Alpha"] != changeTypeRemoved {
		t.Errorf("Alpha = %q, want removed (all: %v)", got["example.com/valchange.Alpha"], got)
	}
}

// TestChangedSymbolsAttachesCompileErrors pins that the delta carries the
// index's compile errors unfiltered: a rename that leaves a caller dangling in
// a package the change did not touch is named, not merely counted.
func TestChangedSymbolsAttachesCompileErrors(t *testing.T) {
	repo := setupGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "pkgA"), 0755); err != nil {
		t.Fatalf("mkdir pkgA: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "pkgB"), 0755); err != nil {
		t.Fatalf("mkdir pkgB: %v", err)
	}
	writeFile0644(t, filepath.Join(repo, "go.mod"), "module example.com/dangling\n\ngo 1.26.2\n")
	writeFile0644(t, filepath.Join(repo, "pkgA", "a.go"), "package pkgA\n\nfunc Hello() string { return \"hi\" }\n")
	writeFile0644(t, filepath.Join(repo, "pkgB", "b.go"), "package pkgB\n\nimport \"example.com/dangling/pkgA\"\n\nfunc Use() string { return pkgA.Hello() }\n")
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	writeFile0644(t, filepath.Join(repo, "pkgA", "a.go"), "package pkgA\n\nfunc Goodbye() string { return \"hi\" }\n")
	runGitIn(t, repo, "add", ".")

	s := indexRepo(t, repo)
	result, err := ChangedSymbols(s, repo, "HEAD", false, false, false)
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	if len(result.CompileErrors) != 1 {
		t.Fatalf("expected 1 compile error, got %d (%+v)", len(result.CompileErrors), result.CompileErrors)
	}
	e := result.CompileErrors[0]
	if !strings.HasSuffix(filepath.ToSlash(e.File), "pkgB/b.go") {
		t.Errorf("compile error file = %q, want the stale call site in pkgB/b.go", e.File)
	}
	if e.Line != 5 {
		t.Errorf("compile error line = %d, want 5 (the pkgA.Hello call)", e.Line)
	}
}
