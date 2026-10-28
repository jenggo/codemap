package pattern

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"slices"
	"testing"
)

func TestCompileValidPatterns(t *testing.T) {
	for _, src := range []string{
		"if err != nil { return $ERR }",
		"go func() { $BODY }()",
		"defer $CALL",
		"$A() && $A()",
		"$X := $Y",
		"func $NAME() {}",
	} {
		if _, err := Compile(src); err != nil {
			t.Errorf("Compile(%q) = %v, want success", src, err)
		}
	}
}

func TestCompileInvalidPatterns(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"unparseable", "if err != nil {"},
		{"multiple statements", "x := 1\ny := 2"},
		{"multiple declarations", "func a() {}\nfunc b() {}"},
		{"stray dollar", "$"},
	} {
		if _, err := Compile(tc.src); err == nil {
			t.Errorf("%s: Compile(%q) succeeded, want an explicit error", tc.name, tc.src)
		}
	}
}

func TestMatchDeferBindsWholeCall(t *testing.T) {
	pat, err := Compile("defer $CALL")
	if err != nil {
		t.Fatal(err)
	}
	fset, matches := collectMatches(t, pat, "package p\nfunc f(ctx context.Context) {\n\tdefer cleanup(ctx)\n}\n")
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if got := renderNode(t, fset, matches[0]["CALL"]); got != "cleanup(ctx)" {
		t.Fatalf("$CALL = %q, want cleanup(ctx)", got)
	}
}

func TestRepeatedMetavariableRequiresIdenticalNodes(t *testing.T) {
	pat, err := Compile("$A() && $A()")
	if err != nil {
		t.Fatal(err)
	}

	fset, same := collectMatches(t, pat, "package p\nfunc f() bool {\n\treturn f() && f()\n}\n")
	if len(same) != 1 {
		t.Fatalf("f() && f(): matches = %d, want 1", len(same))
	}
	if got := renderNode(t, fset, same[0]["A"]); got != "f()" {
		t.Fatalf("$A = %q, want f()", got)
	}

	if _, diff := collectMatches(t, pat, "package p\nfunc f() bool {\n\treturn f() && g()\n}\n"); len(diff) != 0 {
		t.Fatalf("f() && g(): matches = %d, want 0", len(diff))
	}
}

func TestMatchStatementPatternBindsSubtree(t *testing.T) {
	pat, err := Compile("if err != nil { return $ERR }")
	if err != nil {
		t.Fatal(err)
	}

	fset, matches := collectMatches(t, pat, "package p\nfunc f() error {\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n")
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if got := renderNode(t, fset, matches[0]["ERR"]); got != "err" {
		t.Fatalf("$ERR = %q, want err", got)
	}

	if _, none := collectMatches(t, pat, "package p\nfunc f() error {\n\tif err == nil {\n\t\treturn nil\n\t}\n\treturn nil\n}\n"); len(none) != 0 {
		t.Fatalf("err == nil: matches = %d, want 0", len(none))
	}
}

func TestMatchGoLiteralPattern(t *testing.T) {
	pat, err := Compile("go func() { $BODY }()")
	if err != nil {
		t.Fatal(err)
	}
	fset, matches := collectMatches(t, pat, "package p\nfunc f() {\n\tgo func() { doWork() }()\n}\n")
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if got := renderNode(t, fset, matches[0]["BODY"]); got != "doWork()" {
		t.Fatalf("$BODY = %q, want doWork()", got)
	}
}

func TestMatchBareMetavariable(t *testing.T) {
	pat, err := Compile("$A")
	if err != nil {
		t.Fatal(err)
	}
	if _, matches := collectMatches(t, pat, "package p\nfunc f() {\n\tg()\n}\n"); len(matches) == 0 {
		t.Fatal("a bare metavariable must match nodes")
	}
}

func TestMatchDeclarationPattern(t *testing.T) {
	pat, err := Compile("func $NAME() {}")
	if err != nil {
		t.Fatal(err)
	}
	fset, matches := collectMatches(t, pat, "package p\nfunc Handler() {}\nfunc Other(x int) {}\n")
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if got := renderNode(t, fset, matches[0]["NAME"]); got != "Handler" {
		t.Fatalf("$NAME = %q, want Handler", got)
	}
}

func TestLiteralsExcludeMetavariables(t *testing.T) {
	pat, err := Compile("if err != nil { return $ERR }")
	if err != nil {
		t.Fatal(err)
	}
	got := pat.Literals()
	for _, want := range []string{"if", "err", "nil", "return"} {
		if !slices.Contains(got, want) {
			t.Errorf("Literals() = %v, missing %q", got, want)
		}
	}
	for _, lit := range got {
		if isPlaceholder(lit) {
			t.Errorf("Literals() leaked the placeholder %q", lit)
		}
	}
}

func TestDeferLiterals(t *testing.T) {
	pat, err := Compile("defer $CALL")
	if err != nil {
		t.Fatal(err)
	}
	if got := pat.Literals(); !slices.Equal(got, []string{"defer"}) {
		t.Fatalf("Literals() = %v, want [defer]", got)
	}
}

func TestSubstituteLeavesLiteralsUntouched(t *testing.T) {
	src := "f(\"$A\", '$B', `$C`) + $D // $E\n"
	want := "f(\"$A\", '$B', `$C`) + __codemap_mv_D // $E\n"
	if got := substitute(src); got != want {
		t.Fatalf("substitute() = %q, want %q", got, want)
	}
}

