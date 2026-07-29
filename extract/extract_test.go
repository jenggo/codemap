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
