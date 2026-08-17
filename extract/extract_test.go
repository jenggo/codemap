package extract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func parseFunc(t *testing.T, code string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", code, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			return fd
		}
	}
	t.Fatal("no FuncDecl found")
	return nil
}

func TestComputeComplexity_Simple(t *testing.T) {
	fd := parseFunc(t, `package main
func simple() {}
`)
	c := computeComplexity(fd)
	if c != 1 {
		t.Errorf("expected 1, got %d", c)
	}
}

func TestComputeComplexity_If(t *testing.T) {
	fd := parseFunc(t, `package main
func withIf(x int) {
	if x > 0 {
		return
	}
}
`)
	c := computeComplexity(fd)
	if c != 2 {
		t.Errorf("expected 2, got %d", c)
	}
}

func TestComputeComplexity_For(t *testing.T) {
	fd := parseFunc(t, `package main
func withFor() {
	for i := 0; i < 10; i++ {
		_ = i
	}
}
`)
	c := computeComplexity(fd)
	if c != 2 {
		t.Errorf("expected 2, got %d", c)
	}
}

func TestComputeComplexity_Switch(t *testing.T) {
	fd := parseFunc(t, `package main
func withSwitch(x int) {
	switch x {
	case 1:
	case 2:
	case 3:
	}
}
`)
	c := computeComplexity(fd)
	// base=1, 3 cases=3 → 4 (standard McCabe: switch itself not counted)
	if c != 4 {
		t.Errorf("expected 4 (1 base + 3 cases), got %d", c)
	}
}

func TestComputeComplexity_LogicalOps(t *testing.T) {
	fd := parseFunc(t, `package main
func withLogic(a, b bool) {
	if a && b || a {
	}
}
`)
	c := computeComplexity(fd)
	if c != 4 {
		t.Errorf("expected 4, got %d", c)
	}
}

func TestComputeComplexity_NilBody(t *testing.T) {
	code := `package main
func empty()
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", code, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			c := computeComplexity(fd)
			if c != 1 {
				t.Errorf("expected 1 for nil body, got %d", c)
			}
			return
		}
	}
	t.Fatal("no FuncDecl found")
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
