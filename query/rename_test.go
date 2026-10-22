package query_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

// renameFixture copies the rename fixture into a temp dir and indexes it, so a
// test renames a throwaway tree. The fixture spans three packages, so a rename
// exercises cross-package sites.
func renameFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	src, err := filepath.Abs("../testdata/fixtures/rename")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "rename")
	if err := copyTree(src, dir); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	parseResult, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolveResult := resolve.Run(parseResult)
	st, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(resolveResult, parse.FileContents(parseResult), nil); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return st, dir
}

func copyTree(src, dst string) error {
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

func applyRename(t *testing.T, st *store.Store, qn, newName string, force bool) *query.RenameResult {
	t.Helper()
	plan, err := query.RenameSymbol(st, qn, newName, force)
	if err != nil {
		t.Fatalf("rename %s -> %s: %v", qn, newName, err)
	}
	result, err := query.ApplyRename(plan)
	if err != nil {
		t.Fatalf("apply rename %s -> %s: %v", qn, newName, err)
	}
	return result
}

// TestRenameFunctionAcrossPackages renames a function referenced from three
// packages and asserts the declaration and every site changed, with counts that
// match what was actually rewritten.
func TestRenameFunctionAcrossPackages(t *testing.T) {
	st, dir := renameFixture(t)
	result := applyRename(t, st, "renamefixture/alpha.Target", "NewTarget", false)

	if len(result.Files) != 3 {
		t.Fatalf("Target is used in three files, got %d: %+v", len(result.Files), result.Files)
	}
	// Declaration + SameLine twice + CallTarget + TwoOnOneLine twice + CallTarget.
	if result.SiteCount != 7 {
		t.Errorf("expected 7 sites, got %d: %+v", result.SiteCount, result.Files)
	}

	for _, path := range []string{"alpha/alpha.go", "beta/beta.go", "gamma/gamma.go"} {
		src := readFile(t, filepath.Join(dir, path))
		code := stripCommentLines(src)
		if regexp.MustCompile(`\bTarget\(`).MatchString(code) {
			t.Errorf("%s still calls Target:\n%s", path, src)
		}
		if !strings.Contains(code, "NewTarget(") {
			t.Errorf("%s has no NewTarget call:\n%s", path, src)
		}
	}
}

// TestRenameMethodLeavesReceiverUntouched renames a method and asserts the
// receiver type's name is unchanged.
func TestRenameMethodLeavesReceiverUntouched(t *testing.T) {
	st, dir := renameFixture(t)
	applyRename(t, st, "renamefixture/alpha.Widget.Describe", "Show", false)

	src := readFile(t, filepath.Join(dir, "alpha/alpha.go"))
	if !strings.Contains(src, "func (w Widget) Show() string") {
		t.Errorf("method was not renamed on its receiver:\n%s", src)
	}
	if strings.Contains(stripCommentLines(src), "Describe") {
		t.Errorf("old method name survived:\n%s", src)
	}
	if !strings.Contains(src, "type Widget struct") {
		t.Errorf("receiver type name was changed:\n%s", src)
	}
}

// TestRenameTypeInSignaturesAndLiterals renames a type used in a signature and
// a composite literal, in two packages.
func TestRenameTypeInSignaturesAndLiterals(t *testing.T) {
	st, dir := renameFixture(t)
	result := applyRename(t, st, "renamefixture/alpha.Widget", "Gadget", false)

	if len(result.Files) != 2 {
		t.Fatalf("Widget is used in two files, got %+v", result.Files)
	}
	alpha := readFile(t, filepath.Join(dir, "alpha/alpha.go"))
	if !strings.Contains(alpha, "type Gadget struct") {
		t.Errorf("type declaration was not renamed:\n%s", alpha)
	}
	if !strings.Contains(alpha, "func BuildWidget(name string) Gadget") || !strings.Contains(alpha, "return Gadget{Name: name}") {
		t.Errorf("signature or composite literal was not renamed:\n%s", alpha)
	}
	beta := readFile(t, filepath.Join(dir, "beta/beta.go"))
	if !strings.Contains(beta, "func UseWidget() alpha.Gadget") || !strings.Contains(beta, "alpha.Gadget{Name:") {
		t.Errorf("cross-package uses were not renamed:\n%s", beta)
	}
}

// TestRenameRewritesEveryOccurrenceOnOneLine asserts a line calling the symbol
// twice is rewritten twice, which only works when splices are applied
// right-to-left.
func TestRenameRewritesEveryOccurrenceOnOneLine(t *testing.T) {
	st, dir := renameFixture(t)
	applyRename(t, st, "renamefixture/alpha.Target", "NewTarget", false)

	beta := readFile(t, filepath.Join(dir, "beta/beta.go"))
	if !strings.Contains(beta, "return alpha.NewTarget() + alpha.NewTarget()") {
		t.Errorf("both occurrences on the line must be renamed:\n%s", beta)
	}
	alpha := readFile(t, filepath.Join(dir, "alpha/alpha.go"))
	if !strings.Contains(alpha, "return NewTarget() + NewTarget()") {
		t.Errorf("both occurrences on the one-line call must be renamed:\n%s", alpha)
	}
}

// TestRenameRefusals covers every refusal path that must leave the tree
// untouched.
func TestRenameRefusals(t *testing.T) {
	tests := []struct {
		name    string
		qn      string
		newName string
		wantIn  string
	}{
		{
			name:    "same-package collision",
			qn:      "renamefixture/alpha.Target",
			newName: "Greeting",
			wantIn:  "renamefixture/alpha.Greeting",
		},
		{
			name:    "method collision on the same receiver",
			qn:      "renamefixture/alpha.Widget.Describe",
			newName: "Label",
			wantIn:  "renamefixture/alpha.Widget.Label",
		},
		{
			name:    "invalid identifier",
			qn:      "renamefixture/alpha.Target",
			newName: "1bad",
			wantIn:  "not a valid Go identifier",
		},
		{
			name:    "keyword",
			qn:      "renamefixture/alpha.Target",
			newName: "func",
			wantIn:  "not a valid Go identifier",
		},
		{
			name:    "cross-package unexported",
			qn:      "renamefixture/alpha.internal",
			newName: "renamed",
			wantIn:  "unexported",
		},
		{
			name:    "no-op rename",
			qn:      "renamefixture/alpha.Target",
			newName: "Target",
			wantIn:  "nothing changed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, dir := renameFixture(t)
			before := snapshotTree(t, dir)

			_, err := query.RenameSymbol(st, tt.qn, tt.newName, false)
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("refusal must mention %q, got %q", tt.wantIn, err.Error())
			}
			assertTreeUnchanged(t, dir, before)
		})
	}
}

