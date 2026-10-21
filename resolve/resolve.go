package resolve

import (
	"codemap/extract"
	"codemap/parse"
	"context"
	"errors"
	"fmt"
	"go/ast"
	goimporter "go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ResolvedSymbol struct {
	Symbol extract.Symbol
}

type ResolvedEdge struct {
	Edge extract.Edge
}

type Result struct {
	Packages    []parse.PackageInfo
	Symbols     []ResolvedSymbol
	Edges       []ResolvedEdge
	Warnings    []string
	Diagnostics []Diagnostic
}

// Diagnostic is one type-check or parse error captured during indexing.
type Diagnostic struct {
	File    string
	Message string
	Line    int
	Col     int
}

// parseContext precomputes the file-indexed maps shared across package
// processing in a single pass over the parse result, replacing per-package
// full-set scans (O(F·P) and pointer-equality scans) with O(F) map lookups.
type parseContext struct {
	fset      *token.FileSet
	files     map[string]*ast.File   // file path -> AST
	fileToDir map[string]string      // file path -> owning package dir
	pkgFiles  map[string][]*ast.File // package import path -> AST files
	pathByAST map[*ast.File]string   // AST pointer -> file path
	generated map[string]bool        // file path -> generated code
	pkgPaths  map[string]bool        // package import path -> indexed
}

func newParseContext(pr *parse.Result) *parseContext {
	ctx := &parseContext{
		fset:      pr.Fset,
		files:     pr.Files,
		fileToDir: make(map[string]string),
		pkgFiles:  make(map[string][]*ast.File),
		pathByAST: make(map[*ast.File]string, len(pr.Files)),
		generated: pr.GeneratedFiles,
		pkgPaths:  make(map[string]bool, len(pr.Packages)),
	}
	for path, af := range pr.Files {
		ctx.pathByAST[af] = path
	}
	for _, pkg := range pr.Packages {
		ctx.pkgPaths[pkg.ImportPath] = true
		for _, f := range pkg.Files {
			if _, ok := pr.Files[f]; !ok {
				continue
			}
			ctx.fileToDir[f] = pkg.Dir
			ctx.pkgFiles[pkg.ImportPath] = append(ctx.pkgFiles[pkg.ImportPath], pr.Files[f])
		}
	}
	return ctx
}

func Run(parseResult *parse.Result) *Result {
	result := &Result{
		Packages: parseResult.Packages,
	}

	// De-duplicate warnings across the whole run so the same concrete type
	// error never appears more than once.
	seen := make(map[string]bool)
	warn := func(msg string) {
		if seen[msg] {
			return
		}
		seen[msg] = true
		result.Warnings = append(result.Warnings, msg)
	}

	imp := newImporter(parseResult)
	imp.exports = loadExportIndex(parseResult)
	imp.onError = func(pkgPath string, err error) {
		// Attribute the concrete type error to the package being checked, so the
		// warning stream is identical no matter which pass reached the package.
		warn(fmt.Sprintf("%s: %v", pkgPath, err))
		// Unpositioned errors and indexer-side importer failures (there is no
		// compile problem blamed on the file) stay in Warnings only.
		if d := typedDiagnostic(err); d.File != "" && !isImporterNoise(d.Message) {
			result.Diagnostics = append(result.Diagnostics, d)
		}
	}

	ctx := newParseContext(parseResult)
	collectParseDiagnostics(parseResult, result)

	var typePackages []*types.Package
	for _, pkgInfo := range parseResult.Packages {
		if typePkg := processPackage(imp, ctx, pkgInfo, result); typePkg != nil {
			typePackages = append(typePackages, typePkg)
		}
	}

	resolveCrossPackageSatisfaction(typePackages, ctx.pkgPaths, result)

	sort.Slice(result.Symbols, func(i, j int) bool {
		return result.Symbols[i].Symbol.QualifiedName < result.Symbols[j].Symbol.QualifiedName
	})

	return result
}

// collectParseDiagnostics folds files that failed to parse (and package-load
// errors) into the diagnostics list so "is this file broken" covers syntax
// errors, not just type errors.
func collectParseDiagnostics(parseResult *parse.Result, result *Result) {
	for _, perr := range parseResult.Errors {
		msg := perr.Err
		if parsed, ok := parseParseError(perr.File, msg); ok {
			result.Diagnostics = append(result.Diagnostics, parsed)
			continue
		}
		result.Diagnostics = append(result.Diagnostics, Diagnostic{File: perr.File, Message: msg})
	}
}

