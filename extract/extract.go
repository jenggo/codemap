package extract

import (
	"go/ast"
	"go/token"
	"reflect"
	"strconv"
	"strings"
)

type Position struct {
	File string
	Line int
}

// StructField records one struct field's Go name, effective wire name (from
// the first recognized serialization tag — cbor, json, yaml, toml, bson, db —
// falling back to the Go name) and Go type. Contract analysis compares structs
// on effective wire name + type, so fields are captured at index time and
// served from the symbol table, never re-walked.
type StructField struct {
	GoName   string `json:"go_name,omitempty"`
	WireName string `json:"wire_name,omitempty"`
	GoType   string `json:"go_type,omitempty"`
}

type Symbol struct {
	QualifiedName string
	Name          string
	Kind          string
	Receiver      string
	Signature     string
	Doc           string
	Pos           Position
	Fields        []StructField
	Complexity    int
	Exported      bool
	IsTest        bool
}

type Edge struct {
	FromRef  string
	ToRef    string
	EdgeType string
	Pos      Position
}

type Result struct {
	Symbols []Symbol
	Edges   []Edge
}

func Run(pkgImportPath string, files map[string]*ast.File, fset *token.FileSet, isTest bool) *Result {
	result := &Result{}

	for _, astFile := range files {
		extractor := &fileExtractor{
			pkgPath: pkgImportPath,
			astFile: astFile,
			fset:    fset,
			isTest:  isTest,
			result:  result,
		}
		extractor.extract()
	}

	return result
}

type fileExtractor struct {
	astFile *ast.File
	fset    *token.FileSet
	result  *Result
	pkgPath string
	isTest  bool
}

func (e *fileExtractor) extract() {
	e.extractImports()

	if !e.isTest {
		pos := e.fset.Position(e.astFile.Pos())
		if strings.HasSuffix(pos.Filename, "_test.go") {
			e.isTest = true
		}
	}

	for _, decl := range e.astFile.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			e.extractFuncDecl(d)
		case *ast.GenDecl:
			e.extractGenDecl(d)
		}
	}
}

func (e *fileExtractor) extractFuncDecl(fd *ast.FuncDecl) {
	kind := "function"
	receiver := ""
	qualifiedName := e.pkgPath + "." + fd.Name.Name

	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		kind = "method"
		receiver = typeString(fd.Recv.List[0].Type)
		qualifiedName = e.pkgPath + "." + receiver + "." + fd.Name.Name
	}

	pos := e.fset.Position(fd.Pos())
	sig := signatureString(fd.Type)

	e.result.Symbols = append(e.result.Symbols, Symbol{
		QualifiedName: qualifiedName,
		Name:          fd.Name.Name,
		Kind:          kind,
		Receiver:      receiver,
		Signature:     sig,
		Doc:           docString(fd.Doc),
		Pos: Position{
			File: pos.Filename,
			Line: pos.Line,
		},
		Exported:   fd.Name.IsExported(),
		IsTest:     e.isTest,
		Complexity: computeComplexity(fd),
	})
}

func (e *fileExtractor) extractGenDecl(gd *ast.GenDecl) {
	kind := tokenKind(gd.Tok)
	if kind == "" {
		return
	}

	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			e.extractTypeSpec(s, kind, gd.Doc)
		case *ast.ValueSpec:
			e.extractValueSpec(s, kind, gd.Doc)
		}
	}
}

func tokenKind(tok token.Token) string {
	if tok == token.TYPE {
		return "type"
	}
	if tok == token.CONST {
		return "const"
	}
	if tok == token.VAR {
		return "var"
	}
	return ""
}

const kindType = "type"

func (e *fileExtractor) extractTypeSpec(ts *ast.TypeSpec, kind string, doc *ast.CommentGroup) {
	pos := e.fset.Position(ts.Pos())
	qualifiedName := e.pkgPath + "." + ts.Name.Name

	actualKind := kind
	if ts.Assign.IsValid() {
		actualKind = kindType
	}
	if _, ok := ts.Type.(*ast.InterfaceType); ok {
		actualKind = "interface"
	}
	if _, ok := ts.Type.(*ast.StructType); ok {
		actualKind = kindType
	}

	sym := Symbol{
		QualifiedName: qualifiedName,
		Name:          ts.Name.Name,
		Kind:          actualKind,
		Signature:     typeSignatureString(ts.Type),
		Doc:           docString(doc),
		Pos: Position{
			File: pos.Filename,
			Line: pos.Line,
		},
		Exported: ts.Name.IsExported(),
		IsTest:   e.isTest,
	}
	if st, ok := ts.Type.(*ast.StructType); ok {
		sym.Fields = extractStructFields(st)
	}

	e.result.Symbols = append(e.result.Symbols, sym)
}

// extractStructFields captures each field's Go name, effective CBOR name and
// type. Embedded fields (no name) use their type as the Go name.
func extractStructFields(st *ast.StructType) []StructField {
	if st.Fields == nil {
		return nil
	}
	fields := make([]StructField, 0, len(st.Fields.List))
	for _, f := range st.Fields.List {
		var goName string
		if len(f.Names) > 0 {
			goName = f.Names[0].Name
		} else {
			goName = exprString(f.Type)
		}
		fields = append(fields, StructField{
			GoName:   goName,
			WireName: wireName(f, goName),
			GoType:   exprString(f.Type),
		})
	}
	return fields
}