// TestRenameRefusesMissingSiteFile asserts a site whose file disappeared after
// indexing refuses the rename and leaves the remaining files untouched.
func TestRenameRefusesMissingSiteFile(t *testing.T) {
	st, dir := renameFixture(t)
	if err := os.Remove(filepath.Join(dir, "gamma/gamma.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	before := snapshotTree(t, dir)

	_, err := query.RenameSymbol(st, "renamefixture/alpha.Target", "NewTarget", false)
	if err == nil {
		t.Fatal("expected a refusal when a site file is unreadable")
	}
	if !strings.Contains(err.Error(), "cannot read site file") {
		t.Errorf("refusal must name the unreadable site, got %q", err.Error())
	}
	assertTreeUnchanged(t, dir, before)
}

// TestRenameShadowingRefusalAndForce asserts a site a local could shadow is
// refused by default, listed, and rewritten under force.
func TestRenameShadowingRefusalAndForce(t *testing.T) {
	st, dir := renameFixture(t)

	_, err := query.RenameSymbol(st, "renamefixture/alpha.ShadowTarget", "Hidden", false)
	if err == nil {
		t.Fatal("expected a shadowing refusal")
	}
	if !strings.Contains(err.Error(), "alpha/alpha.go") || !strings.Contains(err.Error(), "force") {
		t.Errorf("refusal must list the shadowed site and name force, got %q", err.Error())
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "alpha/alpha.go")), "Hidden") {
		t.Error("a refused rename wrote to the file")
	}

	result := applyRename(t, st, "renamefixture/alpha.ShadowTarget", "Hidden", true)
	if len(result.OverriddenShadowing) == 0 {
		t.Error("force must report the shadowing it overrode")
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "alpha/alpha.go")), "return Hidden()") {
		t.Error("forced rename did not rewrite the shadowed site")
	}
}

