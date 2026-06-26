package resolve

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"codemap/extract"
	"codemap/parse"
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

func Run(parseResult *parse.Result) *Result {
	result := &Result{
		Packages: parseResult.Packages,
	}

	conf := &types.Config{
		Importer: importer{},
		Error: func(err error) {
			result.Warnings = append(result.Warnings, err.Error())
		},
	}

	pkgFiles := groupFilesByPackage(parseResult)

	var typePackages []*types.Package
	for _, pkgInfo := range parseResult.Packages {
		typePkg := processPackage(conf, parseResult, pkgFiles, pkgInfo, result)
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

func processPackage(conf *types.Config, parseResult *parse.Result, pkgFiles map[string][]*ast.File, pkgInfo parse.PackageInfo, result *Result) *types.Package {
	files := pkgFiles[pkgInfo.Dir]
	if len(files) == 0 {
		return nil
	}

	info := &types.Info{
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Defs:       make(map[*ast.Ident]types.Object),
	}

	typePkg, err := conf.Check(pkgInfo.ImportPath, parseResult.Fset, files, info)
	if err != nil {
		result.Warnings = append(result.Warnings, err.Error())
	}

	fileMap := make(map[string]*ast.File)
	for _, f := range files {
		for filePath, astFile := range parseResult.Files {
			if astFile == f {
				fileMap[filePath] = astFile
				break
			}
		}
	}
	extResult := extract.Run(pkgInfo.ImportPath, fileMap, parseResult.Fset, pkgInfo.IsTest)

	for _, sym := range extResult.Symbols {
		result.Symbols = append(result.Symbols, ResolvedSymbol{Symbol: sym})
	}

	for _, edge := range extResult.Edges {
		if edge.EdgeType == "calls_syntactic" || edge.EdgeType == "references_syntactic" {
			continue
		}
		result.Edges = append(result.Edges, ResolvedEdge{Edge: edge})
	}

	resolveCallSites(fileMap, parseResult.Fset, info, pkgInfo.ImportPath, result)
	resolveReferences(fileMap, parseResult.Fset, info, pkgInfo.ImportPath, result)
	resolveInterfaceSatisfaction(typePkg, pkgInfo.ImportPath, result)
	resolveStructEmbedding(typePkg, pkgInfo.ImportPath, result)

	return typePkg
}

func groupFilesByPackage(pr *parse.Result) map[string][]*ast.File {
	files := make(map[string][]*ast.File)
	for filePath, astFile := range pr.Files {
		dir := ""
		for _, pkg := range pr.Packages {
			if slices.Contains(pkg.Files, filePath) {
				dir = pkg.Dir
			}
			if dir != "" {
				break
			}
		}
		if dir != "" {
			files[dir] = append(files[dir], astFile)
		}
	}
	return files
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

		ast.Inspect(astFile, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				currentFunc = funcDeclName(node, pkgPath)
			case *ast.SelectorExpr:
				resolveSelectorRef(node, currentFunc, fset, info, result)
			case *ast.Ident:
				resolveIdentRef(node, currentFunc, fset, info, result)
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

type importer struct{}

func (i importer) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	return nil, nil
}