// parseParseError splits a go/parser error string of the form
// "<file>:<line>:<col>" into a positioned diagnostic. Best effort: unparseable
// shapes fall back to the unpositioned message.
func parseParseError(file, msg string) (Diagnostic, bool) {
	var line, col int
	n, _ := fmt.Sscanf(msg, "%d:%d:", &line, &col)
	if n < 2 {
		return Diagnostic{}, false
	}
	return Diagnostic{File: file, Line: line, Col: col, Message: strings.TrimPrefix(msg, fmt.Sprintf("%d:%d:", line, col))}, true
}

// isImporterNoise reports whether a type error is an "indexer could not load
// external module" message rather than a problem in the indexed source.
func isImporterNoise(msg string) bool {
	return strings.HasPrefix(msg, "could not import")
}

// typedDiagnostic extracts file/line/col when the error is a types.Error and
// degrades gracefully to an unpositioned diagnostic otherwise.
func typedDiagnostic(err error) Diagnostic {
	var terr types.Error
	if !errors.As(err, &terr) {
		return Diagnostic{Message: err.Error()}
	}
	pos := terr.Fset.Position(terr.Pos)
	return Diagnostic{File: pos.Filename, Line: pos.Line, Col: pos.Column, Message: terr.Msg}
}

// namedTypes keys each package-level type by the name the index will store it
// under. Unexported types are included: they are real implementers and dropping
// them hid whole implementations from interface_impls.
//
// Aliases resolve to the type they alias. When that type is defined in another
// indexed package, the alias collapses onto the definition site so one
// implementer never yields two satisfies edges. When the target is outside the
// index (e.g. an alias of net/http.Handler), the alias itself is the only
// indexed symbol, so it keeps its own name instead of pointing at nothing.
func namedTypes(pkg *types.Package, indexed map[string]bool) map[string]*types.Named {
	result := make(map[string]*types.Named)
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if _, ok := obj.(*types.TypeName); !ok {
			continue
		}
		named := underlyingNamed(obj.Type())
		if named == nil || named.Obj() == nil {
			continue
		}
		key := pkg.Path() + "." + name
		if defPkg := named.Obj().Pkg(); defPkg == nil || defPkg == pkg || indexed[defPkg.Path()] {
			key = canonicalName(named)
		}
		result[key] = named
	}
	return result
}

// canonicalName is the definition-site identity of a named type. Aliases
// resolve to the original, so every alias of one type shares this key.
func canonicalName(named *types.Named) string {
	obj := named.Obj()
	if obj.Pkg() == nil {
		return obj.Name()
	}
	return obj.Pkg().Path() + "." + obj.Name()
}

func isInterfaceNamed(named *types.Named) bool {
	_, ok := named.Underlying().(*types.Interface)
	return ok
}

// underlyingNamed unwraps *types.Alias (Go 1.22+) to the underlying *types.Named.
// Returns nil if the type is not a Named or Alias-wrapping-Named.
func underlyingNamed(t types.Type) *types.Named {
	unaliased := types.Unalias(t)
	named, _ := unaliased.(*types.Named)
	return named
}

func resolveCrossPackageSatisfaction(typePackages []*types.Package, indexed map[string]bool, result *Result) {
	type pkgTypes struct {
		pkg    *types.Package
		types  map[string]*types.Named
		ifaces map[string]*types.Named
	}

	pkgs := make([]pkgTypes, 0, len(typePackages))
	for _, pkg := range typePackages {
		constructors, ifaces := classifyTypes(pkg, indexed)
		pkgs = append(pkgs, pkgTypes{pkg: pkg, types: constructors, ifaces: ifaces})
	}

	for _, pt := range pkgs {
		for _, other := range pkgs {
			if pt.pkg == other.pkg {
				continue
			}
			findCrossPackageSatisfies(pt.types, other.ifaces, result)
		}
	}
}

func classifyTypes(pkg *types.Package, indexed map[string]bool) (constructors, ifaces map[string]*types.Named) {
	constructors = make(map[string]*types.Named)
	ifaces = make(map[string]*types.Named)
	for key, named := range namedTypes(pkg, indexed) {
		if isInterfaceNamed(named) {
			ifaces[key] = named
		} else {
			constructors[key] = named
		}
	}
	return constructors, ifaces
}

