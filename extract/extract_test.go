package extract

import (
	"go/ast"
	"go/parser"
	"go/token"
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
	res := Run("main", map[string]*ast.File{"test.go": f}, fset, false)

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
	res := Run("main", map[string]*ast.File{"test.go": f}, fset, false)

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
