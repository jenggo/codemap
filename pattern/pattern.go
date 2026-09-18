// Package pattern compiles Go source snippets containing $UPPERCASE
// metavariables into structural matchers over go/ast. Matching is syntactic:
// go/types is never consulted.
package pattern

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"reflect"
	"strings"
)

// placeholderPrefix marks a substituted metavariable identifier. It is not a
// legal Go identifier a user would write, so it cannot collide with pattern
// source.
const placeholderPrefix = "__codemap_mv_"

// Pattern is a compiled matcher for one expression, statement, or declaration
// node.
type Pattern struct {
	root     ast.Node
	literals []string
}

// Compile rewrites the pattern's metavariables, parses it into a single
// matchable node, and extracts the literal tokens used for pre-filtering. A
// pattern that cannot be parsed, or that reduces to zero or more than one
// top-level node, fails with an explicit error.
func Compile(src string) (*Pattern, error) {
	substituted := substitute(src)
	root, err := compileNode(substituted)
	if err != nil {
		return nil, err
	}
	return &Pattern{root: root, literals: literalTokens(substituted)}, nil
}

// Literals returns the literal tokens (identifiers, keywords, and basic
// literals) of the pattern, excluding metavariables. Every file containing a
// match must contain each literal verbatim, so they are the safe pre-filter.
func (p *Pattern) Literals() []string {
	out := make([]string, len(p.literals))
	copy(out, p.literals)
	return out
}

// Match reports whether node matches the pattern. On success it returns the
// metavariable bindings keyed by metavariable name; on failure the bindings are
// nil.
func (p *Pattern) Match(fset *token.FileSet, node ast.Node) (map[string]ast.Node, bool) {
	if node == nil {
		return nil, false
	}
	m := &matcher{fset: fset, bindings: make(map[string]ast.Node)}
	if !m.matchValue(reflect.ValueOf(p.root), reflect.ValueOf(node)) {
		return nil, false
	}
	return m.bindings, true
}

func compileNode(src string) (ast.Node, error) {
	if expr, err := parser.ParseExpr(src); err == nil {
		return expr, nil
	}
	stmts, stmtErr := parseStatements(src)
	if stmtErr == nil {
		switch len(stmts) {
		case 1:
			return stmts[0], nil
		case 0:
			return nil, fmt.Errorf("unsupported pattern: no statement found")
		default:
			return nil, fmt.Errorf("unsupported pattern: expected a single statement, got %d", len(stmts))
		}
	}
	decls, declErr := parseDeclarations(src)
	if declErr != nil {
		return nil, fmt.Errorf("invalid pattern: %w", stmtErr)
	}
	if len(decls) != 1 {
		return nil, fmt.Errorf("unsupported pattern: expected a single declaration, got %d", len(decls))
	}
	return decls[0], nil
}

// parseStatements wraps the snippet in a function body so statements (including
// control flow) parse.
func parseStatements(src string) ([]ast.Stmt, error) {
	const wrapper = "package p\nfunc _() {\n"
	f, err := parser.ParseFile(token.NewFileSet(), "pattern.go", wrapper+src+"\n}\n", parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			return fn.Body.List, nil
		}
	}
	return nil, fmt.Errorf("no function body")
}

// parseDeclarations parses the snippet at package level so declarations (e.g.
// func and import) parse.
func parseDeclarations(src string) ([]ast.Decl, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "pattern.go", "package p\n"+src+"\n", parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	return f.Decls, nil
}