// findCrossPackageSatisfies emits one edge per (implementer, interface) pair.
// Both maps are keyed by the symbol name the edge should reference, so aliases
// of the same type are visited once and the edge is reported under the
// definition site.
func findCrossPackageSatisfies(ctors, ifaces map[string]*types.Named, result *Result) {
	for ctorName, ctorType := range ctors {
		for ifaceName, ifaceType := range ifaces {
			if satisfiesInterface(ctorType, ifaceType) {
				result.Edges = append(result.Edges, ResolvedEdge{
					Edge: extract.Edge{
						FromRef:  ctorName,
						ToRef:    ifaceName,
						EdgeType: "satisfies",
						Pos:      extract.Position{File: "", Line: 0},
					},
				})
			}
		}
	}
}

// satisfiesInterface reports whether ctorType implements ifaceType.
//
// It refuses to answer when either side depends on a type that failed to
// resolve. go/types treats such types as vacuously satisfying everything: an
// implementer that embeds an unresolvable type gets an empty method set, and an
// interface that embeds one reports Empty(). Both make Implements return true
// for any candidate, which is how an unimportable gin.ResponseWriter turned a
// compression writer into an "implementation" of an unrelated API-key service.
func satisfiesInterface(ctorType, ifaceType *types.Named) bool {
	if hasUnresolvedType(ctorType) || hasUnresolvedType(ifaceType) {
		return false
	}
	iface := ifaceType.Underlying().(*types.Interface)
	return types.Implements(ctorType, iface) ||
		types.Implements(types.NewPointer(ctorType), iface)
}

// hasUnresolvedType reports whether t's structure contains a type the importer
// could not load.
func hasUnresolvedType(t types.Type) bool {
	return hasUnresolvedTypeSeen(t, make(map[types.Type]bool))
}

// hasUnresolvedTypeSeen guards recursion and mutually recursive named types.
func hasUnresolvedTypeSeen(t types.Type, visited map[types.Type]bool) bool {
	if t == nil || visited[t] {
		return false
	}
	visited[t] = true

	switch u := t.(type) {
	case *types.Basic:
		return u.Kind() == types.Invalid
	case *types.Named:
		return hasUnresolvedTypeSeen(u.Underlying(), visited)
	case *types.Alias:
		return hasUnresolvedTypeSeen(types.Unalias(u), visited)
	case *types.Pointer:
		return hasUnresolvedTypeSeen(u.Elem(), visited)
	case *types.Slice:
		return hasUnresolvedTypeSeen(u.Elem(), visited)
	case *types.Array:
		return hasUnresolvedTypeSeen(u.Elem(), visited)
	case *types.Chan:
		return hasUnresolvedTypeSeen(u.Elem(), visited)
	case *types.Map:
		return hasUnresolvedTypeSeen(u.Key(), visited) || hasUnresolvedTypeSeen(u.Elem(), visited)
	default:
		return compositeHasUnresolved(t, visited)
	}
}

func compositeHasUnresolved(t types.Type, visited map[types.Type]bool) bool {
	switch u := t.(type) {
	case *types.Struct:
		return embeddedFieldUnresolved(u, visited)
	case *types.Interface:
		return interfaceUnresolved(u, visited)
	case *types.Signature:
		return signatureUnresolved(u, visited)
	case *types.Tuple:
		return anyVarUnresolved(u, visited)
	}
	return false
}

// embeddedFieldUnresolved checks only embedded fields: they alone contribute to
// the method set, so only they can make Implements lie. A named field of an
// unresolved type is harmless.
func embeddedFieldUnresolved(st *types.Struct, visited map[types.Type]bool) bool {
	for f := range st.Fields() {
		if f.Embedded() && hasUnresolvedTypeSeen(f.Type(), visited) {
			return true
		}
	}
	return false
}

func interfaceUnresolved(iface *types.Interface, visited map[types.Type]bool) bool {
	for m := range iface.Methods() {
		if hasUnresolvedTypeSeen(m.Type(), visited) {
			return true
		}
	}
	for e := range iface.EmbeddedTypes() {
		if hasUnresolvedTypeSeen(e, visited) {
			return true
		}
	}
	return false
}

func signatureUnresolved(sig *types.Signature, visited map[types.Type]bool) bool {
	if anyVarUnresolved(sig.Params(), visited) || anyVarUnresolved(sig.Results(), visited) {
		return true
	}
	recv := sig.Recv()
	return recv != nil && hasUnresolvedTypeSeen(recv.Type(), visited)
}

func anyVarUnresolved(tup *types.Tuple, visited map[types.Type]bool) bool {
	if tup == nil {
		return false
	}
	for v := range tup.Variables() {
		if hasUnresolvedTypeSeen(v.Type(), visited) {
			return true
		}
	}
	return false
}

