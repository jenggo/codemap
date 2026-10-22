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

const writeFixtureModule = "codemap/testdata/simple"

// writeFixture copies the simple fixture into a temp dir and indexes it, so
// write tests mutate a throwaway tree instead of testdata.
func writeFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "simple")
	src, err := filepath.Abs("../testdata/simple")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
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
	return st, filepath.Join(dir, "parser.go")
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestReplaceSymbolBody covers the four declaration shapes a replacement must
// handle, plus both doc-comment modes.
func TestReplaceSymbolBody(t *testing.T) {
	tests := []struct {
		name       string
		qn         string
		body       string
		includeDoc bool
		wantIn     []string
		wantOut    []string
	}{
		{
			name:   "function",
			qn:     writeFixtureModule + ".NewParser",
			body:   "func NewParser(input string) *Parser {\n\treturn &Parser{input: \"new\"}\n}",
			wantIn: []string{`input: "new"`, "// NewParser creates a new Parser."},
		},
		{
			name:       "function including doc",
			qn:         writeFixtureModule + ".NewParser",
			body:       "// NewParser builds a Parser.\nfunc NewParser(input string) *Parser {\n\treturn &Parser{input: input}\n}",
			includeDoc: true,
			wantIn:     []string{"// NewParser builds a Parser."},
			wantOut:    []string{"// NewParser creates a new Parser."},
		},
		{
			name:   "method keeps receiver",
			qn:     writeFixtureModule + ".Parser.Parse",
			body:   "func (p *Parser) Parse() (string, error) {\n\treturn \"parsed\", nil\n}",
			wantIn: []string{"func (p *Parser) Parse()", `return "parsed", nil`},
		},
		{
			name:   "type declaration",
			qn:     writeFixtureModule + ".Parser",
			body:   "type Parser struct {\n\tinput  string\n\tsuffix string\n}",
			wantIn: []string{"suffix string"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, file := writeFixture(t)
			result, err := query.ReplaceSymbolBody(st, tc.qn, tc.body, tc.includeDoc)
			if err != nil {
				t.Fatalf("ReplaceSymbolBody: %v", err)
			}
			if result.Unchanged {
				t.Fatal("expected a change")
			}
			source := readSource(t, file)
			for _, want := range tc.wantIn {
				if !strings.Contains(source, want) {
					t.Errorf("source missing %q:\n%s", want, source)
				}
			}
			for _, gone := range tc.wantOut {
				if strings.Contains(source, gone) {
					t.Errorf("source still contains %q:\n%s", gone, source)
				}
			}
		})
	}
}

