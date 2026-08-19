package workspace_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
	"codemap/workspace"
)

func runParse(dir string) (*parse.Result, error) {
	return parse.Run(dir)
}

func parseFileContents(pr *parse.Result) map[string]string {
	return parse.FileContents(pr)
}

func resolveResult(pr *parse.Result) *resolve.Result {
	return resolve.Run(pr)
}

// fixture builds a temporary sibling workspace with:
//   - module "a" with a types package, a sub-package, and a NESTED module
//     "a/nested" (nested go.mod) that must be attributed to itself
//   - module "b" importing "a" and importing "unlisted" (not in config) and
//     "gone/x" (a configured member whose directory is missing)
//   - module "c" standalone
//   - module "gone" whose directory does not exist (missing member)
//
// Modules a, b, c get git repos so changed-symbols tests can run.
func fixture(t *testing.T) (root, dbPath string) {
	t.Helper()
	root = t.TempDir()

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("a/go.mod", "module example.com/a\n\ngo 1.21\n")
	write("a/types.go", `package a

type SystemInfo struct {
	Name string
}

func NewInfo(name string) *SystemInfo { return &SystemInfo{Name: name} }
`)
	write("a/sub/sub.go", "package sub\n\nfunc Helper() string { return \"help\" }\n")
	write("a/nested/go.mod", "module example.com/nested\n\ngo 1.21\n")
	write("a/nested/x/x.go", "package x\n\nfunc Nested() int { return 42 }\n")

	write("b/go.mod", "module example.com/b\n\ngo 1.21\n")
	write("b/user.go", `package b

import (
	"example.com/a"
	"example.com/gone/x"
	"example.com/unlisted"
)

func Describe(i *a.SystemInfo) string { return i.Name }

var _ = x.Foo
var _ = unlisted.Missing
`)
	write("c/go.mod", "module example.com/c\n\ngo 1.21\n")
	write("c/c.go", "package c\n\nfunc C() int { return 1 }\n\ntype SystemInfo struct {\n\tCount int\n}\n")

	cfgPath := filepath.Join(root, "codemap.yaml")
	cfg := `members:
  - module: example.com/a
    dir: a
  - module: example.com/nested
    dir: a/nested
  - module: example.com/b
    dir: b
  - module: example.com/c
    dir: c
  - module: example.com/gone
    dir: gone
`
	write("codemap.yaml", cfg)
	_ = cfgPath

	for _, m := range []string{"a", "b", "c"} {
		gitInit(t, filepath.Join(root, m))
	}

	dbPath = filepath.Join(root, ".codemap", "codemap.db")
	return root, dbPath
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("add", "-A")
	run("commit", "-m", "initial")
}

func indexAll(t *testing.T, root, dbPath string) {
	t.Helper()
	cfg, err := workspace.Load(filepath.Join(root, "codemap.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := workspace.IndexAll(cfg, dbPath); err != nil {
		t.Fatalf("index workspace: %v", err)
	}
}

func openStore(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestConfigDedupeByModulePath covers edge #2/#7: the same module listed twice
// (once via a symlinked dir) resolves to a single canonical identity.
func TestConfigDedupeByModulePath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "r/go.mod"), "module example.com/d\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "r/x.go"), "package d\n\nfunc D() int { return 1 }\n")
	symlink := filepath.Join(root, "r-alias")
	if err := os.Symlink(filepath.Join(root, "r"), symlink); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	writeFile(t, filepath.Join(root, "codemap.yaml"), `members:
  - module: example.com/d
    dir: r
  - module: example.com/d
    dir: r-alias
`)
	cfg, err := workspace.Load(filepath.Join(root, "codemap.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Members) != 1 {
		t.Fatalf("expected 1 deduped member, got %d", len(cfg.Members))
	}
}

// TestConfigValidationInvalidMember covers edge #8: a configured dir that is
// not a Go module fails naming the offending entry.
func TestConfigValidationInvalidMember(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok/go.mod"), "module example.com/ok\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "notgo/file.txt"), "x")
	writeFile(t, filepath.Join(root, "codemap.yaml"), `members:
  - module: example.com/ok
    dir: ok
  - module: example.com/bad
    dir: notgo
`)
	_, err := workspace.Load(filepath.Join(root, "codemap.yaml"))
	if err == nil {
		t.Fatal("expected validation error for non-Go member")
	}
	if !strings.Contains(err.Error(), "example.com/bad") {
		t.Fatalf("error should name the offending entry, got: %v", err)
	}
}

// TestConfigMissingMemberNotAnError covers edge #4: a member whose directory is
// missing is retained as a missing member rather than failing config load.
func TestConfigMissingMemberNotAnError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "ok/go.mod"), "module example.com/ok\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "codemap.yaml"), `members:
  - module: example.com/ok
    dir: ok
  - module: example.com/gone
    dir: gone