func processPackage(imp *importer, ctx *parseContext, pkgInfo parse.PackageInfo, result *Result) *types.Package {
	files := ctx.pkgFiles[pkgInfo.ImportPath]
	if len(files) == 0 {
		return nil
	}

	typePkg, info, err := imp.packageFor(pkgInfo.ImportPath)
	if err != nil {
		// The package could not be checked (import cycle, or a nil result).
		// Keep going: symbols and syntactic edges are still worth indexing.
		if imp.onError != nil {
			imp.onError(pkgInfo.ImportPath, err)
		}
	}
	if info == nil {
		// No type information is available, so type-based edges are skipped but
		// extraction still runs. This also covers a second pass over the same
		// package, whose Info the first pass already consumed.
		info = newTypeInfo()
	}

	fileMap := make(map[string]*ast.File, len(files))
	for _, f := range files {
		if path, ok := ctx.pathByAST[f]; ok {
			fileMap[path] = f
		}
	}
	extResult := extract.Run(pkgInfo.ImportPath, fileMap, ctx.fset, pkgInfo.IsTest, ctx.generated)

	for _, sym := range extResult.Symbols {
		result.Symbols = append(result.Symbols, ResolvedSymbol{Symbol: sym})
	}

	for _, edge := range extResult.Edges {
		if edge.EdgeType == "calls_syntactic" || edge.EdgeType == "references_syntactic" {
			continue
		}
		result.Edges = append(result.Edges, ResolvedEdge{Edge: edge})
	}

	resolveCallSites(fileMap, ctx.fset, info, pkgInfo.ImportPath, result)
	resolveReferences(fileMap, ctx.fset, info, pkgInfo.ImportPath, result)

	// The type information is only needed for this package's own edge
	// resolution; dropping it keeps peak memory bounded by packages in flight.
	imp.mu.Lock()
	delete(imp.info, pkgInfo.ImportPath)
	imp.mu.Unlock()

	if typePkg != nil {
		resolveInterfaceSatisfaction(typePkg, ctx.pkgPaths, result)
		resolveStructEmbedding(typePkg, pkgInfo.ImportPath, result)
	}
	resolveCgoRefs(fileMap, ctx.fset, pkgInfo.ImportPath, result)

	return typePkg
}

func resolveCallSites(files map[string]*ast.File, fset *token.FileSet, info *types.Info, pkgPath string, result *Result) {
	for _, astFile := range files {
		var currentFunc string
		var fdBoundary token.Pos

		ast.Inspect(astFile, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Recv != nil && len(node.Recv.List) > 0 {
					currentFunc = pkgPath + "." + recvTypeString(node.Recv.List[0].Type) + "." + node.Name.Name
				} else {
					currentFunc = pkgPath + "." + node.Name.Name
				}
				fdBoundary = node.End()

			case *ast.GenDecl:
				if name := genDeclCurrentName(node, pkgPath, node.Pos() < fdBoundary); name != "" {
					currentFunc = name
				}

			case *ast.CallExpr:
				resolveCallExpr(node, currentFunc, fset, info, result)
			}
			return true
		})
	}
}

func resolveCallExpr(node *ast.CallExpr, currentFunc string, fset *token.FileSet, info *types.Info, result *Result) {
	if currentFunc == "" {
		return
	}

	var callee string
	switch fun := node.Fun.(type) {
	case *ast.Ident:
		if obj, ok := info.Uses[fun]; ok {
			callee = objectQualifiedName(obj)
		}
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[fun]; ok {
			callee = selectionQualifiedName(sel)
		} else if obj, ok := info.Uses[fun.Sel]; ok {
			callee = objectQualifiedName(obj)
		}
	}

	if callee != "" {
		pos := fset.Position(node.Pos())
		result.Edges = append(result.Edges, ResolvedEdge{
			Edge: extract.Edge{
				FromRef:  currentFunc,
				ToRef:    callee,
				EdgeType: "calls",
				Pos: extract.Position{
					File: pos.Filename,
					Line: pos.Line,
				},
			},
		})
	}
}

func isUsefulRef(obj types.Object) bool {
	if obj.Pkg() == nil {
		return false
	}
	switch obj.(type) {
	case *types.Builtin, *types.Nil:
		return false
	}
	return true
}

func isLocalVar(obj types.Object) bool {
	v, ok := obj.(*types.Var)
	if !ok {
		return false
	}
	if v.Parent() == nil {
		return false
	}
	if v.Parent() == v.Pkg().Scope() {
		return false
	}
	return true
}

