package resolve

import (
	"codemap/extract"
	"codemap/parse"
	"fmt"
	"go/ast"
	goimporter "go/importer"
	"go/token"
	"go/types"
	"sort"
	"sync"
)

type ResolvedSymbol struct {
	Symbol extract.Symbol
}

type ResolvedEdge struct {
	Edge extract.Edge
}

type Result struct {
	Packages []parse.PackageInfo
	Symbols  []ResolvedSymbol
	Edges    []ResolvedEdge
	Warnings []string
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
}

func newParseContext(pr *parse.Result) *parseContext {
	ctx := &parseContext{
		fset:      pr.Fset,
		files:     pr.Files,
		fileToDir: make(map[string]string),
		pkgFiles:  make(map[string][]*ast.File),
		pathByAST: make(map[*ast.File]string, len(pr.Files)),
	}
	for path, af := range pr.Files {
		ctx.pathByAST[af] = path
	}
	for _, pkg := range pr.Packages {
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
	imp.warn = func(pkgPath string, err error) {
		// Attribute the concrete dependency error to the package being checked
		// instead of collapsing it into a generic "could not type-check".
		warn(fmt.Sprintf("%s: %v", pkgPath, err))
	}

	conf := &types.Config{
		Importer: imp,
		Error: func(err error) {
			warn(err.Error())
		},
	}

	ctx := newParseContext(parseResult)

	var typePackages []*types.Package
	for _, pkgInfo := range parseResult.Packages {
		typePkg := processPackage(conf, ctx, pkgInfo, result, warn)
		if typePkg != nil {
			typePackages = append(typePackages, typePkg)
		}
	}

	resolveCrossPackageSatisfaction(typePackages, result)

	sort.Slice(result.Symbols, func(i, j int) bool {
		return result.Symbols[i].Symbol.QualifiedName < result.Symbols[j].Symbol.QualifiedName
	})

	return result
}

func getExportedNamedTypes(pkg *types.Package) map[string]*types.Named {
	result := make(map[string]*types.Named)
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}
		named, ok := obj.Type().(*types.Named)
		if !ok {
			continue
		}
		result[name] = named
	}
	return result
}

func isInterfaceNamed(named *types.Named) bool {
	_, ok := named.Underlying().(*types.Interface)
	return ok
}

func resolveCrossPackageSatisfaction(typePackages []*types.Package, result *Result) {
	type pkgTypes struct {
		pkg    *types.Package
		types  map[string]*types.Named
		ifaces map[string]*types.Named
	}

	pkgs := make([]pkgTypes, 0, len(typePackages))
	for _, pkg := range typePackages {
		constructors, ifaces := classifyTypes(pkg)
		pkgs = append(pkgs, pkgTypes{pkg: pkg, types: constructors, ifaces: ifaces})
	}

	for _, pt := range pkgs {
		for _, other := range pkgs {
			if pt.pkg == other.pkg {
				continue
			}
			findCrossPackageSatisfies(pt.pkg.Path(), pt.types, other.pkg.Path(), other.ifaces, result)
		}
	}
}

func classifyTypes(pkg *types.Package) (constructors, ifaces map[string]*types.Named) {
	namedTypes := getExportedNamedTypes(pkg)
	constructors = make(map[string]*types.Named)
	ifaces = make(map[string]*types.Named)
	for name, named := range namedTypes {
		if isInterfaceNamed(named) {
			ifaces[name] = named
		} else {
			constructors[name] = named
		}
	}
	return constructors, ifaces
}

func findCrossPackageSatisfies(fromPkg string, ctors map[string]*types.Named, toPkg string, ifaces map[string]*types.Named, result *Result) {
	for ctorName, ctorType := range ctors {
		for ifaceName, ifaceType := range ifaces {
			if satisfiesInterface(ctorType, ifaceType) {
				result.Edges = append(result.Edges, ResolvedEdge{
					Edge: extract.Edge{
						FromRef:  fromPkg + "." + ctorName,
						ToRef:    toPkg + "." + ifaceName,
						EdgeType: "satisfies",
						Pos:      extract.Position{File: "", Line: 0},
					},
				})
			}
		}
	}
}

func satisfiesInterface(ctorType, ifaceType *types.Named) bool {
	iface := ifaceType.Underlying().(*types.Interface)
	return types.Implements(ctorType, iface) ||
		types.Implements(types.NewPointer(ctorType), iface)
}

