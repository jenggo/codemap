package extract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// singleFuncComplexity runs the extractor over code and returns the complexity
// recorded for its single function symbol. This exercises the production walk,
// where complexity is computed inline rather than by a standalone inspect.
func singleFuncComplexity(t *testing.T, code string) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", code, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := Run("main", map[string]*ast.File{"test.go": f}, fset, false, nil)

	var funcSyms []Symbol
	for _, s := range res.Symbols {
		if s.Kind == "function" {
			funcSyms = append(funcSyms, s)
		}
	}
	if len(funcSyms) != 1 {
		t.Fatalf("expected exactly 1 function symbol, got %d (%d total symbols)", len(funcSyms), len(res.Symbols))
	}
	return funcSyms[0].Complexity
}

func TestComputeComplexity_Simple(t *testing.T) {
	c := singleFuncComplexity(t, `package main
func simple() {}
`)
	if c != 1 {
		t.Errorf("expected 1, got %d", c)
	}
}

func TestComputeComplexity_If(t *testing.T) {
	c := singleFuncComplexity(t, `package main
func withIf(x int) {
	if x > 0 {
		return
	}
}
`)
	if c != 2 {
		t.Errorf("expected 2, got %d", c)
	}
}

func TestComputeComplexity_For(t *testing.T) {
	c := singleFuncComplexity(t, `package main
func withFor() {
	for i := 0; i < 10; i++ {
		_ = i
	}
}
`)
	if c != 2 {
		t.Errorf("expected 2, got %d", c)
	}
}

func TestComputeComplexity_Switch(t *testing.T) {
	c := singleFuncComplexity(t, `package main
func withSwitch(x int) {
	switch x {
	case 1:
	case 2:
	case 3:
	}
}
`)
	// base=1, 3 cases=3 → 4 (standard McCabe: switch itself not counted)
	if c != 4 {
		t.Errorf("expected 4 (1 base + 3 cases), got %d", c)
	}
}

func TestComputeComplexity_LogicalOps(t *testing.T) {
	c := singleFuncComplexity(t, `package main
func withLogic(a, b bool) {
	if a && b || a {
	}
}
`)
	if c != 4 {
		t.Errorf("expected 4, got %d", c)
	}
}

func TestComputeComplexity_NilBody(t *testing.T) {
	c := singleFuncComplexity(t, `package main
func empty()
`)
	if c != 1 {
		t.Errorf("expected 1 for nil body, got %d", c)
	}
}

// TestComputeComplexity_NestedFuncLitCountsIntoOwner verifies complexity inside
// a func literal nested in a function body still counts toward the enclosing
// function, while a package-scope func literal does not leak into a preceding
// function's count.
func TestComputeComplexity_NestedFuncLitCountsIntoOwner(t *testing.T) {
	fset := token.NewFileSet()
	code := `package main
func first() {
	var _ = func(x int) {
		if x > 0 {
		}
	}
}
var g = func(x int) {
	if x > 0 {
	}
}
`
	f, err := parser.ParseFile(fset, "test.go", code, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := Run("main", map[string]*ast.File{"test.go": f}, fset, false, nil)

	for _, s := range res.Symbols {
		switch s.Name {
		case "first":
			// 1 base + 1 if inside the nested func literal.
			if s.Complexity != 2 {
				t.Errorf("first() complexity = %d, want 2 (nested func lit counted)", s.Complexity)
			}
		case "g":
			// g is a var of function type; its body's `if` must NOT leak into
			// first()'s count (already asserted above).
			if s.Kind != "var" {
				t.Errorf("g() symbol kind = %q, want var", s.Kind)
			}
		}
	}
}

func TestWireNameGenericTags(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{
			name: "cbor tag wins over json",
			code: `package main
type T struct {
	Host string ` +
				"`cbor:\"host\" json:\"hostname\"`" + `
}

`, want: "host",
		},
		{
			name: "json tag",
			code: `package main
type T struct {
	FirstName string ` +
				"`json:\"first_name\"`" + `
}

`, want: "first_name",
		},
		{
			name: "yaml tag",
			code: `package main
type T struct {
	Enabled bool ` +
				"`yaml:\"enabled,omitempty\"`" + `
}
`, want: "enabled",
		},
		{
			name: "toml tag",
			code: `package main
type T struct {
	Port int ` +
				"`toml:\"port\"`" + `
}
`, want: "port",
		},
		{
			name: "bson tag",
			code: `package main
type T struct {
	ID string ` +
				"`bson:\"_id\"`" + `
}
`, want: "_id",
		},
		{
			name: "db tag",
			code: `package main
type T struct {
	UserID int64 ` +
				"`db:\"user_id\"`" + `
}
`, want: "user_id",
		},
		{
			name: "no tag falls back to go name",
			code: `package main
type T struct {
	CreatedAt time.Time
}
`,
			want: "CreatedAt",
		},
		{
			name: "tag with dash is skipped",
			code: `package main
type T struct {
	Secret string ` +
				"`json:\"-\"`" + `
}
`, want: "Secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "test.go", tt.code, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			field := f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List[0]
			goName := "Fallback"
			if len(field.Names) > 0 {
				goName = field.Names[0].Name
			}
			if got := wireName(field, goName); got != tt.want {
				t.Errorf("wireName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExprStringArrayTypes(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{expr: "[4]byte", want: "[4]byte"},
		{expr: "[...]int", want: "[...]int"},
		{expr: "[]string", want: "[]string"},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			expr, err := parser.ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("parse expression: %v", err)
			}
			if got := exprString(expr); got != tt.want {
				t.Fatalf("exprString(%q) = %q, want %q", tt.expr, got, tt.want)
			}
		})
	}
}