`)
	cfg, err := workspace.Load(filepath.Join(root, "codemap.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var gone bool
	for _, m := range cfg.Members {
		if m.Module == "example.com/gone" && m.Missing {
			gone = true
		}
	}
	if !gone {
		t.Fatal("expected example.com/gone to be marked missing")
	}
}

// TestDiscover covers the --discover fallback (workspace-config spec).
func TestDiscover(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "emak/go.mod"), "module github.com/jenggo/emak\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "krucil/go.mod"), "module github.com/jenggo/krucil\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "other/go.mod"), "module unrelated\n\ngo 1.21\n")

	candidates, err := workspace.Discover("github.com/jenggo/emak", filepath.Join(root, "emak"))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Module != "github.com/jenggo/krucil" {
		t.Fatalf("expected only prefix-matching sibling, got %+v", candidates)
	}
}

func TestWorkspaceIndexAttributionAndNamespacing(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)
	s := openStore(t, dbPath)

	// Packages from every member (edge #10: tests attributed to own repo).
	pkgs, err := s.ListPackages()
	if err != nil {
		t.Fatal(err)
	}
	pathSet := map[string]string{}
	for _, p := range pkgs {
		pathSet[p.Path] = p.Repo
	}
	for _, want := range []struct{ pkg, repo string }{
		{"example.com/a", "example.com/a"},
		{"example.com/a/sub", "example.com/a"},
		{"example.com/b", "example.com/b"},
		{"example.com/c", "example.com/c"},
		{"example.com/nested/x", "example.com/nested"},
	} {
		if repo := pathSet[want.pkg]; repo != want.repo {
			t.Errorf("package %s attributed to %q, want %q", want.pkg, repo, want.repo)
		}
	}
	// Nested module packages must NOT leak into the outer module (edge #6).
	if len(pathSet) != 5 {
		t.Errorf("expected 5 packages, got %d (%v)", len(pathSet), pathSet)
	}

	// Namespaced qualified names (edge #1): distinct per repo.
	sym, err := s.SymbolByName("example.com/a.SystemInfo")
	if err != nil {
		t.Fatalf("namespaced symbol lookup: %v", err)
	}
	if sym.Repo != "example.com/a" {
		t.Errorf("symbol repo = %q", sym.Repo)
	}
}

func TestCrossRepoImportEdges(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)
	s := openStore(t, dbPath)

	edges, err := query.ImportersOf(s, "example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].FromRef != "example.com/b" {
		t.Fatalf("expected example.com/b importing example.com/a, got %+v", edges)
	}
	if edges[0].Repo != "example.com/b" {
		t.Errorf("importer repo = %q, want example.com/b", edges[0].Repo)
	}

	// Dangling import of a known-but-missing module (edge #3/#4).
	imports, err := query.ImportsOf(s, "example.com/b")
	if err != nil {
		t.Fatal(err)
	}
	foundGone := false
	foundUnlisted := false
	for _, e := range imports {
		switch e.ToRef {
		case "example.com/gone/x":
			foundGone = true
			if e.State != "not indexed" {
				t.Errorf("gone import state = %q, want 'not indexed'", e.State)
			}
		case "example.com/unlisted":
			foundUnlisted = true
		}
	}
	if !foundGone {
		t.Error("expected dangling edge to example.com/gone/x")
	}
	if !foundUnlisted {
		t.Error("expected edge to unlisted module (reported, not dropped)")
	}

	// Correct absence (cross-repo-imports spec): c imports nothing.
	cImports, err := query.ImportsOf(s, "example.com/c")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cImports {
		if strings.HasPrefix(e.ToRef, "example.com/") {
			t.Errorf("c should have no cross-repo imports, got %+v", e)
		}
	}
}

func TestMissingMemberRepoNotIndexed(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)
	s := openStore(t, dbPath)

	repos, err := s.ListRepos()
	if err != nil {
		t.Fatal(err)
	}
	var goneState string
	for _, r := range repos {
		if r.ModulePath == "example.com/gone" {
			goneState = s.RepoState(r)
		}
	}
	if goneState != "missing" {
		t.Fatalf("gone state = %q, want missing", goneState)
	}
}

func TestRepoFilterAndAmbiguity(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)
	s := openStore(t, dbPath)

	results, err := query.Search(s, "Describe", query.WithRepo("example.com/a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Repo != "example.com/a" {
			t.Errorf("repo filter leaked result from %q", r.Repo)
		}
	}
	if len(results) != 0 {
		t.Errorf("a has no Describe; got %d results", len(results))
	}

	// Ambiguity handling (edge #1, repo-scoped-queries spec): the same short
	// name in two repos returns both, each tagged with its repo.
	all, err := query.Search(s, "SystemInfo")
	if err != nil {
		t.Fatal(err)
	}
	repos := map[string]bool{}
	for _, r := range all {
		repos[r.Repo] = true
	}
	if !repos["example.com/a"] || !repos["example.com/c"] {
		t.Errorf("short-name search should return SystemInfo from a and c, got %v", repos)
	}
}

func TestFindPathCrossRepo(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)
	s := openStore(t, dbPath)

	path, err := query.FindPath(s, "example.com/b.Describe", "example.com/a.SystemInfo", 5)
	if err != nil {
		t.Fatalf("find_path: %v", err)
	}
	if len(path) == 0 {
		t.Fatal("expected a path")
	}
}

// TestBackwardCompatSingleRepoOpen covers edge #9: a single-repo database
// (built with the old write path) opens and queries unchanged with repo_id NULL.
func TestBackwardCompatSingleRepoOpen(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/standalone\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "s.go"), "package standalone\n\nfunc Stand() int { return 1 }\n")
	gitInit(t, root)

	dbPath := filepath.Join(root, ".codemap", "codemap.db")
	cfg := &workspace.Config{Root: root}
	_ = cfg
	// Use the single-repo CLI-equivalent path: parse + resolve + Write.
	parseRes, err := runParse(root)
	if err != nil {
		t.Fatal(err)
	}
	resolveRes := resolveResult(parseRes)
	st, err := store.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(resolveRes, parseFileContents(parseRes), nil); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	s := openStore(t, dbPath)
	sym, err := s.SymbolByName("example.com/standalone.Stand")
	if err != nil {
		t.Fatalf("single-repo symbol lookup after upgrade: %v", err)
	}
	if sym.Repo != "" {
		t.Errorf("single-repo symbol repo should be empty, got %q", sym.Repo)
	}
	repos, _ := s.ListRepos()
	if len(repos) != 0 {
		t.Errorf("single-repo DB should register no workspace repos, got %d", len(repos))
	}
}

// TestStalenessPerRepo covers edges #5/#13/#14 and the workspace-index spec:
// editing one repo marks only that repo stale and auto-reindex updates only it.
func TestStalenessPerRepo(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)
	s := openStore(t, dbPath)

	// All repos freshly indexed -> not stale.
	repos, _ := s.ListRepos()
	for _, r := range repos {
		if r.ModulePath == "example.com/gone" {
			continue
		}
		stale, err := s.IsRepoStale(r.ModulePath)
		if err != nil {
			t.Fatal(err)
		}
		if stale {
			t.Errorf("repo %s should not be stale right after index", r.ModulePath)
		}
	}

	// Rewind only a's index time, then touch a file in a -> only a stale.
	old := time.Now().Add(-24 * time.Hour)
	if err := s.SetRepoIndexedAt("example.com/a", old); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "a/sub/sub.go"), "package sub\n\nfunc Helper() string { return \"changed\" }\n")
	now := time.Now()
	_ = os.Chtimes(filepath.Join(root, "a/sub/sub.go"), now, now)

	staleA, err := s.IsRepoStale("example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	staleB, err := s.IsRepoStale("example.com/b")
	if err != nil {
		t.Fatal(err)
	}
	if !staleA {
		t.Error("a should be stale after its file changed")
	}
	if staleB {
		t.Error("b must not be stale — invalidation is scoped per repo")
	}

	// workspace.ReindexStale reindexes only a and returns ["example.com/a"].
	reindexed, err := workspace.ReindexStale(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reindexed) != 1 || reindexed[0] != "example.com/a" {
		t.Fatalf("expected only example.com/a reindexed, got %v", reindexed)
	}
	// After reindex, a is fresh again and its new body is queryable.
	staleA, _ = s.IsRepoStale("example.com/a")
	if staleA {
		t.Error("a should be fresh after reindex")
	}
	body, err := s.FileContent(filepath.Join(root, "a/sub/sub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "changed") {
		t.Errorf("reindexed file content not updated: %q", body)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceChangedSymbolsAcrossRepos(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)

	// Modify b's working tree (uncommitted) so the diff against its ref shows
	// changes; a and c stay untouched. The edit preserves line structure so the
	// indexed symbol's line range still overlaps the diff hunk.
	writeFile(t, filepath.Join(root, "b/user.go"), `package b