func processPackage(conf *types.Config, ctx *parseContext, pkgInfo parse.PackageInfo, result *Result, warn func(string)) *types.Package {
	files := ctx.pkgFiles[pkgInfo.ImportPath]
	if len(files) == 0 {
		return nil
	}

	info := &types.Info{
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Defs:       make(map[*ast.Ident]types.Object),
	}

	typePkg, err := conf.Check(pkgInfo.ImportPath, ctx.fset, files, info)
	if err != nil {
		warn(err.Error())
	}

	fileMap := make(map[string]*ast.File, len(files))
	for _, f := range files {
		if path, ok := ctx.pathByAST[f]; ok {
			fileMap[path] = f
		}
	}
	extResult := extract.Run(pkgInfo.ImportPath, fileMap, ctx.fset, pkgInfo.IsTest)

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
	resolveInterfaceSatisfaction(typePkg, pkgInfo.ImportPath, result)
	resolveStructEmbedding(typePkg, pkgInfo.ImportPath, result)

	return typePkg
}

func resolveCallSites(files map[string]*ast.File, fset *token.FileSet, info *types.Info, pkgPath string, result *Result) {
	for _, astFile := range files {
		var currentFunc string

		ast.Inspect(astFile, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Recv != nil && len(node.Recv.List) > 0 {
					currentFunc = pkgPath + "." + recvTypeString(node.Recv.List[0].Type) + "." + node.Name.Name
				} else {
					currentFunc = pkgPath + "." + node.Name.Name
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

func resolveSelectorRef(node *ast.SelectorExpr, currentFunc string, fset *token.FileSet, info *types.Info, result *Result) {
	if currentFunc == "" {
		return
	}
	if _, ok := info.Selections[node]; ok {
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
	if toRef != "" && toRef != currentFunc {
		addReferenceEdge(currentFunc, toRef, fset.Position(node.Pos()), result)
	}
}

func resolveIdentRef(node *ast.Ident, currentFunc string, fset *token.FileSet, info *types.Info, result *Result) {
	if currentFunc == "" {
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
	if toRef != "" && toRef != currentFunc {
		addReferenceEdge(currentFunc, toRef, fset.Position(node.Pos()), result)
	}
}

func resolveReferences(files map[string]*ast.File, fset *token.FileSet, info *types.Info, pkgPath string, result *Result) {
	for _, astFile := range files {
		var currentFunc string

		// Nodes that are the callee of a call expression already produce a
		// `calls` edge (resolveCallSites runs first). Emitting a `references`
		// edge for the same expression would double-count the call site in every
		// edge-based metric, so callee identifiers and selector expressions are
		// skipped here. Method values (`f := obj.Get`) and non-call selectors
		// (field access, type references) are not call callees and still emit
		// `references`. A single pre-order walk suffices because the CallExpr is
		// visited before its callee children, so the callee set is complete
		// when the children are reached — the former separate collection pass
		// is gone (one walk per reference-exposed declaration instead of two).
		calleeNodes := make(map[ast.Node]bool)
		ast.Inspect(astFile, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				currentFunc = funcDeclName(node, pkgPath)
			case *ast.CallExpr:
				switch fun := node.Fun.(type) {
				case *ast.SelectorExpr:
					calleeNodes[fun] = true
					calleeNodes[fun.Sel] = true
				case *ast.Ident:
					calleeNodes[fun] = true
				}
			case *ast.SelectorExpr:
				if !calleeNodes[node] {
					resolveSelectorRef(node, currentFunc, fset, info, result)
				}
			case *ast.Ident:
				if !calleeNodes[node] {
					resolveIdentRef(node, currentFunc, fset, info, result)
				}
			}
			return true
		})
	}
}

func funcDeclName(fd *ast.FuncDecl, pkgPath string) string {
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		return pkgPath + "." + recvTypeString(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return pkgPath + "." + fd.Name.Name
}

func recvTypeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return recvTypeString(t.X)
	case *ast.IndexExpr:
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
	named, ok := recvType.(*types.Named)
	if !ok {
		return pkgPath + "." + fn.Name()
	}
	return pkgPath + "." + named.Obj().Name() + "." + fn.Name()
}

func resolveInterfaceSatisfaction(pkg *types.Package, pkgPath string, result *Result) {
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		named, ok := exportedNonInterfaceNamed(scope, name)
		if !ok {
			continue
		}
		findInPackageInterfaces(scope, pkgPath, name, named, result)
	}
}

func exportedNonInterfaceNamed(scope *types.Scope, name string) (*types.Named, bool) {
	obj := scope.Lookup(name)
	if !obj.Exported() {
		return nil, false
	}
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, false
	}
	if isInterfaceNamed(named) {
		return nil, false
	}
	return named, true
}

func exportedInterfaceNamed(scope *types.Scope, name string) (*types.Named, bool) {
	obj := scope.Lookup(name)
	if !obj.Exported() {
		return nil, false
	}
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, false
	}
	if !isInterfaceNamed(named) {
		return nil, false
	}
	return named, true
}

func findInPackageInterfaces(scope *types.Scope, pkgPath, name string, named *types.Named, result *Result) {
	for _, otherName := range scope.Names() {
		if otherName == name {
			continue
		}
		otherNamed, ok := exportedInterfaceNamed(scope, otherName)
		if !ok {
			continue
		}
		if satisfiesInterface(named, otherNamed) {
			result.Edges = append(result.Edges, ResolvedEdge{
				Edge: extract.Edge{
					FromRef:  pkgPath + "." + name,
					ToRef:    pkgPath + "." + otherName,
					EdgeType: "satisfies",
					Pos:      extract.Position{File: "", Line: 0},
				},
			})
		}
	}
}

func resolveStructEmbedding(pkg *types.Package, pkgPath string, result *Result) {
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		named, ok := obj.Type().(*types.Named)
		if !ok {
			continue
		}
		st, ok := named.Underlying().(*types.Struct)
		if !ok {
			continue
		}

		for field := range st.Fields() {
			if !field.Embedded() {
				continue
			}
			fieldNamed, ok := field.Type().(*types.Named)
			if !ok {
				continue
			}
			embeddedPkg := fieldNamed.Obj().Pkg()
			embeddedPkgPath := ""
			if embeddedPkg != nil {
				embeddedPkgPath = embeddedPkg.Path()
			}
			result.Edges = append(result.Edges, ResolvedEdge{
				Edge: extract.Edge{
					FromRef:  pkgPath + "." + name,
					ToRef:    embeddedPkgPath + "." + fieldNamed.Obj().Name(),
					EdgeType: "embeds",
					Pos:      extract.Position{File: "", Line: 0},
				},
			})
		}
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

	inProgress map[string]bool

	// warn records concrete type-check errors for a project dependency, keyed
	// by the import path being checked. Nil means "drop" (no-op), matching the
	// pre-diagnostic behavior.
	warn func(pkgPath string, err error)

	mu sync.Mutex
}

func newImporter(pr *parse.Result) *importer {
	imp := &importer{
		pkgFiles:   make(map[string][]*ast.File),
		fset:       pr.Fset,
		memo:       make(map[string]*types.Package),
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
	if path == "unsafe" {
		return types.Unsafe, nil
	}

	i.mu.Lock()
	if pkg, ok := i.memo[path]; ok {
		i.mu.Unlock()
		return pkg, nil
	}
	if i.inProgress[path] {
		i.mu.Unlock()
		return nil, fmt.Errorf("import cycle: %s", path)
	}
	files, isProject := i.pkgFiles[path]
	if isProject {
		i.inProgress[path] = true
	}
	i.mu.Unlock()

	if !isProject {
		return i.importExternal(path)
	}

	conf := &types.Config{
		Importer: i,
		Error: func(err error) {
			if i.warn != nil {
				i.warn(path, err)
			}
		},
	}
	pkg, _ := conf.Check(path, i.fset, files, nil)

	i.mu.Lock()
	delete(i.inProgress, path)
	if pkg != nil {
		i.memo[path] = pkg
	}
	i.mu.Unlock()

	if pkg == nil {
		return nil, fmt.Errorf("could not type-check package %s", path)
	}
	return pkg, nil
}

func (i *importer) importExternal(path string) (*types.Package, error) {
	i.mu.Lock()
	if i.gc == nil {
		i.gc = goimporter.ForCompiler(i.fset, "gc", nil)
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
