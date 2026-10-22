package query

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"sort"
	"strings"

	"codemap/store"
)

// RenameFile reports one file a rename rewrote and how many occurrences it
// carried.
type RenameFile struct {
	File  string `json:"file"`
	Sites int    `json:"sites"`
}

// RenamePlan is a validated rename: every site resolved to a byte range, every
// refusal already applied, and nothing written yet. Applying it is the only step
// that touches the filesystem, which is what lets a caller confine the write
// before it happens.
type RenamePlan struct {
	QualifiedName       string       `json:"qualified_name"`
	NewName             string       `json:"new_name"`
	Files               []RenameFile `json:"files"`
	Repos               []string     `json:"repos,omitempty"`
	OverriddenShadowing []string     `json:"overridden_shadowing,omitempty"`

	plans     []renameFilePlan
	SiteCount int `json:"site_count"`
}

// RenameResult reports a completed rename. SiteCount counts the declaration
// plus every rewritten reference occurrence.
type RenameResult struct {
	Action              string       `json:"action"`
	QualifiedName       string       `json:"qualified_name"`
	NewName             string       `json:"new_name"`
	ReindexError        string       `json:"reindex_error,omitempty"`
	Files               []RenameFile `json:"files"`
	Repos               []string     `json:"repos,omitempty"`
	OverriddenShadowing []string     `json:"overridden_shadowing,omitempty"`
	SiteCount           int          `json:"site_count"`
}

// renameSite is one occurrence to rewrite: a declaration (Column 0) or an
// indexed edge site. Package is the package the occurrence is written in, which
// the visibility check compares against the symbol's own package.
type renameSite struct {
	File    string
	Package string
	Line    int
	Column  int
}

// renameSplice is the byte range of one identifier occurrence in a file.
type renameSplice struct {
	start int
	end   int
}

// renameFilePlan is one file's pending rewrite: its current bytes and the
// identifier ranges to replace.
type renameFilePlan struct {
	path    string
	data    []byte
	splices []renameSplice
}

// RenameSymbol collects a symbol's declaration and every indexed reference site
// and validates that renaming it to newName can be applied correctly. It refuses,
// without touching any file, whenever the index cannot support a correct rename:
// an invalid identifier, a same-package or same-receiver collision, a
// cross-package unexported reference, a site whose recorded position no longer
// holds the name, or (unless force) a site inside a scope declaring a same-named
// local. The returned plan is applied with ApplyRename.
func RenameSymbol(s *store.Store, qualifiedName, newName string, force bool) (*RenamePlan, error) {
	sym, err := renameTarget(s, qualifiedName)
	if err != nil {
		return nil, err
	}
	if newName == sym.Name {
		return nil, fmt.Errorf("refusing to rename %s: %q is the current name; nothing changed", qualifiedName, newName)
	}
	if !token.IsIdentifier(newName) {
		return nil, fmt.Errorf("refusing to rename %s: %q is not a valid Go identifier", qualifiedName, newName)
	}
	if err := renameCollision(s, sym, newName); err != nil {
		return nil, err
	}

	sites, err := collectRenameSites(s, sym)
	if err != nil {
		return nil, err
	}
	if err := visibilityRefusal(sym, sites); err != nil {
		return nil, err
	}

	plans, shadowed, err := planRename(sym, sites)
	if err != nil {
		return nil, err
	}
	if len(shadowed) > 0 && !force {
		return nil, fmt.Errorf("refusing to rename %s: %d site(s) lie in a scope declaring a local %q, so the identifier there may not be the symbol: %s; pass force to override",
			qualifiedName, len(shadowed), sym.Name, strings.Join(shadowed, ", "))
	}

	plan := &RenamePlan{
		QualifiedName:       qualifiedName,
		NewName:             newName,
		OverriddenShadowing: shadowed,
		plans:               plans,
		Repos:               renameRepos(s, plans),
	}
	for _, file := range plans {
		plan.Files = append(plan.Files, RenameFile{File: file.path, Sites: len(file.splices)})
		plan.SiteCount += len(file.splices)
	}
	return plan, nil
}