func addReferenceEdge(from, to string, pos token.Position, result *Result) {
	result.Edges = append(result.Edges, ResolvedEdge{
		Edge: extract.Edge{
			FromRef:  from,
			ToRef:    to,
			EdgeType: "references",
			Pos:      extract.Position{File: pos.Filename, Line: pos.Line},
		},
	})
}

// selectorQualifier returns the ident on the left of a selector expression,
// unwrapping the parentheses gofmt leaves in cases like (T{}).M. A qualifier
// that is itself a selector (pkg.T.M) is ignored: its own X is visited
// separately by the walk.
func selectorQualifier(node *ast.SelectorExpr) *ast.Ident {
	ident, _ := ast.Unparen(node.X).(*ast.Ident)
	return ident
}

func resolveSelectorRef(node *ast.SelectorExpr, currentDecl string, fset *token.FileSet, info *types.Info, result *Result) {
	if currentDecl == "" {
		return
	}
	// Method values (f := svc.Get), field accesses (svc.x), and method
	// expressions (T.M) are all present in info.Selections. The Sel ident
	// of these expressions is handled by resolveIdentRef, so skip here to
	// avoid double-emit. Callee selections (svc.Get()) are skipped by the
	// calleeNodes filter in resolveReferences and produce calls edges via
	// resolveCallExpr.
	//
	// Returning early still leaves the qualifier (sel.X) unvisited: the
	// selectors here are like `re.MatchString`, where X is the referenced
	// object and Sel is only a method on it. resolveIdentRef sees Sel, not X,
	// so without this the package-level var behind X is never linked.
	if _, ok := info.Selections[node]; ok {
		resolveIdentRef(selectorQualifier(node), currentDecl, fset, info, result)
		return
	}
	obj, ok := info.Uses[node.Sel]
	if !ok || !isUsefulRef(obj) {
		return
	}
	if _, ok := obj.(*types.Builtin); ok {
		return
	}
	toRef := objectQualifiedName(obj)
	if toRef != "" && toRef != currentDecl {
		addReferenceEdge(currentDecl, toRef, fset.Position(node.Pos()), result)
	}
}

func resolveIdentRef(node *ast.Ident, currentDecl string, fset *token.FileSet, info *types.Info, result *Result) {
	if node == nil || currentDecl == "" {
		return
	}
	obj, ok := info.Uses[node]
	if !ok || !isUsefulRef(obj) {
		return
	}
	if isLocalVar(obj) {
		return
	}
	toRef := objectQualifiedName(obj)
	if toRef != "" && toRef != currentDecl {
		addReferenceEdge(currentDecl, toRef, fset.Position(node.Pos()), result)
	}
}

func resolveReferences(files map[string]*ast.File, fset *token.FileSet, info *types.Info, pkgPath string, result *Result) {
	for _, astFile := range files {
		var currentDecl string
		var fdBoundary token.Pos

		// First pass: collect all nodes that are part of a call-expression's
		// callee (the function being called and all its descendants). These
		// nodes already produce `calls` edges via resolveCallSites, so they
		// must be skipped during the reference walk.
		calleeNodes := make(map[ast.Node]bool)
		ast.Inspect(astFile, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				markCallees(call.Fun, calleeNodes)
			}
			return true
		})

		// Second pass: walk the AST to emit reference edges.
		ast.Inspect(astFile, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				currentDecl = funcDeclName(node, pkgPath)
				fdBoundary = node.End()
			case *ast.GenDecl:
				if name := genDeclCurrentName(node, pkgPath, node.Pos() < fdBoundary); name != "" {
					currentDecl = name
				}
			case *ast.SelectorExpr:
				if !calleeNodes[node] {
					resolveSelectorRef(node, currentDecl, fset, info, result)
				}
			case *ast.Ident:
				if !calleeNodes[node] {
					resolveIdentRef(node, currentDecl, fset, info, result)
				}
			}
			return true
		})
	}
}

// markCallees records the nodes of a call's function expression that
// resolveReferences must skip, since the callee already yields a calls edge
// via resolveCallExpr. Only the method name and the exact nested call nodes
// are recorded. The qualifier of a method selector (re in `re.MatchString()`)
// is deliberately NOT recorded: it is a genuine reference to a package-level
// value, distinct from the method being called, and marking it would leave
// that value with no incoming edge at all.
func markCallees(fun ast.Node, calleeNodes map[ast.Node]bool) {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if _, isCall := f.X.(*ast.CallExpr); isCall {
			markCallees(f.X, calleeNodes)
		}
		calleeNodes[f] = true
		calleeNodes[f.Sel] = true
	case *ast.Ident:
		calleeNodes[f] = true
	case *ast.IndexExpr:
		// A generic instantiation f[T]: f still needs its calls edge, and T
		// is type-only, so descending into f covers it.
		markCallees(f.X, calleeNodes)
	case *ast.IndexListExpr:
		markCallees(f.X, calleeNodes)
	case *ast.ParenExpr:
		markCallees(f.X, calleeNodes)
	}
}