// snapshotTree records every file's content so a refusal can be shown to have
// written nothing.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return files
}

func assertTreeUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := snapshotTree(t, dir)
	for path, want := range before {
		if got := after[path]; got != want {
			t.Errorf("refused rename modified %s:\n%s", path, got)
		}
	}
}

// stripCommentLines drops comment-only lines, so an assertion about code is not
// confused by a doc comment that still names the old symbol.
func stripCommentLines(src string) string {
	var out []string
	for line := range strings.SplitSeq(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestRenameAllOrNothingUnparseableSite asserts a site whose file no longer
// parses refuses the whole rename and leaves every other file untouched.
func TestRenameAllOrNothingUnparseableSite(t *testing.T) {
	st, dir := renameFixture(t)
	broken := filepath.Join(dir, "gamma/gamma.go")
	if err := os.WriteFile(broken, []byte("package gamma\n\nfunc CallTarget() int { return alpha.Target( }\n"), 0o644); err != nil {
		t.Fatalf("break file: %v", err)
	}
	before := snapshotTree(t, dir)

	_, err := query.RenameSymbol(st, "renamefixture/alpha.Target", "NewTarget", false)
	if err == nil {
		t.Fatal("expected a refusal when a site file does not parse")
	}
	if !strings.Contains(err.Error(), "gamma/gamma.go") {
		t.Errorf("refusal must name the offending file, got %q", err.Error())
	}
	assertTreeUnchanged(t, dir, before)
}

// TestRenameAllOrNothingAfterPlanChange asserts a file edited between planning
// and applying refuses the rename, so no file is written at a stale offset.
func TestRenameAllOrNothingAfterPlanChange(t *testing.T) {
	st, dir := renameFixture(t)
	plan, err := query.RenameSymbol(st, "renamefixture/alpha.Target", "NewTarget", false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	edited := filepath.Join(dir, "gamma/gamma.go")
	if err := os.WriteFile(edited, []byte("package gamma\n\n// edited\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	before := snapshotTree(t, dir)

	if _, err := query.ApplyRename(plan); err == nil {
		t.Fatal("expected a refusal when a planned file changed")
	}
	assertTreeUnchanged(t, dir, before)
}

// TestRenameRefreshesIndex asserts that after a rename and reindex the old name
// no longer resolves, the new name does, and the callers are unchanged.
func TestRenameRefreshesIndex(t *testing.T) {
	st, dir := renameFixture(t)
	oldCallers, err := query.CallersOf(st, "renamefixture/alpha.Target")
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	applyRename(t, st, "renamefixture/alpha.Target", "NewTarget", false)

	parseResult, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if err := st.Write(resolve.Run(parseResult), parse.FileContents(parseResult), nil); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	if _, err := query.Show(st, "renamefixture/alpha.Target"); err == nil {
		t.Error("the old qualified name must not resolve after a rename")
	}
	if _, err := query.Show(st, "renamefixture/alpha.NewTarget"); err != nil {
		t.Errorf("the new qualified name must resolve after a rename: %v", err)
	}

	newCallers, err := query.CallersOf(st, "renamefixture/alpha.NewTarget")
	if err != nil {
		t.Fatalf("callers after rename: %v", err)
	}
	if len(newCallers) != len(oldCallers) {
		t.Fatalf("callers changed across the rename: %d before, %d after", len(oldCallers), len(newCallers))
	}
}