// ApplyRename rewrites every site in the plan. Each file is spliced and parsed
// in memory first, so a rename that would break any file writes nothing.
func ApplyRename(plan *RenamePlan) (*RenameResult, error) {
	rewritten := make([][]byte, len(plan.plans))
	for i, file := range plan.plans {
		current, err := os.ReadFile(file.path)
		if err != nil {
			return nil, fmt.Errorf("refusing to rename %s: cannot re-read %s: %w", plan.QualifiedName, file.path, err)
		}
		if !bytes.Equal(current, file.data) {
			return nil, fmt.Errorf("refusing to rename %s: %s changed after the rename was planned; re-run it", plan.QualifiedName, file.path)
		}
		data, err := spliceIdentifiers(file.data, file.splices, plan.NewName)
		if err != nil {
			return nil, fmt.Errorf("refusing to rename %s: %s: %w", plan.QualifiedName, file.path, err)
		}
		if _, err := parser.ParseFile(token.NewFileSet(), file.path, data, parser.ParseComments); err != nil {
			return nil, fmt.Errorf("refusing to rename %s: the rename would break %s: %w", plan.QualifiedName, file.path, err)
		}
		rewritten[i] = data
	}
	for i, file := range plan.plans {
		if err := writeFileAtomic(file.path, rewritten[i]); err != nil {
			return nil, err
		}
	}
	return &RenameResult{
		Action:              "rename",
		QualifiedName:       plan.QualifiedName,
		NewName:             plan.NewName,
		SiteCount:           plan.SiteCount,
		Files:               plan.Files,
		Repos:               plan.Repos,
		OverriddenShadowing: plan.OverriddenShadowing,
	}, nil
}

// renameTarget resolves the symbol to rename, reporting a miss with near-name
// candidates like every other write path.
func renameTarget(s *store.Store, qualifiedName string) (*store.Symbol, error) {
	sym, err := s.SymbolByName(qualifiedName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, symbolMissError(s, qualifiedName)
		}
		return nil, err
	}
	return sym, nil
}

// renameCollision refuses a new name that already names a package-level symbol
// in the same package, or a method on the same receiver.
func renameCollision(s *store.Store, sym *store.Symbol, newName string) error {
	if sym.Kind == kindMethod {
		receiver := strings.TrimSuffix(sym.QualifiedName, "."+sym.Name)
		methods, err := s.MethodsByQualifiedReceiver(receiver, true)
		if err != nil {
			return err
		}
		for _, m := range methods {
			if m.Name == newName {
				return fmt.Errorf("refusing to rename %s: receiver %s already declares method %s",
					sym.QualifiedName, m.Receiver, m.QualifiedName)
			}
		}
		return nil
	}

	syms, err := s.SymbolsByPackage(sym.PackagePath, true)
	if err != nil {
		return err
	}
	for _, other := range syms {
		if other.Name == newName && other.Kind != kindMethod {
			return fmt.Errorf("refusing to rename %s: package %s already declares %s",
				sym.QualifiedName, sym.PackagePath, other.QualifiedName)
		}
	}
	return nil
}

// collectRenameSites gathers the declaration plus every site recorded on an
// incoming calls, references, satisfies, or embeds edge. Identical positions are
// deduplicated; edges without a recorded position (structural satisfies and
// embeds) contribute nothing, since they have no identifier to rewrite.
func collectRenameSites(s *store.Store, sym *store.Symbol) ([]renameSite, error) {
	seen := map[renameSite]bool{}
	var sites []renameSite
	add := func(site renameSite) {
		if seen[site] {
			return
		}
		seen[site] = true
		sites = append(sites, site)
	}

	add(renameSite{File: sym.PosFile, Line: sym.PosLine, Package: sym.PackagePath})

	edges, err := s.EdgesTo(sym.QualifiedName)
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		if !slices.Contains(liveEdgeTypes, e.EdgeType) {
			continue
		}
		pkg := ""
		if from, err := s.SymbolByName(e.FromRef); err == nil {
			pkg = from.PackagePath
		}
		for _, site := range e.Sites {
			if site.File == "" {
				continue
			}
			add(renameSite{File: site.File, Line: site.Line, Column: site.Column, Package: pkg})
		}
	}
	return sites, nil
}