func funcDeclName(fd *ast.FuncDecl, pkgPath string) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		return pkgPath + "." + recvTypeString(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return pkgPath + "." + fd.Name.Name
}

// genDeclCurrentName returns the current declaration name from a GenDecl spec.
// For ValueSpec it returns the last name; for TypeSpec it returns the type name.
// Only package-level declarations count: a GenDecl inside a function body
// declares locals, whose names must never become an edge source. inFunc reports
// whether the walk has descended into a function body.
func genDeclCurrentName(node *ast.GenDecl, pkgPath string, inFunc bool) string {
	// inFunc is derived by the caller from a position comparison, not passed
	// down a stack, because ast.Inspect has no enter/leave callback.
	if inFunc {
		return ""
	}
	for _, spec := range node.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return pkgPath + "." + s.Names[len(s.Names)-1].Name
			}
		case *ast.TypeSpec:
			return pkgPath + "." + s.Name.Name
		}
	}
	return ""
}

func recvTypeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return recvTypeString(t.X)
	case *ast.IndexExpr:
		return recvTypeString(t.X)
	case *ast.IndexListExpr:
		return recvTypeString(t.X)
	default:
		return ""
	}
}

func objectQualifiedName(obj types.Object) string {
	if obj == nil {
		return ""
	}
	pkgPath := ""
	if obj.Pkg() != nil {
		pkgPath = obj.Pkg().Path()
	}

	if fn, ok := obj.(*types.Func); ok {
		return funcQualifiedName(fn, pkgPath)
	}

	return pkgPath + "." + obj.Name()
}

func selectionQualifiedName(sel *types.Selection) string {
	obj := sel.Obj()
	if obj == nil {
		return ""
	}

	if fn, ok := obj.(*types.Func); ok {
		pkgPath := ""
		if fn.Pkg() != nil {
			pkgPath = fn.Pkg().Path()
		}
		return funcQualifiedName(fn, pkgPath)
	}

	pkgPath := ""
	if obj.Pkg() != nil {
		pkgPath = obj.Pkg().Path()
	}
	return pkgPath + "." + obj.Name()
}

func funcQualifiedName(fn *types.Func, pkgPath string) string {
	sig := fn.Type().(*types.Signature)
	recv := sig.Recv()
	if recv == nil {
		return pkgPath + "." + fn.Name()
	}

	recvType := recv.Type()
	if ptr, ok := recvType.(*types.Pointer); ok {
		recvType = ptr.Elem()
	}
	named := underlyingNamed(recvType)
	if named == nil {
		return pkgPath + "." + fn.Name()
	}
	return pkgPath + "." + named.Obj().Name() + "." + fn.Name()
}

// resolveInterfaceSatisfaction records same-package implementation edges. Both
// sides are keyed by the stored symbol name, so an alias never produces a
// second edge for the same (implementer, interface) pair.
func resolveInterfaceSatisfaction(pkg *types.Package, indexed map[string]bool, result *Result) {
	constructors, ifaces := classifyTypes(pkg, indexed)
	findCrossPackageSatisfies(constructors, ifaces, result)
}

func resolveStructEmbedding(pkg *types.Package, pkgPath string, result *Result) {
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		named := underlyingNamed(obj.Type())
		if named == nil {
			continue
		}
		switch u := named.Underlying().(type) {
		case *types.Struct:
			collectStructEmbeds(pkgPath, name, u, result)
		case *types.Interface:
			collectInterfaceEmbeds(pkgPath, name, u, result)
		}
	}
}

func collectStructEmbeds(pkgPath, name string, st *types.Struct, result *Result) {
	for field := range st.Fields() {
		if !field.Embedded() {
			continue
		}
		fieldNamed := underlyingNamed(field.Type())
		if fieldNamed == nil {
			continue
		}
		embPkg := fieldNamed.Obj().Pkg()
		embPkgPath := ""
		if embPkg != nil {
			embPkgPath = embPkg.Path()
		}
		result.Edges = append(result.Edges, ResolvedEdge{
			Edge: extract.Edge{
				FromRef:  pkgPath + "." + name,
				ToRef:    embPkgPath + "." + fieldNamed.Obj().Name(),
				EdgeType: "embeds",
				Pos:      extract.Position{File: "", Line: 0},
			},
		})
	}
}