// substitute rewrites each $NAME metavariable to a placeholder identifier. A
// metavariable that is the entire expression of a defer or go statement is
// emitted as a zero-argument call of the placeholder, because Go's parser
// rejects a bare identifier in those positions; the matcher recognizes that
// shape as the metavariable itself. String, rune, and comment literals are
// copied verbatim: $ is only a metavariable in code positions, and rewriting a
// literal would corrupt it.
func substitute(src string) string {
	literals := literalRanges(src)
	var b strings.Builder
	next := 0
	for i := 0; i < len(src); {
		if next < len(literals) && i == literals[next].start {
			lit := literals[next]
			b.WriteString(src[lit.start:lit.end])
			i = lit.end
			next++
			continue
		}
		name, end, ok := metavariableAt(src, i)
		if !ok {
			b.WriteByte(src[i])
			i++
			continue
		}
		b.WriteString(placeholderPrefix)
		b.WriteString(name)
		if wholeDeferOrGoOperand(src[:i], src[end:]) {
			b.WriteString("()")
		}
		i = end
	}
	return b.String()
}

// span is a half-open byte range [start, end) of the pattern source.
type span struct {
	start int
	end   int
}

// literalRanges returns the byte ranges of the pattern's string, rune, and
// comment literals, in source order. The token's end is the start of the next
// token rather than the literal's length, because the scanner strips carriage
// returns from raw string and comment values.
func literalRanges(src string) []span {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, []byte(src), nil, scanner.ScanComments)

	var spans []span
	start := -1
	for {
		pos, tok, _ := s.Scan()
		off := fset.Position(pos).Offset
		if start >= 0 && off > start {
			spans = append(spans, span{start, off})
			start = -1
		}
		if tok == token.EOF {
			return spans
		}
		if tok == token.STRING || tok == token.CHAR || tok == token.COMMENT {
			start = off
		}
	}
}

// metavariableAt returns the name of the $UPPERCASE metavariable starting at i
// and the index just past it.
func metavariableAt(src string, i int) (string, int, bool) {
	if src[i] != '$' {
		return "", 0, false
	}
	j := i + 1
	for j < len(src) && src[j] >= 'A' && src[j] <= 'Z' {
		j++
	}
	if j == i+1 {
		return "", 0, false
	}
	return src[i+1 : j], j, true
}

// wholeDeferOrGoOperand reports whether a metavariable at the end of prefix is
// the whole operand of a defer/go statement and is not already followed by a
// call.
func wholeDeferOrGoOperand(prefix, rest string) bool {
	fields := strings.Fields(prefix)
	if len(fields) == 0 {
		return false
	}
	last := fields[len(fields)-1]
	if last != "defer" && last != "go" {
		return false
	}
	return !strings.HasPrefix(strings.TrimLeft(rest, " \t"), "(")
}

// literalTokens extracts identifiers, keywords, and basic literals from the
// substituted source, skipping metavariable placeholders.
func literalTokens(src string) []string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, []byte(src), nil, 0)

	seen := make(map[string]bool)
	var out []string
	add := func(tok string) {
		if tok == "" || seen[tok] || isPlaceholder(tok) {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		switch {
		case tok == token.IDENT:
			add(lit)
		case tok.IsKeyword():
			add(tok.String())
		case tok == token.INT, tok == token.FLOAT, tok == token.IMAG, tok == token.CHAR, tok == token.STRING:
			add(lit)
		}
	}
	return out
}

func isPlaceholder(name string) bool {
	return strings.HasPrefix(name, placeholderPrefix)
}

func metaName(name string) string {
	return strings.TrimPrefix(name, placeholderPrefix)
}

// matcher walks the pattern and candidate ASTs in parallel, capturing
// metavariables and enforcing repeated-metavariable consistency.
type matcher struct {
	fset     *token.FileSet
	bindings map[string]ast.Node
}

var (
	identPtrType     = reflect.TypeFor[*ast.Ident]()
	callPtrType      = reflect.TypeFor[*ast.CallExpr]()
	posType          = reflect.TypeFor[token.Pos]()
	commentGroupType = reflect.TypeFor[*ast.CommentGroup]()
)

// skipField reports whether an AST struct field carries no matched structure:
// source positions, comments, and the deprecated ident->object/scope links
// differ between independently parsed trees.
func skipField(f reflect.StructField) bool {
	if f.Type == posType || f.Type == commentGroupType {
		return true
	}
	return f.Name == "Obj" || f.Name == "Scope"
}