// visibilityRefusal rejects an unexported rename whose references live in
// another package: those references would stop resolving.
func visibilityRefusal(sym *store.Symbol, sites []renameSite) error {
	if sym.Exported {
		return nil
	}
	var foreign []string
	for _, site := range sites {
		if site.Package != "" && site.Package != sym.PackagePath {
			foreign = append(foreign, fmt.Sprintf("%s:%d", site.File, site.Line))
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to rename unexported %s: %d reference site(s) lie in another package: %s",
		sym.QualifiedName, len(foreign), strings.Join(foreign, ", "))
}

// planRename reads and parses every file a site lands in, resolving each site to
// an identifier offset. Any site that cannot be resolved refuses the whole
// rename. It also reports sites enclosed by a same-named local declaration.
func planRename(sym *store.Symbol, sites []renameSite) ([]renameFilePlan, []string, error) {
	byFile := make(map[string][]renameSite)
	var order []string
	for _, site := range sites {
		if _, ok := byFile[site.File]; !ok {
			order = append(order, site.File)
		}
		byFile[site.File] = append(byFile[site.File], site)
	}

	plans := make([]renameFilePlan, 0, len(order))
	var shadowed []string
	for _, path := range order {
		plan, fileShadowed, err := planFileRename(sym, path, byFile[path])
		if err != nil {
			return nil, nil, err
		}
		plans = append(plans, plan)
		shadowed = append(shadowed, fileShadowed...)
	}
	return plans, shadowed, nil
}

func planFileRename(sym *store.Symbol, path string, sites []renameSite) (renameFilePlan, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return renameFilePlan{}, nil, fmt.Errorf("refusing to rename %s: cannot read site file %s: %w", sym.QualifiedName, path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return renameFilePlan{}, nil, fmt.Errorf("refusing to rename %s: cannot parse site file %s: %w", sym.QualifiedName, path, err)
	}

	idents := identifierOffsets(fset, file)
	plan := renameFilePlan{path: path, data: data}
	var shadowed, missing []string
	for _, site := range sites {
		start, ok := siteOffset(fset, file, idents, sym, site)
		if !ok {
			missing = append(missing, fmt.Sprintf("%s:%d:%d", site.File, site.Line, site.Column))
			continue
		}
		plan.splices = append(plan.splices, renameSplice{start: start, end: start + len(sym.Name)})
		if site.Column != 0 && enclosingFuncDeclares(fset, file, start, sym.Name) {
			shadowed = append(shadowed, fmt.Sprintf("%s:%d:%d", site.File, site.Line, site.Column))
		}
	}
	if len(missing) > 0 {
		return renameFilePlan{}, nil, fmt.Errorf("refusing to rename %s: the name %q is not at the recorded position(s): %s",
			sym.QualifiedName, sym.Name, strings.Join(missing, ", "))
	}

	sort.Slice(plan.splices, func(i, j int) bool {
		return plan.splices[i].start > plan.splices[j].start
	})
	return plan, shadowed, nil
}

// siteOffset resolves one site to a byte offset. A site with a column is looked
// up exactly; a declaration site (column 0) is resolved from the declaration
// node, since the index records no column for symbols.
func siteOffset(fset *token.FileSet, file *ast.File, idents map[[2]int]string, sym *store.Symbol, site renameSite) (int, bool) {
	if site.Column == 0 {
		return declarationOffset(fset, file, sym)
	}
	if name, ok := idents[[2]int{site.Line, site.Column}]; !ok || name != sym.Name {
		return 0, false
	}
	tf := fset.File(file.Pos())
	return tf.Offset(tf.LineStart(site.Line)) + site.Column - 1, true
}

// identifierOffsets maps each identifier's (line, column) to its name, which is
// what turns a recorded edge site into a byte offset.
func identifierOffsets(fset *token.FileSet, file *ast.File) map[[2]int]string {
	out := make(map[[2]int]string)
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		pos := fset.Position(ident.Pos())
		out[[2]int{pos.Line, pos.Column}] = ident.Name
		return true
	})
	return out
}

// declarationOffset returns the offset of the identifier that declares sym in
// file, matched by name and declaration line.
func declarationOffset(fset *token.FileSet, file *ast.File, sym *store.Symbol) (int, bool) {
	for _, decl := range file.Decls {
		if ident := declaredIdent(decl, sym, fset); ident != nil {
			return fset.Position(ident.Pos()).Offset, true
		}
	}
	return 0, false
}