import (
	"example.com/a"
	"example.com/gone/x"
	"example.com/unlisted"
)

func Describe(i *a.SystemInfo) string { return "v2" + i.Name }

var _ = x.Foo
var _ = unlisted.Missing
`)

	s := openStore(t, dbPath)
	result, err := query.WorkspaceChangedSymbols(s, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Symbols) == 0 {
		t.Fatal("expected changed symbols from workspace")
	}
	for _, cs := range result.Symbols {
		if cs.Repo != "example.com/b" {
			t.Errorf("changed symbol tagged repo %q, want example.com/b", cs.Repo)
		}
	}
}

func TestAutoReindexBeforeQuery(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)

	// Make c stale.
	s := openStore(t, dbPath)
	old := time.Now().Add(-24 * time.Hour)
	_ = s.SetRepoIndexedAt("example.com/c", old)
	writeFile(t, filepath.Join(root, "c/c.go"), "package c\n\nfunc C() int { return 42 }\n")
	now := time.Now()
	_ = os.Chtimes(filepath.Join(root, "c/c.go"), now, now)
	_ = s.Close()

	// workspace.ReindexStale triggers before answering; c reindexed.
	reindexed, err := workspace.ReindexStale(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reindexed) != 1 || reindexed[0] != "example.com/c" {
		t.Fatalf("expected example.com/c reindexed, got %v", reindexed)
	}

	s2 := openStore(t, dbPath)
	repos, _ := s2.ListRepos()
	for _, r := range repos {
		if r.ModulePath == "example.com/c" && s2.RepoState(r) != "indexed" {
			t.Errorf("c state = %q, want indexed", s2.RepoState(r))
		}
	}
}

func TestOverviewReportsRepos(t *testing.T) {
	root, dbPath := fixture(t)
	indexAll(t, root, dbPath)
	s := openStore(t, dbPath)

	ov, err := query.Overview(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Repos) != 5 {
		t.Fatalf("overview should report 5 repos, got %d", len(ov.Repos))
	}
	states := map[string]string{}
	for _, r := range ov.Repos {
		states[r.ModulePath] = r.State
	}
	if states["example.com/gone"] != "missing" {
		t.Errorf("gone state in overview = %q, want missing", states["example.com/gone"])
	}
	if states["example.com/a"] != "indexed" {
		t.Errorf("a state in overview = %q, want indexed", states["example.com/a"])
	}
}

// TestWorkspaceBrokenMemberDoesNotBlockOthers covers task 6.4: a three-repo
// workspace where one member has broken Go source (unresolvable import) still
// indexes the two good members. The broken member gets zero symbols/edges
// because its files fail to parse, but IndexAll does not error.
func TestWorkspaceBrokenMemberDoesNotBlockOthers(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "good1/go.mod"), "module example.com/good1\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "good1/valid.go"), "package good1\n\nfunc Helper() int { return 1 }\n")

	writeFile(t, filepath.Join(root, "good2/go.mod"), "module example.com/good2\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "good2/user.go"), `package good2

