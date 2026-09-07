package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// seedSearchModule writes a module whose file names, symbols, and contents
// exercise literal handling of LIKE wildcards, FTS operators, and backslashes.
func seedSearchModule(t *testing.T) *Store {
	t.Helper()

	dir := writeModule(t, map[string]string{
		// Underscore pairs: Old_Func vs OldFunc (prefix-search disambiguation).
		"old_func.go": "package lit\n\n// Old_Func does things\nfunc Old_Func() {}\n",
		"oldfunc.go":  "package lit\n\nfunc OldFunc() {}\n",

		// Percent/underscore pairs in file names.
		"save_100%.go": "package lit\n\n// save 100% line\nfunc Save100() {}\n",
		"save_100x.go": "package lit\n\nfunc Save100x() {}\n",

		// Backslash file name plus a decoy that would over-match if the
		// backslash were interpreted as the LIKE escape character.
		"a\\b.go": "package lit\n\nfunc AB() {}\n",
		"ab.go":   "package lit\n\nfunc AB2() {}\n",

		// Underscore type pair for type-signature matching.
		"types.go": "package lit\n\n" +
			"type Foo_Bar struct{}\n" +
			"func TakeFooBar(x Foo_Bar) {}\n" +
			"type FooXBar struct{}\n" +
			"func TakeFooXBar(x FooXBar) {}\n",

		// FTS content for literal operator patterns.
		"svc.go":    "package lit\n\n// handler for user-service\n// -deprecated still here\n// refactor pkg:F helper\nfunc Svc() {}\n",
		"quote.go":  "package lit\n\n// say \"hi\" there\nfunc Quote() {}\n",
		"prefix.go": "package lit\n\n// only alpha\n// alpha beta\n// cli/codemap.Run called\nfunc Prefix() {}\n",

		// camelCase identifier used to exercise trigram substring matching.
		"camel.go": "package lit\n\n// GetSymbolBody resolves a symbol\nfunc GetSymbolBody() {}\n",
	})

	res, files := parseModule(t, dir)
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

func TestEscapeLike(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"a\\b", `a\\b`},
		{"100%", `100\%`},
		{"foo_bar", `foo\_bar`},
		{`a\b%_`, `a\\b\%\_`},
	}
	for _, tc := range cases {
		if got := escapeLike(tc.in); got != tc.want {
			t.Errorf("escapeLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrefixSearchMatchesUnderscoreLiterally(t *testing.T) {
	s := seedSearchModule(t)

	syms, err := s.SearchByQualifiedNamePrefix("atomic.example.Old_", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 {
		t.Fatalf("expected exactly Old_Func to match, got %d: %+v", len(syms), syms)
	}
	if syms[0].Name != "Old_Func" {
		t.Fatalf("expected Old_Func, got %+v", syms[0])
	}
}

func TestFileSearchMatchesPercentLiterally(t *testing.T) {
	s := seedSearchModule(t)

	// Pattern "100%" must match only the file whose name literally contains
	// "100%" and not the file containing "100x".
	syms, err := s.SearchSymbolsByFile("100%", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var save100, save100x bool
	for _, sym := range syms {
		if strings.Contains(sym.PosFile, "save_100%.go") {
			save100 = true
		}
		if strings.Contains(sym.PosFile, "save_100x.go") {
			save100x = true
		}
	}
	if !save100 {
		t.Fatalf("expected save_100%%.go to match, got: %+v", syms)
	}
	if save100x {
		t.Fatalf("save_100x.go over-matched a literal %% search: %+v", syms)
	}
}

func TestFileSearchMatchesBackslashLiterally(t *testing.T) {
	s := seedSearchModule(t)

	// Pattern "a\b" must match the file named `a\b.go` and not `ab.go`.
	syms, err := s.SearchSymbolsByFile(`a\b`, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var wb, plain bool
	for _, sym := range syms {
		if strings.Contains(sym.PosFile, "a\\b.go") {
			wb = true
		}
		if strings.Contains(sym.PosFile, "ab.go") {
			plain = true
		}
	}
	if !wb {
		t.Fatalf("expected a\\b.go to match, got: %+v", syms)
	}
	if plain {
		t.Fatalf("ab.go over-matched a backslash search: %+v", syms)
	}
}

func TestTypeSearchMatchesUnderscoreLiterally(t *testing.T) {
	s := seedSearchModule(t)

	// Pattern "Foo_Bar" must hit Foo_Bar symbols but not the FooXBar decoy
	// (where the wildcard underscore would otherwise stand in for "X").
	syms, err := s.SearchByType("Foo_Bar", false)
	if err != nil {
		t.Fatal(err)
	}
	var target, decoy bool
	for _, sym := range syms {
		if sym.Name == "TakeFooBar" {
			target = true
		}
		if sym.Name == "TakeFooXBar" {
			decoy = true
		}
	}
	if !target {
		t.Fatalf("expected TakeFooBar to match, got: %+v", syms)
	}
	if decoy {
		t.Fatalf("TakeFooXBar over-matched a literal _ search: %+v", syms)
	}
}

func TestSearchFileContentFTSSemantics(t *testing.T) {
	s := seedSearchModule(t)

	cases := []struct {
		name        string
		pattern     string
		wantFile    string // substring of file path that must match
		decoy       string // substring that must NOT appear in results
		expectError bool
	}{
		{
			name:     "hyphen is literal",
			pattern:  "user-service",
			wantFile: "svc.go",
		},
		{
			name:     "leading minus never errors",
			pattern:  "-deprecated",
			wantFile: "svc.go",
		},
		{
			name:     "qualified name substring",
			pattern:  "cli/codemap.Run",
			wantFile: "prefix.go",
		},
		{
			name:     "pkg colon literal",
			pattern:  "pkg:F",
			wantFile: "svc.go",
		},
		{
			name:     "quoted term literal",
			pattern:  `say "hi"`,
			wantFile: "quote.go",
		},
		{
			name:     "multi-word is phrase-AND",
			pattern:  "alpha beta",
			wantFile: "prefix.go",
			decoy:    "only alpha",
		},
		{
			name:     "camelCase fragment matches inside identifier",
			pattern:  "ymbol",
			wantFile: "camel.go",
			decoy:    "symbol body",
		},
		{
			name:     "full camelCase identifier matches",
			pattern:  "GetSymbolBody",
			wantFile: "camel.go",
		},
		{
			name:        "punctuation-only pattern is inert",
			pattern:     "-",
			expectError: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches, err := s.SearchFileContent(tc.pattern, "", false, 0)
			if err != nil {
				t.Fatalf("search %q errored: %v", tc.pattern, err)
			}
			if tc.expectError {
				return
			}
			if tc.wantFile != "" {
				var found bool
				for _, m := range matches {
					if strings.Contains(m.FilePath, tc.wantFile) {
						found = true
					}
				}
				if !found {
					t.Fatalf("pattern %q did not match file %q, got: %+v", tc.pattern, tc.wantFile, matches)
				}
			}
			if tc.decoy != "" {
				for _, m := range matches {
					if strings.Contains(m.Line, tc.decoy) {
						t.Fatalf("pattern %q over-matched decoy content %q: %+v", tc.pattern, tc.decoy, m)
					}
				}
			}
		})
	}
}

// seedAlternationModule indexes files whose identifiers sit on separate
// sides of an alternation so `A|B`, `A B|C`, and the empty-segment rules can
// be pinned end to end through SearchFileContent.
func seedAlternationModule(t *testing.T) *Store {
	t.Helper()

	dir := writeModule(t, map[string]string{
		"alpha.go": "package alt\n\n// alpha one\nfunc Alpha() {}\n",
		"beta.go":  "package alt\n\n// beta two\nfunc Beta() {}\n",
		"both.go":  "package alt\n\n// alpha beta together\nfunc Both() {}\n",
		"ab.go":    "package alt\n\n// ab short\nfunc AB() {}\n",
		"x.go":     "package alt\n\n// xray\nfunc X() {}\n",
	})
	res, files := parseModule(t, dir)
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	return s
}

// TestSearchFileContentFTSAlternation pins literal-mode alternation end to
// end: `A|B` finds either side, `A B|C` means (A AND B) OR C, empty
// segments are inert, and every returned line is consistent with what the
// pattern claims.
func TestSearchFileContentFTSAlternation(t *testing.T) {
	s := seedAlternationModule(t)

	t.Run("either side matches", func(t *testing.T) {
		matches, err := s.SearchFileContent("alpha|beta", "", false, 0)
		if err != nil {
			t.Fatalf("alternation search errored: %v", err)
		}
		for _, want := range []string{"alpha.go", "beta.go", "both.go"} {
			if !hasFile(matches, want) {
				t.Fatalf("alternation missed %s: %+v", want, matches)
			}
		}
		for _, m := range matches {
			if !strings.Contains(m.Line, "alpha") && !strings.Contains(m.Line, "beta") {
				t.Fatalf("returned line matches neither alternative: %+v", m)
			}
		}
	})

	t.Run("mixed AND within OR", func(t *testing.T) {
		matches, err := s.SearchFileContent("alpha beta|gamma", "", false, 0)
		if err != nil {
			t.Fatalf("mixed alternation errored: %v", err)
		}
		if !hasFile(matches, "both.go") {
			t.Fatalf("(alpha AND beta) alternative missed both.go: %+v", matches)
		}
		for _, m := range matches {
			if strings.Contains(m.FilePath, "alpha.go") || strings.Contains(m.FilePath, "beta.go") {
				t.Fatalf("single-side file over-matched the AND alternative: %+v", m)
			}
		}
	})

	t.Run("empty segments are inert", func(t *testing.T) {
		matches, err := s.SearchFileContent("alpha||beta", "", false, 0)
		if err != nil {
			t.Fatalf("empty-segment search errored: %v", err)
		}
		if !hasFile(matches, "alpha.go") || !hasFile(matches, "beta.go") {
			t.Fatalf("empty segment broke alternation: %+v", matches)
		}

		pipeOnly, err := s.SearchFileContent("|", "", false, 0)
		if err != nil {
			t.Fatalf("pipe-only search errored: %v", err)
		}
		if len(pipeOnly) != 0 {
			t.Fatalf("pipe-only pattern should match nothing: %+v", pipeOnly)
		}

		punct, err := s.SearchFileContent("-|-", "", false, 0)
		if err != nil {
			t.Fatalf("punctuation-only search errored: %v", err)
		}
		if len(punct) != 0 {
			t.Fatalf("punctuation-only pattern should match nothing: %+v", punct)
		}
	})

	t.Run("short-token alternation served by full scan", func(t *testing.T) {
		matches, err := s.SearchFileContent("ab|x", "", false, 0)
		if err != nil {
			t.Fatalf("short-token alternation errored: %v", err)
		}
		if !hasFile(matches, "ab.go") {
			t.Fatalf("short-token alternative missed ab.go: %+v", matches)
		}
	})
}

func TestSearchFileContentRegexErrorsSurface(t *testing.T) {
	s := seedSearchModule(t)

	// Invalid pattern must return an error naming the pattern.
	_, err := s.SearchFileContent("[unclosed", "", true, 0)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(err.Error(), "[unclosed") {
		t.Fatalf("expected error to name the pattern, got: %v", err)
	}
}

func TestSearchFileContentValidRegexWorksEndToEnd(t *testing.T) {
	s := seedSearchModule(t)

	matches, err := s.SearchFileContent(`func [A-Z]`, "", true, 0)
	if err != nil {
		t.Fatalf("valid regex errored: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("expected regex search to find Func declarations")
	}
}

// TestSearchFileContentTermsSpanningLinesAreConsistent pins the guarantee that
// FTS row selection and per-line output never disagree: a multi-term pattern
// whose terms appear on different lines in a file selects no row, and the same
// pattern with an exact adjacent occurrence on one line still matches.
func TestSearchFileContentTermsSpanningLinesAreConsistent(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"span.go":  "package lit\n\n// alpha one\n// zero beta\nfunc Span() {}\n",
		"exact.go": "package lit\n\n// alpha beta exact\nfunc Exact() {}\n",
	})
	res, files := parseModule(t, dir)
	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	spanning, err := s.SearchFileContent("alpha beta", "", false, 0)
	if err != nil {
		t.Fatalf("spanning search errored: %v", err)
	}
	for _, m := range spanning {
		if strings.Contains(m.FilePath, "span.go") {
			t.Fatalf("span.go terms are on separate lines but was surfaced: %+v", m)
		}
	}

	exact, err := s.SearchFileContent("alpha beta", "", false, 0)
	if err != nil {
		t.Fatalf("exact search errored: %v", err)
	}
	var found bool
	for _, m := range exact {
		if strings.Contains(m.FilePath, "exact.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("exact phrase match not surfaced: %+v", exact)
	}
}

func TestSanitizeFTSQuery(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"simple", `"simple"`},
		{"user-service", `"user-service"`},
		{"pkg:F", `"pkg:F"`},
		{`say "hi"`, `("say" """hi""")`},
		{"alpha beta", `("alpha" "beta")`},
		{"-", ""},
		{"-deprecated", `"-deprecated"`},
		{"a - b", `("a" "b")`},
		{"alpha|beta", `"alpha" OR "beta"`},
		{"alpha beta|gamma", `("alpha" "beta") OR "gamma"`},
		{"alpha||beta", `"alpha" OR "beta"`},
		{"|", ""},
		{"-|-", ""},
		{" alpha | beta ", `"alpha" OR "beta"`},
	}
	for _, tc := range cases {
		if got := sanitizeFTSQuery(tc.in); got != tc.want {
			t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildSymbolFTSQuery pins the FTS5 expression shape used for symbol
// search: each term becomes a parenthesized OR of the exact phrase and a
// name-column-scoped prefix, and multi-term patterns AND the groups together.
func TestBuildSymbolFTSQuery(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"simple", `("simple" OR {name} : "simple"*)`},
		{"user-service", `("user-service" OR {name} : "user-service"*)`},
		{"pkg:F", `("pkg:F" OR {name} : "pkg:F"*)`},
		{`say "hi"`, `("say" OR {name} : "say"*) AND ("""hi""" OR {name} : """hi"""*)`},
		{"alpha beta", `("alpha" OR {name} : "alpha"*) AND ("beta" OR {name} : "beta"*)`},
		{"-", ""},
		{"-deprecated", `("-deprecated" OR {name} : "-deprecated"*)`},
		{"a - b", `("a" OR {name} : "a"*) AND ("b" OR {name} : "b"*)`},
	}
	for _, tc := range cases {
		if got := buildSymbolFTSQuery(tc.in); got != tc.want {
			t.Errorf("buildSymbolFTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSearchSymbolsPartialIdentifier verifies that a partial identifier
// matches every symbol whose name starts with it (bleve-style prefix search),
// while a name-scoped prefix never over-matches unrelated columns.
func TestSearchSymbolsPartialIdentifier(t *testing.T) {
	s := seedSearchModule(t)

	cases := []struct {
		name     string
		pattern  string
		wantName string // symbol name that must be returned
		decoy    string // symbol name that must NOT be returned
	}{
		{
			name:     "partial identifier matches",
			pattern:  "GetSym",
			wantName: "GetSymbolBody",
			decoy:    "",
		},
		{
			name:     "short partial prefix matches",
			pattern:  "GetS",
			wantName: "GetSymbolBody",
			decoy:    "",
		},
		{
			name:     "exact identifier still matches",
			pattern:  "GetSymbolBody",
			wantName: "GetSymbolBody",
			decoy:    "",
		},
		{
			// The prefix is scoped to the name column, so a term that only
			// appears in the qualified_name column must not over-match.
			name:     "name-scoped prefix avoids cross-column over-match",
			pattern:  "pkg:F",
			wantName: "",
			decoy:    "FooXBar",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			syms, err := s.SearchSymbols(tc.pattern, "", nil, "", false)
			if err != nil {
				t.Fatalf("search %q: %v", tc.pattern, err)
			}
			names := map[string]bool{}
			for _, sym := range syms {
				names[sym.Name] = true
			}
			if tc.wantName != "" && !names[tc.wantName] {
				t.Fatalf("pattern %q did not return %q, got: %v", tc.pattern, tc.wantName, names)
			}
			if tc.decoy != "" && names[tc.decoy] {
				t.Fatalf("pattern %q over-matched decoy %q: %v", tc.pattern, tc.decoy, names)
			}
		})
	}
}