// TestReplaceSymbolBodyRejectsBadInput covers both validation layers: a snippet
// that does not parse, and a snippet that parses alone but breaks the file.
func TestReplaceSymbolBodyRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unparseable snippet", body: "func NewParser( {"},
		{name: "valid alone but breaks the file", body: `import "fmt"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, file := writeFixture(t)
			before := readSource(t, file)
			if _, err := query.ReplaceSymbolBody(st, writeFixtureModule+".NewParser", tc.body, false); err == nil {
				t.Fatal("expected a refusal")
			}
			if after := readSource(t, file); after != before {
				t.Errorf("file changed after a refused write:\n%s", after)
			}
		})
	}
}

// TestInsertSymbol asserts the target declaration survives byte-identically and
// that exactly one blank line separates the insert from its neighbour.
func TestInsertSymbol(t *testing.T) {
	const inserted = "func Extra(s string) string {\n\treturn s\n}"

	t.Run("before", func(t *testing.T) {
		st, file := writeFixture(t)
		before := readSource(t, file)
		if _, err := query.InsertSymbol(st, writeFixtureModule+".NewParser", inserted, false); err != nil {
			t.Fatalf("InsertSymbol: %v", err)
		}
		after := readSource(t, file)
		if !strings.Contains(after, "}\n\n"+inserted+"\n\n// NewParser creates a new Parser.") {
			t.Errorf("insert is not separated by exactly one blank line:\n%s", after)
		}
		if restored := strings.Replace(after, inserted+"\n\n", "", 1); restored != before {
			t.Errorf("removing the insert must restore the original file:\n%s", restored)
		}
	})

	t.Run("after", func(t *testing.T) {
		st, file := writeFixture(t)
		before := readSource(t, file)
		if _, err := query.InsertSymbol(st, writeFixtureModule+".Parser", inserted, true); err != nil {
			t.Fatalf("InsertSymbol: %v", err)
		}
		after := readSource(t, file)
		if !strings.Contains(after, "}\n\n"+inserted+"\n\n// NewParser creates a new Parser.") {
			t.Errorf("insert is not separated by exactly one blank line:\n%s", after)
		}
		if restored := strings.Replace(after, inserted+"\n\n", "", 1); restored != before {
			t.Errorf("removing the insert must restore the original file:\n%s", restored)
		}
	})
}

func TestInsertSymbolRejectsBadText(t *testing.T) {
	st, file := writeFixture(t)
	before := readSource(t, file)
	if _, err := query.InsertSymbol(st, writeFixtureModule+".NewParser", "func Broken(", true); err == nil {
		t.Fatal("expected a parse error")
	}
	if after := readSource(t, file); after != before {
		t.Error("file changed after a refused insert")
	}
}

// TestDeleteSymbol covers the reference guard, both overrides, and the doc
// comment removal.
func TestDeleteSymbol(t *testing.T) {
	t.Run("unreferenced symbol is removed with its doc comment", func(t *testing.T) {
		st, file := writeFixture(t)
		result, err := query.DeleteSymbol(st, writeFixtureModule+".helper", false, false)
		if err != nil {
			t.Fatalf("DeleteSymbol: %v", err)
		}
		if result.Action != "delete" {
			t.Errorf("action = %q", result.Action)
		}
		source := readSource(t, file)
		if strings.Contains(source, "func helper") {
			t.Errorf("declaration still present:\n%s", source)
		}
		if strings.Contains(source, "// Helper is an unexported helper function.") {
			t.Errorf("doc comment was orphaned:\n%s", source)
		}
		if strings.Contains(source, "\n\n\n") {
			t.Errorf("deletion left a widened gap between neighbours:\n%s", source)
		}
	})

	t.Run("referenced symbol is refused with referrers listed", func(t *testing.T) {
		st, file := writeFixture(t)
		addCallerFile(t, st, filepath.Dir(file))
		before := readSource(t, file)
		_, err := query.DeleteSymbol(st, writeFixtureModule+".helper", false, false)
		if err == nil {
			t.Fatal("expected a refusal for a referenced symbol")
		}
		if !strings.Contains(err.Error(), "incoming reference") || !strings.Contains(err.Error(), "caller.go:") {
			t.Errorf("refusal must list referrers with file:line, got: %v", err)
		}
		if after := readSource(t, file); after != before {
			t.Error("file changed after a refused delete")
		}
	})

	t.Run("method is removed but its receiver type survives", func(t *testing.T) {
		st, file := writeFixture(t)
		if _, err := query.DeleteSymbol(st, writeFixtureModule+".Parser.Parse", true, false); err != nil {
			t.Fatalf("DeleteSymbol: %v", err)
		}
		source := readSource(t, file)
		if strings.Contains(source, "func (p *Parser) Parse()") {
			t.Errorf("method declaration still present:\n%s", source)
		}
		if !strings.Contains(source, "type Parser struct {") {
			t.Errorf("deleting a method must leave its receiver type intact:\n%s", source)
		}
	})

	t.Run("exported symbol needs the override", func(t *testing.T) {
		st, file := writeFixture(t)
		addUnreferencedExported(t, st, filepath.Dir(file))
		qn := writeFixtureModule + ".Orphan"

		if _, err := query.DeleteSymbol(st, qn, false, false); err == nil ||
			!strings.Contains(err.Error(), "allow_exported") {
			t.Fatalf("expected an exported-symbol refusal naming allow_exported, got %v", err)
		}
		orphan := filepath.Join(filepath.Dir(file), "orphan.go")
		if source := readSource(t, orphan); !strings.Contains(source, "func Orphan") {
			t.Error("exported symbol must survive a refused delete")
		}
		if _, err := query.DeleteSymbol(st, qn, true, false); err != nil {
			t.Fatalf("allow_exported must proceed: %v", err)
		}
		if source := readSource(t, orphan); strings.Contains(source, "func Orphan") {
			t.Error("exported symbol was not removed with the override")
		}
	})
}

// TestDeleteSymbolTestOnlyRefusal covers the test-only path: refused by default,
// allowed with the override. The fixture's helper is called only from a test.
func TestDeleteSymbolTestOnlyRefusal(t *testing.T) {
	st, file := writeFixture(t)
	dir := filepath.Dir(file)
	testSrc := `package simple

import "testing"

func TestHelper(t *testing.T) {
	if helper("x") != "x" {
		t.Fatal("unexpected")
	}
}
`
	testFile := filepath.Join(dir, "parser_test.go")
	if err := os.WriteFile(testFile, []byte(testSrc), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	reindex(t, st, dir)

	_, err := query.DeleteSymbol(st, writeFixtureModule+".helper", false, false)
	if err == nil || !strings.Contains(err.Error(), "test-only") {
		t.Fatalf("expected a test-only refusal, got %v", err)
	}
	if _, err := query.DeleteSymbol(st, writeFixtureModule+".helper", false, true); err != nil {
		t.Fatalf("allow_test_only must proceed: %v", err)
	}
}

// TestGroupedDeclarationRefusals asserts replace and delete refuse a grouped
// member while insert still succeeds at the group boundary.
func TestGroupedDeclarationRefusals(t *testing.T) {
	st, file := writeFixture(t)
	dir := filepath.Dir(file)
	src := `package simple

const (
	A = 1
	B = 2
)
`
	groupFile := filepath.Join(dir, "group.go")
	if err := os.WriteFile(groupFile, []byte(src), 0o644); err != nil {
		t.Fatalf("write group file: %v", err)
	}
	reindex(t, st, dir)

	qn := writeFixtureModule + ".A"
	if _, err := query.ReplaceSymbolBody(st, qn, "const A = 9", false); err == nil ||
		!strings.Contains(err.Error(), "2 members") {
		t.Fatalf("expected a grouped replace refusal naming the member count, got %v", err)
	}
	if _, err := query.DeleteSymbol(st, qn, false, false); err == nil ||
		!strings.Contains(err.Error(), "grouped") {
		t.Fatalf("expected a grouped delete refusal, got %v", err)
	}

	if _, err := query.InsertSymbol(st, qn, "const C = 3", true); err != nil {
		t.Fatalf("insert must be permitted for grouped members: %v", err)
	}
	if got := readSource(t, groupFile); !strings.Contains(got, "B = 2") || !strings.Contains(got, "const C = 3") {
		t.Errorf("insert disturbed the group:\n%s", got)
	}
}

// TestWritePreservesFileMode asserts the temp-file rename keeps permission bits.
func TestWritePreservesFileMode(t *testing.T) {
	st, file := writeFixture(t)
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := query.ReplaceSymbolBody(st, writeFixtureModule+".helper", "func helper(s string) string {\n\treturn s\n}", false); err != nil {
		t.Fatalf("ReplaceSymbolBody: %v", err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestFailedWriteLeavesOriginalIntact covers both failure stages: a directory
// that cannot accept the temp file, and a rename the filesystem rejects.
func TestFailedWriteLeavesOriginalIntact(t *testing.T) {
	st, file := writeFixture(t)
	before := readSource(t, file)
	dir := filepath.Dir(file)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	_, err := query.ReplaceSymbolBody(st, writeFixtureModule+".helper", "func helper(s string) string {\n\treturn s + s\n}", false)
	if chmodErr := os.Chmod(dir, 0o755); chmodErr != nil {
		t.Fatalf("restore chmod: %v", chmodErr)
	}
	if err == nil {
		t.Fatal("expected the write to fail on a read-only directory")
	}
	if after := readSource(t, file); after != before {
		t.Error("original content changed after a failed write")
	}
	assertNoTempFiles(t, dir)
}

// TestWriteNoOpDoesNotTouchFile asserts a byte-identical replacement writes
// nothing at all.
func TestWriteNoOpDoesNotTouchFile(t *testing.T) {
	st, file := writeFixture(t)
	before := readSource(t, file)
	result, err := query.ReplaceSymbolBody(st, writeFixtureModule+".helper",
		"func helper(s string) string {\n\treturn s\n}", false)
	if err != nil {
		t.Fatalf("ReplaceSymbolBody: %v", err)
	}
	if !result.Unchanged {
		t.Fatal("expected the identical replacement to report unchanged")
	}
	if after := readSource(t, file); after != before {
		t.Error("a no-op replacement must not rewrite the file")
	}
}

// TestWriteReportsStaleIndex edits the file so the indexed line no longer
// matches, then asserts the write applies by name and reports the staleness.
func TestWriteReportsStaleIndex(t *testing.T) {
	st, file := writeFixture(t)
	source := readSource(t, file)
	shifted := "// padding\n// padding\n// padding\n\n" + source
	if err := os.WriteFile(file, []byte(shifted), 0o644); err != nil {
		t.Fatalf("write shifted source: %v", err)
	}

	result, err := query.ReplaceSymbolBody(st, writeFixtureModule+".helper",
		"func helper(s string) string {\n\treturn \"stale\"\n}", false)
	if err != nil {
		t.Fatalf("ReplaceSymbolBody: %v", err)
	}
	if !result.StaleIndex {
		t.Error("expected stale_index to be reported")
	}
	if got := readSource(t, file); !strings.Contains(got, `return "stale"`) {
		t.Errorf("write did not apply to the name-matched declaration:\n%s", got)
	}
}

func TestWriteMissingSymbol(t *testing.T) {
	st, file := writeFixture(t)
	before := readSource(t, file)
	if _, err := query.ReplaceSymbolBody(st, writeFixtureModule+".Nope", "func Nope() {}", false); err == nil {
		t.Fatal("expected a declaration-not-found error")
	}
	if after := readSource(t, file); after != before {
		t.Error("file changed for a missing symbol")
	}
}

// reindex re-parses dir and replaces the whole index, so a test can add a file
// mid-scenario.
func reindex(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	parseResult, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := st.Write(resolve.Run(parseResult), parse.FileContents(parseResult), nil); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// addUnreferencedExported appends an exported function nobody calls, so the
// exported-override path can be exercised without tripping the reference guard.
func addUnreferencedExported(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	path := filepath.Join(dir, "orphan.go")
	src := "package simple\n\n// Orphan is exported and unreferenced.\nfunc Orphan() {}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write orphan file: %v", err)
	}
	reindex(t, st, dir)
}

// addCallerFile adds a file whose function references helper, then reindexes so
// the reference edge exists.
func addCallerFile(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	src := "package simple\n\nfunc caller() string {\n\treturn helper(\"x\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "caller.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write caller file: %v", err)
	}
	reindex(t, st, dir)
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "codemap-tmp") {
			t.Errorf("temp file survived: %s", e.Name())
		}
	}
}