func collectInterfaceEmbeds(pkgPath, name string, iface *types.Interface, result *Result) {
	for embType := range iface.EmbeddedTypes() {
		embNamed, ok := embType.(*types.Named)
		if !ok {
			continue
		}
		embPkg := embNamed.Obj().Pkg()
		embPkgPath := ""
		if embPkg != nil {
			embPkgPath = embPkg.Path()
		}
		result.Edges = append(result.Edges, ResolvedEdge{
			Edge: extract.Edge{
				FromRef:  pkgPath + "." + name,
				ToRef:    embPkgPath + "." + embNamed.Obj().Name(),
				EdgeType: "embeds",
				Pos:      extract.Position{File: "", Line: 0},
			},
		})
	}
}

// importer resolves import paths to *types.Package. Project packages are
// type-checked from the source files collected by parse (memoized per import
// path), so cross-package calls and references resolve to real objects instead
// of being dropped as unresolvable imports. Any other import (standard library,
// third-party module) is loaded through the gc importer's compiled export data.
type importer struct {
	gc       types.Importer
	pkgFiles map[string][]*ast.File
	fset     *token.FileSet
	memo     map[string]*types.Package
	info     map[string]*types.Info

	inProgress map[string]bool

	// onError records concrete type-check errors for the package being checked.
	// Every check goes through it, whether triggered by an import or by the
	// top-level pass, so diagnostics and warnings do not depend on which path
	// reached the package first.
	onError func(pkgPath string, err error)

	// exports maps an import path to its compiled export-data file, resolved in
	// one `go list -export -deps` call before type-checking starts.
	exports map[string]string

	mu sync.Mutex
}

func newImporter(pr *parse.Result) *importer {
	imp := &importer{
		pkgFiles:   make(map[string][]*ast.File),
		fset:       pr.Fset,
		memo:       make(map[string]*types.Package),
		info:       make(map[string]*types.Info),
		inProgress: make(map[string]bool),
	}
	for _, pi := range pr.Packages {
		files := make([]*ast.File, 0, len(pi.Files))
		for _, f := range pi.Files {
			if af, ok := pr.Files[f]; ok {
				files = append(files, af)
			}
		}
		if len(files) > 0 {
			imp.pkgFiles[pi.ImportPath] = files
		}
	}
	return imp
}

func (i *importer) Import(path string) (*types.Package, error) {
	pkg, _, err := i.packageFor(path)
	return pkg, err
}

// newTypeInfo returns an empty types.Info. Callers that pass it to Check get it
// populated; callers that fall back to it read empty maps and simply produce no
// type-based edges.
func newTypeInfo() *types.Info {
	return &types.Info{
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Defs:       make(map[*ast.Ident]types.Object),
	}
}

// packageFor returns the single checked instance of a package and the type
// information gathered while checking it. Every consumer shares that instance,
// so types.Identical sees the same object identity across package boundaries.
func (i *importer) packageFor(path string) (*types.Package, *types.Info, error) {
	if path == "unsafe" {
		return types.Unsafe, nil, nil
	}

	i.mu.Lock()
	if pkg, ok := i.memo[path]; ok {
		info := i.info[path]
		i.mu.Unlock()
		return pkg, info, nil
	}
	if i.inProgress[path] {
		i.mu.Unlock()
		return nil, nil, fmt.Errorf("import cycle: %s", path)
	}
	files, isProject := i.pkgFiles[path]
	if isProject {
		i.inProgress[path] = true
	}
	i.mu.Unlock()

	if !isProject {
		pkg, err := i.importExternal(path)
		return pkg, nil, err
	}

	info := newTypeInfo()
	conf := &types.Config{
		Importer: i,
		Error: func(err error) {
			if i.onError != nil {
				i.onError(path, err)
			}
		},
	}
	pkg, _ := conf.Check(path, i.fset, files, info)

	i.mu.Lock()
	delete(i.inProgress, path)
	if pkg != nil {
		i.memo[path] = pkg
		i.info[path] = info
	}
	i.mu.Unlock()

	if pkg == nil {
		return nil, nil, fmt.Errorf("could not type-check package %s", path)
	}
	return pkg, info, nil
}