// declaredIdent returns the identifier decl declares for sym, or nil when decl
// declares something else.
func declaredIdent(decl ast.Decl, sym *store.Symbol, fset *token.FileSet) *ast.Ident {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return identAtLine(d.Name, sym, fset)
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if ident := identAtLine(s.Name, sym, fset); ident != nil {
					return ident
				}
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if ident := identAtLine(n, sym, fset); ident != nil {
						return ident
					}
				}
			}
		}
	}
	return nil
}

// identAtLine reports ident when it names sym on the line the index recorded.
func identAtLine(ident *ast.Ident, sym *store.Symbol, fset *token.FileSet) *ast.Ident {
	if ident == nil || ident.Name != sym.Name {
		return nil
	}
	if fset.Position(ident.Pos()).Line != sym.PosLine {
		return nil
	}
	return ident
}

// enclosingFuncDeclares reports whether a same-named local is declared by the
// function containing offset. The check is deliberately conservative: it looks
// for any same-named declaration in the enclosing function, because the index
// records that a reference exists at a position, not that the identifier token
// there is the one producing it.
func enclosingFuncDeclares(fset *token.FileSet, file *ast.File, offset int, name string) bool {
	pos := fset.File(file.Pos()).Pos(offset)
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if pos < fd.Pos() || pos >= fd.End() {
			continue
		}
		return funcDeclaresName(fd, name)
	}
	return false
}

func funcDeclaresName(fd *ast.FuncDecl, name string) bool {
	if fieldListDeclares(fd.Recv, name) || fieldListDeclares(fd.Type.Params, name) || fieldListDeclares(fd.Type.Results, name) {
		return true
	}
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch node := n.(type) {
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE && exprListDeclares(node.Lhs, name) {
				found = true
			}
		case *ast.RangeStmt:
			if node.Tok == token.DEFINE && (identNamed(node.Key, name) || identNamed(node.Value, name)) {
				found = true
			}
		case *ast.DeclStmt:
			if genDeclDeclares(node.Decl, name) {
				found = true
			}
		}
		return !found
	})
	return found
}

func fieldListDeclares(fields *ast.FieldList, name string) bool {
	if fields == nil {
		return false
	}
	for _, field := range fields.List {
		for _, n := range field.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

func exprListDeclares(exprs []ast.Expr, name string) bool {
	for _, expr := range exprs {
		if identNamed(expr, name) {
			return true
		}
	}
	return false
}

func identNamed(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

func genDeclDeclares(decl ast.Decl, name string) bool {
	gd, ok := decl.(*ast.GenDecl)
	if !ok {
		return false
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, n := range vs.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// spliceIdentifiers replaces each splice with newName. Splices are applied
// right-to-left so an earlier occurrence's offset stays valid after a later one
// changes the byte length, which is what makes two occurrences on one line safe.
// A splice past the end of data means the file was rewritten after the plan was
// built, which is refused rather than applied at the wrong offset.
func spliceIdentifiers(data []byte, splices []renameSplice, newName string) ([]byte, error) {
	out := slices.Clone(data)
	for _, sp := range splices {
		if sp.start < 0 || sp.end > len(out) || sp.end < sp.start {
			return nil, fmt.Errorf("site offset %d-%d is past the end of the %d-byte file", sp.start, sp.end, len(out))
		}
		tail := slices.Clone(out[sp.end:])
		out = append(out[:sp.start], newName...)
		out = append(out, tail...)
	}
	return out, nil
}

// renameRepos returns the repos a rename touched, so the caller can reindex each
// once. A single-repo index has no repo rows, so the empty module path is
// returned as the one repo to rebuild.
func renameRepos(s *store.Store, plans []renameFilePlan) []string {
	repos, err := s.ListRepos()
	if err != nil {
		return nil
	}
	if len(repos) == 0 {
		return []string{""}
	}
	seen := map[string]bool{}
	var out []string
	for _, plan := range plans {
		for _, r := range repos {
			if seen[r.ModulePath] {
				continue
			}
			if !strings.HasPrefix(plan.path, r.Dir+string(os.PathSeparator)) && plan.path != r.Dir {
				continue
			}
			seen[r.ModulePath] = true
			out = append(out, r.ModulePath)
		}
	}
	return out
}