func (m *matcher) matchValue(pv, cv reflect.Value) bool {
	// Metavariables are checked before the type comparison because a
	// metavariable matches a node of any type.
	if name, ok := metavariableName(pv); ok {
		return m.bindCandidate(name, cv)
	}
	if pv.Type() != cv.Type() {
		return false
	}
	kind := pv.Kind()
	if kind == reflect.Interface || kind == reflect.Pointer {
		return m.matchIndirect(pv, cv)
	}
	if kind == reflect.Struct {
		return m.matchStruct(pv, cv)
	}
	if kind == reflect.Slice {
		return m.matchSlice(pv, cv)
	}
	return pv.Interface() == cv.Interface()
}

func (m *matcher) bindCandidate(name string, cv reflect.Value) bool {
	node, ok := asNode(cv)
	if !ok {
		return false
	}
	return m.bind(name, node)
}

func (m *matcher) matchIndirect(pv, cv reflect.Value) bool {
	if pv.IsNil() || cv.IsNil() {
		return pv.IsNil() && cv.IsNil()
	}
	return m.matchValue(pv.Elem(), cv.Elem())
}

func (m *matcher) matchSlice(pv, cv reflect.Value) bool {
	if pv.Len() != cv.Len() {
		return false
	}
	for i := 0; i < pv.Len(); i++ {
		if !m.matchValue(pv.Index(i), cv.Index(i)) {
			return false
		}
	}
	return true
}

// metavariableName reports whether the pattern value is a metavariable: a
// placeholder identifier, or the zero-argument call of a placeholder produced
// for a defer/go operand.
func metavariableName(pv reflect.Value) (string, bool) {
	if pv.Kind() == reflect.Interface {
		if pv.IsNil() {
			return "", false
		}
		pv = pv.Elem()
	}
	if pv.Kind() != reflect.Pointer || pv.IsNil() {
		return "", false
	}
	switch pv.Type() {
	case identPtrType:
		id := pv.Interface().(*ast.Ident)
		if isPlaceholder(id.Name) {
			return metaName(id.Name), true
		}
	case callPtrType:
		return metavarCallName(pv.Interface().(*ast.CallExpr))
	}
	return "", false
}

// asNode unwraps one interface level of a candidate value and returns its node.
func asNode(v reflect.Value) (ast.Node, bool) {
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, false
	}
	return reflect.TypeAssert[ast.Node](v)
}

func (m *matcher) matchStruct(pv, cv reflect.Value) bool {
	t := pv.Type()
	for i := 0; i < pv.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || skipField(f) {
			continue
		}
		if !m.matchValue(pv.Field(i), cv.Field(i)) {
			return false
		}
	}
	return true
}

// metavarCallName reports whether call is the metavariable reference produced
// for a defer/go operand: a zero-argument call whose callee is a placeholder.
// Such a call stands for the metavariable itself and matches the whole
// candidate call.
func metavarCallName(call *ast.CallExpr) (string, bool) {
	if call == nil || len(call.Args) != 0 || call.Ellipsis != token.NoPos {
		return "", false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || !isPlaceholder(id.Name) {
		return "", false
	}
	return metaName(id.Name), true
}

// bind records node under name, or verifies an existing binding is structurally
// identical.
func (m *matcher) bind(name string, node ast.Node) bool {
	if bound, ok := m.bindings[name]; ok {
		return m.equal(bound, node)
	}
	m.bindings[name] = node
	return true
}

// equal compares two candidate nodes by rendering them with go/printer; the
// rendering is position-independent, so independently parsed trees compare on
// structure alone.
func (m *matcher) equal(a, b ast.Node) bool {
	var ab, bb bytes.Buffer
	if err := printer.Fprint(&ab, m.fset, a); err != nil {
		return false
	}
	if err := printer.Fprint(&bb, m.fset, b); err != nil {
		return false
	}
	return ab.String() == bb.String()
}