// importExternal loads compiled export data for external (module/stdlib)
// imports. The default gc importer cannot locate module-cache export data, so
// `go list -export` resolves the path first (also populating the build cache);
// the result is handed to the gc importer through a lookup function.
func (i *importer) importExternal(path string) (*types.Package, error) {
	i.mu.Lock()
	if i.gc == nil {
		i.gc = goimporter.ForCompiler(i.fset, "gc", i.exportLookup)
	}
	i.mu.Unlock()

	pkg, err := i.gc.Import(path)
	if err != nil {
		return nil, err
	}

	i.mu.Lock()
	i.memo[path] = pkg
	i.mu.Unlock()
	return pkg, nil
}

// exportLookup opens the compiled export data for an external import. The path
// is normally resolved up front by loadExportIndex; test-only dependencies are
// absent from `go list -deps ./...`, so those fall back to a single-path lookup.
func (i *importer) exportLookup(path string) (io.ReadCloser, error) {
	if exportFile := i.exports[path]; exportFile != "" {
		return os.Open(exportFile)
	}
	return exportLookupOne(path)
}

// exportLookupOne resolves one import's export data with its own `go list`
// call. Only reached for imports the batched lookup could not see.
func exportLookupOne(path string) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-export", "-f", "{{.Export}}", path).Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export %s: %w", path, err)
	}
	exportFile := strings.TrimSpace(string(out))
	if exportFile == "" {
		return nil, fmt.Errorf("no export data for %s", path)
	}
	return os.Open(exportFile)
}

// loadExportIndex resolves every dependency's compiled export data with a single
// `go list -export -deps` call. Resolving per import instead spawns one `go`
// process per dependency, which dominates index time on repos with many
// third-party imports.
func loadExportIndex(pr *parse.Result) map[string]string {
	dir := moduleRoot(pr)
	if dir == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-export", "-deps", "-e",
		"-f", "{{.ImportPath}} {{.Export}}", "./...").Output()
	if err != nil {
		return nil
	}
	exports := make(map[string]string)
	for line := range strings.SplitSeq(string(out), "\n") {
		path, exportFile, ok := strings.Cut(line, " ")
		if !ok || exportFile == "" {
			continue
		}
		exports[path] = exportFile
	}
	return exports
}

// moduleRoot returns the nearest directory holding a go.mod above the first
// parsed package, so `go list ./...` covers the whole module rather than one
// package's dependency subtree.
func moduleRoot(pr *parse.Result) string {
	for _, p := range pr.Packages {
		if p.Dir == "" {
			continue
		}
		dir := p.Dir
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

// resolveCgoRefs detects cgo files (those importing "C"), emits a per-file
// warning, maps C.foo selector expressions to cgo/foo pseudo-symbols with
// references edges, and inserts cgo/* symbol rows so show/search can explain
// them.
func resolveCgoRefs(files map[string]*ast.File, fset *token.FileSet, pkgPath string, result *Result) {
	cgoNames := make(map[string]bool)

	for filePath, astFile := range files {
		if !isCgoFile(astFile) {
			continue
		}
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("cgo: file %s uses cgo; C.foo references are mapped to cgo/foo pseudo-symbols", filePath))
		walkCgoFile(astFile, fset, pkgPath, cgoNames, result)
	}

	for name := range cgoNames {
		result.Symbols = append(result.Symbols, ResolvedSymbol{
			Symbol: extract.Symbol{
				QualifiedName: name,
				Name:          name,
				Kind:          "function",
				Exported:      true,
			},
		})
	}
}

func walkCgoFile(astFile *ast.File, fset *token.FileSet, pkgPath string, cgoNames map[string]bool, result *Result) {
	var currentDecl string
	var fdBoundary token.Pos
	ast.Inspect(astFile, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			currentDecl = funcDeclName(node, pkgPath)
			fdBoundary = node.End()
		case *ast.GenDecl:
			if name := genDeclCurrentName(node, pkgPath, node.Pos() < fdBoundary); name != "" {
				currentDecl = name
			}
		case *ast.SelectorExpr:
			ident, ok := node.X.(*ast.Ident)
			if !ok || ident.Name != "C" {
				return true
			}
			if currentDecl == "" {
				currentDecl = pkgPath
			}
			cgoName := "cgo/" + node.Sel.Name
			cgoNames[cgoName] = true
			addReferenceEdge(currentDecl, cgoName, fset.Position(node.Pos()), result)
		}
		return true
	})
}

// isCgoFile reports whether an AST file imports the "C" pseudo-package.
func isCgoFile(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path.Value == `"C"` {
			return true
		}
	}
	return false
}