// wireTagPriority lists the serialization tags consulted to derive a field's
// effective wire name, most wire-specific first. The first tag present wins.
var wireTagPriority = []string{"cbor", "json", "yaml", "toml", "bson", "db"}

// wireName resolves the effective wire name for a field: the value of the
// first recognized serialization tag (`cbor`, `json`, `yaml`, `toml`, `bson`,
// `db`) when present, otherwise the Go field name.
func wireName(f *ast.Field, goName string) string {
	if f.Tag == nil {
		return goName
	}
	tag, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return goName
	}
	st := reflect.StructTag(tag)
	for _, key := range wireTagPriority {
		if name, ok := st.Lookup(key); ok {
			// Tag may include options after a comma (e.g. "name,omitempty").
			if i := strings.Index(name, ","); i >= 0 {
				name = name[:i]
			}
			if name != "" && name != "-" {
				return name
			}
		}
	}
	return goName
}

func (e *fileExtractor) extractValueSpec(vs *ast.ValueSpec, kind string, doc *ast.CommentGroup) {
	for i, name := range vs.Names {
		pos := e.fset.Position(name.Pos())
		qualifiedName := e.pkgPath + "." + name.Name

		sig := ""
		if i < len(vs.Values) {
			sig = valueString(vs.Values[i])
		} else if vs.Type != nil {
			sig = exprString(vs.Type)
		}

		e.result.Symbols = append(e.result.Symbols, Symbol{
			QualifiedName: qualifiedName,
			Name:          name.Name,
			Kind:          kind,
			Signature:     sig,
			Doc:           docString(doc),
			Pos: Position{
				File: pos.Filename,
				Line: pos.Line,
			},
			Exported: name.IsExported(),
			IsTest:   e.isTest,
		})
	}
}

func (e *fileExtractor) extractImports() {
	for _, imp := range e.astFile.Imports {
		path := strings.Trim(imp.Path.Value, "\"")
		pos := e.fset.Position(imp.Pos())
		e.result.Edges = append(e.result.Edges, Edge{
			FromRef:  e.pkgPath,
			ToRef:    path,
			EdgeType: "imports",
			Pos: Position{
				File: pos.Filename,
				Line: pos.Line,
			},
		})
	}
}

func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeString(t.X)
	case *ast.IndexExpr:
		return typeString(t.X)
	default:
		return ""
	}
}

func signatureString(ft *ast.FuncType) string {
	parts := []string{"func"}
	if ft.Params != nil {
		params := make([]string, 0, ft.Params.NumFields())
		for _, f := range ft.Params.List {
			params = append(params, fieldListString(f))
		}
		parts = append(parts, "("+strings.Join(params, ", ")+")")
	}
	if ft.Results != nil && ft.Results.NumFields() > 0 {
		results := make([]string, 0, ft.Results.NumFields())
		for _, f := range ft.Results.List {
			results = append(results, fieldListString(f))
		}
		if len(results) == 1 {
			parts = append(parts, results[0])
		} else {
			parts = append(parts, "("+strings.Join(results, ", ")+")")
		}
	}
	return strings.Join(parts, " ")
}

func fieldListString(f *ast.Field) string {
	if len(f.Names) > 0 {
		names := make([]string, len(f.Names))
		for i, n := range f.Names {
			names[i] = n.Name
		}
		return strings.Join(names, ", ") + " " + exprString(f.Type)
	}
	return exprString(f.Type)
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(e.Elt)
	case *ast.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	case *ast.ChanType:
		return "chan " + exprString(e.Value)
	case *ast.FuncType:
		return "func(...)"
	case *ast.Ellipsis:
		return "..." + exprString(e.Elt)
	default:
		return "?"
	}
}

func valueString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Value
	case *ast.Ident:
		return e.Name
	case *ast.BinaryExpr:
		return valueString(e.X) + " " + e.Op.String() + " " + valueString(e.Y)
	case *ast.UnaryExpr:
		return e.Op.String() + valueString(e.X)
	case *ast.ParenExpr:
		return "(" + valueString(e.X) + ")"
	case *ast.SelectorExpr:
		return valueString(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		return valueString(e.Fun) + "(...)"
	case *ast.FuncLit:
		return "func(...)"
	case *ast.CompositeLit:
		return exprString(e.Type) + "{...}"
	default:
		return "?"
	}
}

func typeSignatureString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StructType:
		if t.Fields == nil || len(t.Fields.List) == 0 {
			return "struct{}"
		}
		fields := make([]string, 0, len(t.Fields.List))
		for _, f := range t.Fields.List {
			fields = append(fields, fieldListString(f))
		}
		return "struct { " + strings.Join(fields, "; ") + " }"
	case *ast.InterfaceType:
		return "interface"
	default:
		return exprString(expr)
	}
}

func docString(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	return doc.Text()
}

func computeComplexity(fn *ast.FuncDecl) int {
	if fn.Body == nil {
		return 1
	}
	c := 1
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			c++
		case *ast.ForStmt, *ast.RangeStmt:
			c++
		case *ast.CaseClause:
			c++
		case *ast.BinaryExpr:
			if node.Op == token.LAND || node.Op == token.LOR {
				c++
			}
		}
		return true
	})
	return c
}