func TestMetavariableInStringLiteralIsLiteral(t *testing.T) {
	pat, err := Compile(`f("$A")`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	_, matches := collectMatches(t, pat, "package p\nfunc g() {\n\tf(\"$A\")\n}\n")
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	if len(matches[0]) != 0 {
		t.Fatalf("bindings = %v, want none: $A inside a string is not a metavariable", matches[0])
	}

	if _, none := collectMatches(t, pat, "package p\nfunc g() {\n\tf(\"x\")\n}\n"); len(none) != 0 {
		t.Fatalf("f(\"x\"): matches = %d, want 0", len(none))
	}
}

// collectMatches parses src and returns the bindings of every matching node.
func collectMatches(t *testing.T, pat *Pattern, src string) (*token.FileSet, []map[string]ast.Node) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "candidate.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse candidate: %v", err)
	}
	var matches []map[string]ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if bindings, ok := pat.Match(fset, n); ok {
			matches = append(matches, bindings)
		}
		return true
	})
	return fset, matches
}

func renderNode(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	if n == nil {
		t.Fatal("expected a binding, got nil")
	}
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, n); err != nil {
		t.Fatalf("print node: %v", err)
	}
	return b.String()
}

// TestCompositeLiteralEllipsis pins partial matching for composite literals.
// `$T{...}` and `$T{$F: $V, ...}` are not valid Go, so without the ellipsis
// sentinel they were a parse error or a no-match against any literal that
// carried fields the pattern omitted.
func TestCompositeLiteralEllipsis(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		src     string
		want    bool
	}{
		{"any literal", `$T{...}`, `return types.ServerSync{IP: s.IP, Name: s.Name}`, true},
		{"empty literal", `$T{...}`, `return types.ServerSync{}`, true},
		{"partial fields", `$T{$F: $V, ...}`, `return types.ServerSync{IP: s.IP, Name: s.Name}`, true},
		{"partial fields exact", `$T{$F: $V, ...}`, `return types.ServerSync{IP: s.IP}`, true},
		{"partial fields wrong key", `$T{IP: $V, ...}`, `return types.ServerSync{Name: s.Name}`, false},
		{"no ellipsis still exact", `$T{$F: $V}`, `return types.ServerSync{IP: s.IP, Name: s.Name}`, false},
		{"variadic call untouched", `f($X...)`, `f(xs...)`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(tc.pattern)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tc.pattern, err)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", "package x\n\nfunc F() {\n\t"+tc.src+"\n}\n", 0)
			if err != nil {
				t.Fatalf("parse candidate: %v", err)
			}
			matched := false
			ast.Inspect(file, func(n ast.Node) bool {
				if n == nil || matched {
					return false
				}
				if _, ok := p.Match(fset, n); ok {
					matched = true
				}
				return true
			})
			if matched != tc.want {
				t.Errorf("%q against %q = %v, want %v", tc.pattern, tc.src, matched, tc.want)
			}
		})
	}
}

// TestEllipsisSentinelIsNotAPrefilterLiteral pins that the ellipsis marker does
// not become a required literal token. It was added to the literal set, so the
// file prefilter demanded the text "_cmEllipsis" and dropped every file,
// returning "no matches" for a pattern that matched in unit tests.
func TestEllipsisSentinelIsNotAPrefilterLiteral(t *testing.T) {
	for _, src := range []string{`$T{...}`, `$T{$F: $V, ...}`} {
		p, err := Compile(src)
		if err != nil {
			t.Fatalf("Compile(%q): %v", src, err)
		}
		for _, lit := range p.Literals() {
			if lit == ellipsisSentinel {
				t.Errorf("Compile(%q) literals = %v, must not require %q", src, p.Literals(), ellipsisSentinel)
			}
		}
	}
}

// TestVariadicMetavariableRequiresVariadicCall pins that "$F($X...)" only
// matches a call forwarding a variadic argument. CallExpr.Ellipsis is a
// token.Pos, which the matcher skipped as a source position, so the pattern
// matched every call in the repo.
func TestVariadicMetavariableRequiresVariadicCall(t *testing.T) {
	pat, err := Compile("$F($X...)")
	if err != nil {
		t.Fatal(err)
	}

	variadic := "package p\nfunc f(xs []int) {\n\tg(xs...)\n}\n"
	if _, got := collectMatches(t, pat, variadic); len(got) != 1 {
		t.Fatalf("g(xs...): matches = %d, want 1", len(got))
	}

	plain := "package p\nfunc f(x int) {\n\tg(x)\n}\n"
	if _, got := collectMatches(t, pat, plain); len(got) != 0 {
		t.Fatalf("g(x): matches = %d, want 0", len(got))
	}
}

// TestNonVariadicPatternSkipsVariadicCalls is the other half: a pattern without
// the marker must not match a variadic call.
func TestNonVariadicPatternSkipsVariadicCalls(t *testing.T) {
	pat, err := Compile("g($X)")
	if err != nil {
		t.Fatal(err)
	}
	src := "package p\nfunc f(xs []int, x int) {\n\tg(xs...)\n\tg(x)\n}\n"
	_, got := collectMatches(t, pat, src)
	if len(got) != 1 {
		t.Fatalf("g($X) over g(xs...) and g(x): matches = %d, want 1", len(got))
	}
}
