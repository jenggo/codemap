package query

import (
	"path/filepath"
	"reflect"
	"testing"

	"codemap/parse"
	"codemap/pattern"
	"codemap/resolve"
	"codemap/store"
)

func newPatternFixtureStore(t *testing.T) *store.Store {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "testdata", "fixtures", "pattern"))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(abs)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), parse.FileContents(pr), nil); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return s
}

func TestPatternSearchReportsEnclosingSymbol(t *testing.T) {
	s := newPatternFixtureStore(t)
	for _, tc := range []struct {
		pattern string
		symbol  string
		text    string
	}{
		{"defer $CALL", "example.com/pattern.Handle", `defer fmt.Println("done")`},
		{"if err != nil { return $ERR }", "example.com/pattern.Handle", ""},
		{"$A() && $A()", "example.com/pattern.same", "check() && check()"},
	} {
		matches, err := PatternSearch(s, tc.pattern, "", "")
		if err != nil {
			t.Fatalf("PatternSearch(%q): %v", tc.pattern, err)
		}
		if len(matches) != 1 {
			t.Fatalf("PatternSearch(%q) = %d matches, want 1: %+v", tc.pattern, len(matches), matches)
		}
		if matches[0].Symbol != tc.symbol {
			t.Errorf("PatternSearch(%q) symbol = %q, want %q", tc.pattern, matches[0].Symbol, tc.symbol)
		}
		if matches[0].File == "" || matches[0].Line == 0 {
			t.Errorf("PatternSearch(%q) must report file and line, got %+v", tc.pattern, matches[0])
		}
		if tc.text != "" && matches[0].Text != tc.text {
			t.Errorf("PatternSearch(%q) text = %q, want %q", tc.pattern, matches[0].Text, tc.text)
		}
	}
}

func TestPatternSearchOutsideSymbol(t *testing.T) {
	s := newPatternFixtureStore(t)
	matches, err := PatternSearch(s, `import "fmt"`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1: %+v", len(matches), matches)
	}
	if matches[0].Symbol != "" {
		t.Fatalf("a match outside any symbol must not claim one, got %q", matches[0].Symbol)
	}
	if matches[0].File == "" || matches[0].Line == 0 {
		t.Fatal("a match outside any symbol must still report file and line")
	}
}

func TestPatternSearchInvalidPattern(t *testing.T) {
	s := newPatternFixtureStore(t)
	if _, err := PatternSearch(s, "if err != nil {", "", ""); err == nil {
		t.Fatal("an unparseable pattern must return an error, not zero matches")
	}
}

func TestPatternSearchScope(t *testing.T) {
	s := newPatternFixtureStore(t)

	if matches, err := PatternSearch(s, "defer $CALL", "main.go", ""); err != nil || len(matches) != 1 {
		t.Fatalf("file-scoped search = %d matches, err=%v", len(matches), err)
	}

	for _, tc := range []struct{ file, repo string }{
		{"nope.go", ""},
		{"", "example.com/other"},
	} {
		matches, err := PatternSearch(s, "defer $CALL", tc.file, tc.repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("scope file=%q repo=%q must report no matches, got %+v", tc.file, tc.repo, matches)
		}
	}
}

func TestPatternSearchIsReadOnly(t *testing.T) {
	s := newPatternFixtureStore(t)
	before := indexedContentMap(t, s)
	if _, err := PatternSearch(s, "if err != nil { return $ERR }", "", ""); err != nil {
		t.Fatal(err)
	}
	if after := indexedContentMap(t, s); !reflect.DeepEqual(before, after) {
		t.Fatal("pattern search modified indexed file content")
	}
}

func indexedContentMap(t *testing.T, s *store.Store) map[string]string {
	t.Helper()
	files, err := s.IndexedFiles("", "")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

// TestPrefilterDoesNotChangeMatches runs the same search with and without the
// literal pre-filter and requires an identical result set.
func TestPrefilterDoesNotChangeMatches(t *testing.T) {
	s := newPatternFixtureStore(t)
	files, err := s.IndexedFiles("", "")
	if err != nil {
		t.Fatal(err)
	}
	symbols, err := symbolsByFile(s)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := pattern.Compile("if err != nil { return $ERR }")
	if err != nil {
		t.Fatal(err)
	}

	full := matchIndexedFiles(files, compiled, symbols)
	filtered := matchIndexedFiles(prefilter(files, compiled.Literals()), compiled, symbols)
	if !reflect.DeepEqual(full, filtered) {
		t.Fatalf("pre-filter changed the result set:\nfull=%+v\nfiltered=%+v", full, filtered)
	}
	if len(full) == 0 {
		t.Fatal("expected the fixture to produce at least one match")
	}
}