import "example.com/good1"

func Use() int { return good1.Helper() }
`)

	// Broken: valid go.mod but source file has a syntax error (unclosed
	// brace). go list -e reports the file in GoFiles but parser.ParseFile
	// fails, so no AST is produced and the module gets 0 parsed files.
	writeFile(t, filepath.Join(root, "broken/go.mod"), "module example.com/broken\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "broken/broken.go"), "package broken\n\nfunc Bad() int {\n")

	writeFile(t, filepath.Join(root, "codemap.yaml"), `members:
  - module: example.com/good1
    dir: good1
  - module: example.com/good2
    dir: good2
  - module: example.com/broken
    dir: broken
`)

	cfg, err := workspace.Load(filepath.Join(root, "codemap.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	dbPath := filepath.Join(root, ".codemap", "codemap.db")
	summaries, err := workspace.IndexAll(cfg, dbPath)
	if err != nil {
		t.Fatalf("index workspace should not fail: %v", err)
	}

	if len(summaries) != 3 {
		t.Fatalf("expected 3 summaries, got %d", len(summaries))
	}

	byRepo := make(map[string]workspace.IndexSummary)
	for _, s := range summaries {
		byRepo[s.Repo] = s
	}

	// Good members are indexed with symbols.
	for _, want := range []string{"example.com/good1", "example.com/good2"} {
		s := byRepo[want]
		if s.State != "indexed" {
			t.Errorf("%s state = %q, want indexed", want, s.State)
		}
		if s.Symbols == 0 {
			t.Errorf("%s should have symbols after indexing", want)
		}
	}

	// Broken member has zero symbols — its files failed to resolve but it
	// did not block the other members.
	broken := byRepo["example.com/broken"]
	if broken.Symbols != 0 || broken.Edges != 0 {
		t.Errorf("broken member should have 0 symbols/edges, got symbols=%d edges=%d",
			broken.Symbols, broken.Edges)
	}

	// The index records how it was built (indexed_via meta). With a working
	// go toolchain the workspace index is go-list, not the dir-walk fallback.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	health, err := st.Health()
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.IndexedVia != "go-list" {
		t.Errorf("indexed_via = %q, want go-list", health.IndexedVia)
	}
}

var _ = fmt.Sprintf
