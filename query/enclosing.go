package query

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"slices"

	"codemap/parse"
	"codemap/store"
)

// enclosingSymbol returns the innermost declaration on the ancestor stack that
// maps to an indexed symbol. Pattern search and position lookup share this walk
// so they cannot disagree about which symbol contains a node.
func enclosingSymbol(fset *token.FileSet, stack []ast.Node, byLine map[int][]store.Symbol) (store.Symbol, bool) {
	for _, n := range slices.Backward(stack) {
		name, line, ok := declaredName(fset, n)
		if !ok {
			continue
		}
		if sym, found := symbolAt(byLine[line], name); found {
			return sym, true
		}
	}
	return store.Symbol{}, false
}

func symbolAt(syms []store.Symbol, name string) (store.Symbol, bool) {
	for _, sym := range syms {
		if sym.Name == name {
			return sym, true
		}
	}
	return store.Symbol{}, false
}

// declaredName returns the declared name and start line of a declaration node,
// matching the position the index records for its symbol.
func declaredName(fset *token.FileSet, n ast.Node) (string, int, bool) {
	switch d := n.(type) {
	case *ast.FuncDecl:
		if d.Name == nil {
			return "", 0, false
		}
		return d.Name.Name, fset.Position(d.Pos()).Line, true
	case *ast.TypeSpec:
		if d.Name == nil {
			return "", 0, false
		}
		return d.Name.Name, fset.Position(d.Pos()).Line, true
	case *ast.ValueSpec:
		if len(d.Names) == 0 {
			return "", 0, false
		}
		return d.Names[0].Name, fset.Position(d.Pos()).Line, true
	}
	return "", 0, false
}

// EnclosingSymbolAt parses filePath and returns the innermost indexed symbol
// whose declaration span contains the 1-based line and column. Resolution is by
// AST span, not by the index's recorded line, because a position inside a
// function body has no symbol row of its own.
//
// A position outside every declaration returns false with no error; a line past
// the end of the file is an error naming the file's actual line count.
func EnclosingSymbolAt(s *store.Store, filePath string, line, col int) (store.Symbol, bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return store.Symbol{}, false, fmt.Errorf("read source file %s: %w", filePath, err)
	}
	fset := token.NewFileSet()
	file, err := parse.File(fset, filePath, data)
	if err != nil {
		return store.Symbol{}, false, fmt.Errorf("parse source file %s: %w", filePath, err)
	}
	pos, err := lineColPos(fset, file, filePath, line, col)
	if err != nil {
		return store.Symbol{}, false, err
	}

	symbols, err := symbolsByFile(s)
	if err != nil {
		return store.Symbol{}, false, err
	}
	byLine := make(map[int][]store.Symbol, len(symbols[filePath]))
	for _, sym := range symbols[filePath] {
		byLine[sym.PosLine] = append(byLine[sym.PosLine], sym)
	}

	sym, ok := enclosingSymbol(fset, ancestorStack(file, pos), byLine)
	return sym, ok, nil
}

// lineColPos converts a 1-based line and column to a token position, clamping a
// column past the line's end to the line start so a stale column still resolves.
func lineColPos(fset *token.FileSet, file *ast.File, filePath string, line, col int) (token.Pos, error) {
	tf := fset.File(file.Pos())
	if line < 1 || line > tf.LineCount() {
		return token.NoPos, fmt.Errorf("line %d is out of range: %s has %d lines", line, filePath, tf.LineCount())
	}
	start := tf.LineStart(line)
	if col < 2 {
		return start, nil
	}
	end := token.Pos(tf.Base() + tf.Size())
	if line < tf.LineCount() {
		end = tf.LineStart(line + 1)
	}
	if pos := start + token.Pos(col-1); pos < end {
		return pos, nil
	}
	return start, nil
}

// ancestorStack walks file down to the innermost node containing pos and returns
// the chain of enclosing nodes, outermost first. The deepest chain seen during
// the walk is kept, because the live stack is unwound by the time Inspect
// returns.
func ancestorStack(file *ast.File, pos token.Pos) []ast.Node {
	var stack, deepest []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return false
		}
		if n.Pos() <= pos && pos < n.End() {
			stack = append(stack, n)
			if len(stack) > len(deepest) {
				deepest = slices.Clone(stack)
			}
			return true
		}
		return false
	})
	return deepest
}
