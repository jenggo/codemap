package query_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

// buildAffectedStore indexes a temporary module and returns the store with its
// module directory, which AffectedTests needs to resolve relative file paths.
func buildAffectedStore(t *testing.T, files map[string]string) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module affected.example\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), parse.FileContents(pr), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s, dir
}

func affectedBySuffix(t *testing.T, files []query.AffectedTest, suffix string) query.AffectedTest {
	t.Helper()
	for _, f := range files {
		if strings.HasSuffix(f.File, suffix) {
			return f
		}
	}
	t.Fatalf("no affected entry ending in %q: %+v", suffix, files)
	return query.AffectedTest{}
}

func hasVia(entry query.AffectedTest, nameSuffix string) bool {
	for _, v := range entry.Via {
		if strings.HasSuffix(v, nameSuffix) {
			return true
		}
	}
	return false
}

func TestAffectedTestsDirectCall(t *testing.T) {
	s, dir := buildAffectedStore(t, map[string]string{
		"calc/calc.go":      "package calc\n\nfunc Add(a, b int) int { return a + b }\n",
		"calc/calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { Add(1, 2) }\n",
	})

	result, err := query.AffectedTests(s, dir, "", []string{"calc/calc.go"}, 3)
	if err != nil {
		t.Fatalf("AffectedTests: %v", err)
	}
	entry := affectedBySuffix(t, result.Files, "calc_test.go")
	if entry.Reason != "callers" {
		t.Fatalf("reason = %q, want callers", entry.Reason)
	}
	if !hasVia(entry, ".Add") {
		t.Fatalf("via = %v, want the changed symbol", entry.Via)
	}
}

func TestAffectedTestsIndirectCall(t *testing.T) {
	s, dir := buildAffectedStore(t, map[string]string{
		"calc/calc.go":      "package calc\n\nfunc Add(a, b int) int { return a + b }\n",
		"prod/prod.go":      "package prod\n\nimport \"affected.example/calc\"\n\nfunc Total() int { return calc.Add(1, 2) }\n",
		"prod/prod_test.go": "package prod\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) { Total() }\n",
	})

	result, err := query.AffectedTests(s, dir, "", []string{"calc/calc.go"}, 3)
	if err != nil {
		t.Fatalf("AffectedTests: %v", err)
	}
	entry := affectedBySuffix(t, result.Files, "prod_test.go")
	if entry.Reason != "callers" {
		t.Fatalf("reason = %q, want callers", entry.Reason)
	}
	if !hasVia(entry, ".Add") {
		t.Fatalf("via = %v, want the changed symbol", entry.Via)
	}
}

func TestAffectedTestsDepthOneExcludesIndirect(t *testing.T) {
	s, dir := buildAffectedStore(t, map[string]string{
		"calc/calc.go":      "package calc\n\nfunc Add(a, b int) int { return a + b }\n",
		"prod/prod.go":      "package prod\n\nimport \"affected.example/calc\"\n\nfunc Total() int { return calc.Add(1, 2) }\n",
		"prod/prod_test.go": "package prod\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) { Total() }\n",
	})

	result, err := query.AffectedTests(s, dir, "", []string{"calc/calc.go"}, 1)
	if err != nil {
		t.Fatalf("AffectedTests: %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("depth 1 reported indirect callers: %+v", result.Files)
	}
}

func TestAffectedTestsConventionOnly(t *testing.T) {
	s, dir := buildAffectedStore(t, map[string]string{
		"solo/solo.go":      "package solo\n\nfunc Solo() {}\n",
		"solo/solo_test.go": "package solo\n\nimport \"testing\"\n\nfunc TestSolo(t *testing.T) {}\n",
	})

	result, err := query.AffectedTests(s, dir, "", []string{"solo/solo.go"}, 3)
	if err != nil {
		t.Fatalf("AffectedTests: %v", err)
	}
	entry := affectedBySuffix(t, result.Files, "solo_test.go")
	if entry.Reason != "convention" {
		t.Fatalf("reason = %q, want convention", entry.Reason)
	}
	if !hasVia(entry, ".Solo") {
		t.Fatalf("via = %v, want the changed file's symbols", entry.Via)
	}
}

func TestAffectedTestsBothStrategiesDeduped(t *testing.T) {
	s, dir := buildAffectedStore(t, map[string]string{
		"calc/calc.go":      "package calc\n\nfunc Add(a, b int) int { return a + b }\n",
		"calc/calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { Add(1, 2); Add(3, 4) }\n",
	})

	result, err := query.AffectedTests(s, dir, "", []string{"calc/calc.go"}, 3)
	if err != nil {
		t.Fatalf("AffectedTests: %v", err)
	}
	entry := affectedBySuffix(t, result.Files, "calc_test.go")
	if entry.Reason != "callers" {
		t.Fatalf("reason = %q, want callers to win over convention", entry.Reason)
	}
	if len(entry.Via) != 1 {
		t.Fatalf("via = %v, want one deduplicated entry", entry.Via)
	}
}

func TestAffectedTestsEmptyResult(t *testing.T) {
	s, dir := buildAffectedStore(t, map[string]string{
		"lonely/lonely.go": "package lonely\n\nfunc Lonely() {}\n",
	})

	result, err := query.AffectedTests(s, dir, "", []string{"lonely/lonely.go"}, 3)
	if err != nil {
		t.Fatalf("AffectedTests: %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("expected no affected tests, got %+v", result.Files)
	}
	if !strings.Contains(result.Note, "no affected tests found") {
		t.Fatalf("note does not state the empty result: %q", result.Note)
	}
}

// TestAffectedTestsViaCapped pins the evidence cap: a test file reached by more
// than five changed symbols reports the first five and says it truncated them,
// rather than growing one entry without bound.
func TestAffectedTestsViaCapped(t *testing.T) {
	s, dir := buildAffectedStore(t, map[string]string{
		"many/many.go":      "package many\n\nfunc A() {}\nfunc B() {}\nfunc C() {}\nfunc D() {}\nfunc E() {}\nfunc F() {}\nfunc G() {}\n",
		"many/many_test.go": "package many\n\nimport \"testing\"\n\nfunc TestAll(t *testing.T) {\n\tA()\n\tB()\n\tC()\n\tD()\n\tE()\n\tF()\n\tG()\n}\n",
	})

	result, err := query.AffectedTests(s, dir, "", []string{"many/many.go"}, 1)
	if err != nil {
		t.Fatalf("AffectedTests: %v", err)
	}
	entry := affectedBySuffix(t, result.Files, "many_test.go")
	if len(entry.Via) != 5 {
		t.Fatalf("via = %v, want the cap of 5", entry.Via)
	}
	if !entry.ViaTruncated {
		t.Fatalf("via_truncated = false, want true for %d reached symbols", 7)
	}
}