// extractSource runs the extractor over one in-memory file, the same way the
// index pipeline does, so symbol kinds and receivers match production.
func extractSource(t *testing.T, code string) *Result {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", code, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return Run("example.com/x", map[string]*ast.File{"test.go": f}, fset, false, nil)
}

func symbolsNamed(res *Result, name string) []Symbol {
	var out []Symbol
	for _, s := range res.Symbols {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// An interface's unexported method is still part of its contract: Go uses it
// for constraint interfaces and internal seams. Dropping the symbol made
// interface_impls fall back to reporting the implementer's whole method set.
func TestExtractUnexportedInterfaceMethod(t *testing.T) {
	res := extractSource(t, `package p

type task interface {
	getIDs() (string, string)
}
`)
	got := symbolsNamed(res, "getIDs")
	if len(got) != 1 {
		t.Fatalf("expected 1 getIDs symbol, got %d (%d total)", len(got), len(res.Symbols))
	}
	sym := got[0]
	if sym.Kind != "method" {
		t.Errorf("kind = %q, want method", sym.Kind)
	}
	if sym.Receiver != "task" {
		t.Errorf("receiver = %q, want task", sym.Receiver)
	}
	if sym.Exported {
		t.Error("exported = true, want false for an unexported method")
	}
	if sym.QualifiedName != "example.com/x.task.getIDs" {
		t.Errorf("qualified name = %q", sym.QualifiedName)
	}
}

// Visibility must be read from the name, not hardcoded: an interface can
// declare both kinds, and each symbol has to carry its own answer.
func TestExtractInterfaceMethodVisibility(t *testing.T) {
	res := extractSource(t, `package p

type Store interface {
	Save() error
	reset()
}
`)
	for _, tc := range []struct {
		name     string
		exported bool
	}{
		{"Save", true},
		{"reset", false},
	} {
		got := symbolsNamed(res, tc.name)
		if len(got) != 1 {
			t.Fatalf("%s: expected 1 symbol, got %d", tc.name, len(got))
		}
		if got[0].Exported != tc.exported {
			t.Errorf("%s: exported = %v, want %v", tc.name, got[0].Exported, tc.exported)
		}
		if got[0].Receiver != "Store" {
			t.Errorf("%s: receiver = %q, want Store", tc.name, got[0].Receiver)
		}
	}
}

// A union-only constraint declares no methods, so it must yield the interface
// symbol and no method symbols — and must not error.
func TestExtractUnionOnlyConstraintHasNoMethods(t *testing.T) {
	res := extractSource(t, `package p

type Number interface {
	int | int64 | float64
}
`)
	types := symbolsNamed(res, "Number")
	if len(types) != 1 {
		t.Fatalf("expected 1 Number symbol, got %d", len(types))
	}
	if types[0].Kind != "interface" {
		t.Errorf("kind = %q, want interface", types[0].Kind)
	}
	for _, s := range res.Symbols {
		if s.Kind == "method" && s.Receiver == "Number" {
			t.Errorf("unexpected method symbol on a union-only constraint: %s", s.QualifiedName)
		}
	}
}

// TestExtractInlineStructFields verifies an inline anonymous struct field keeps
// its own fields. Without them every nested config section rendered as
// "struct{}", so a whole config tree was invisible to show and package.
func TestExtractInlineStructFields(t *testing.T) {
	res := extractSource(t, `package p

type config struct {
	Warns struct {
		Buffer struct {
			Duration    int
			MinBreaches int `+"`json:\"min_breaches\"`"+`
		}
	}
	Named Inner
}

type Inner struct{ A int }
`)

	syms := symbolsNamed(res, "config")
	if len(syms) != 1 {
		t.Fatalf("expected 1 config symbol, got %d", len(syms))
	}
	fields := syms[0].Fields
	if len(fields) != 2 {
		t.Fatalf("expected 2 top-level fields, got %d", len(fields))
	}

	warns := fields[0]
	if warns.GoName != "Warns" || len(warns.Fields) != 1 {
		t.Fatalf("Warns should carry its inline struct fields, got %+v", warns)
	}
	buffer := warns.Fields[0]
	if buffer.GoName != "Buffer" || len(buffer.Fields) != 2 {
		t.Fatalf("Buffer should carry its inline struct fields, got %+v", buffer)
	}
	if got := buffer.Fields[1]; got.WireName != "min_breaches" {
		t.Errorf("nested wire name = %q, want min_breaches", got.WireName)
	}

	// A named type resolves through its own symbol, so it carries no inline
	// subtree and must not be expanded here.
	if named := fields[1]; named.GoName != "Named" || len(named.Fields) != 0 {
		t.Errorf("named type field should not be expanded, got %+v", named)
	}
}

// TestExtractGenericSignatureKeepsTypeParams pins that a generic declaration's
// type-parameter clause survives into the indexed signature. Without it every
// generic function looked non-generic and the signature could not be used to
// call the function.
func TestExtractGenericSignatureKeepsTypeParams(t *testing.T) {
	res := extractSource(t, `package p

func Fetch[T any](label string, fn func() (T, error)) T { var z T; return z }

type Box[T any] struct{ V T }

type Pair[K comparable, V any] struct {
	Key K
	Val V
}
`)

	cases := []struct{ name, want string }{
		{"Fetch", "func[T any](label string, fn func() (T, error)) T"},
		{"Box", "[T any] struct { V T }"},
		{"Pair", "[K comparable, V any] struct { Key K; Val V }"},
	}
	for _, tc := range cases {
		syms := symbolsNamed(res, tc.name)
		if len(syms) != 1 {
			t.Fatalf("%s: expected 1 symbol, got %d", tc.name, len(syms))
		}
		if got := syms[0].Signature; got != tc.want {
			t.Errorf("%s signature = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestExtractNestedStructSignatureDoesNotClaimEmpty pins that a struct field
// with fields renders as struct{...}, not struct{}, so a signature never
// contradicts the fields column.
func TestExtractNestedStructSignatureDoesNotClaimEmpty(t *testing.T) {
	res := extractSource(t, `package p

type cfg struct {
	Warns struct{ Level int }
	Empty struct{}
}
`)
	syms := symbolsNamed(res, "cfg")
	if len(syms) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(syms))
	}
	sig := syms[0].Signature
	if !strings.Contains(sig, "Warns struct{...}") {
		t.Errorf("non-empty nested struct should render struct{...}, got %q", sig)
	}
	if !strings.Contains(sig, "Empty struct{}") {
		t.Errorf("empty nested struct should stay struct{}, got %q", sig)
	}
}

// TestIotaConstantsCarryResolvedValue pins that an iota block reports each
// constant's value. Without it the first constant's signature was the literal
// "iota" and every later one was empty, so the only thing a reader looks up an
// enum constant for had to be re-derived from source.
func TestIotaConstantsCarryResolvedValue(t *testing.T) {
	src := `package p

type state int

const (
	stateNew state = iota
	stateReady
	stateClosed
)

const (
	flagA = 1 << iota
	flagB
	flagC
)

const (
	offset = iota + 10
	offset2
)

const name = "not-an-int"
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	syms := Run("p", map[string]*ast.File{"test.go": f}, fset, false, nil).Symbols
	want := map[string]string{
		"p.stateNew":    "0",
		"p.stateReady":  "1",
		"p.stateClosed": "2",
		"p.flagA":       "1",
		"p.flagB":       "2",
		"p.flagC":       "4",
		"p.offset":      "10",
		"p.offset2":     "11",
		"p.name":        `"not-an-int"`,
	}
	for _, sym := range syms {
		if exp, ok := want[sym.QualifiedName]; ok {
			if sym.Signature != exp {
				t.Errorf("%s signature = %q, want %q", sym.QualifiedName, sym.Signature, exp)
			}
		}
	}
}
