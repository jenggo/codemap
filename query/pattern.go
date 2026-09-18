package query

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"sort"
	"strings"

	"codemap/parse"
	"codemap/pattern"
	"codemap/store"
)

// PatternMatch is one AST pattern match: its location, the matched source text,
// and the enclosing indexed symbol when the match lies inside one.
type PatternMatch struct {
	Symbol string `json:"symbol,omitempty"`
	File   string `json:"file"`
	Text   string `json:"text"`
	Repo   string `json:"repo,omitempty"`
	Line   int    `json:"line"`
}

// PatternSearch compiles src, loads the indexed files in scope, pre-filters
// them by the pattern's literal tokens, and matches the ASTs of the remaining
// files. Matching is syntactic and read-only.
func PatternSearch(s *store.Store, src, filePattern, repo string) ([]PatternMatch, error) {
	compiled, err := pattern.Compile(src)
	if err != nil {
		return nil, err
	}
	files, err := s.IndexedFiles(repo, filePattern)
	if err != nil {
		return nil, fmt.Errorf("pattern search: loading indexed files: %w", err)
	}
	symbols, err := symbolsByFile(s)
	if err != nil {
		return nil, fmt.Errorf("pattern search: loading symbols: %w", err)
	}
	return matchIndexedFiles(prefilter(files, compiled.Literals()), compiled, symbols), nil
}

// symbolIndex maps an indexed file path to the symbols defined in it.
type symbolIndex map[string][]store.Symbol

func symbolsByFile(s *store.Store) (symbolIndex, error) {
	syms, err := s.AllSymbols(true)
	if err != nil {
		return nil, err
	}
	index := make(symbolIndex)
	for _, sym := range syms {
		index[sym.PosFile] = append(index[sym.PosFile], sym)
	}
	return index, nil
}

// prefilter drops files that cannot contain a match because they lack one of
// the pattern's literal tokens. Literals are the pattern's non-metavariable
// tokens, which structural matching requires to appear verbatim.
func prefilter(files []store.IndexedFile, literals []string) []store.IndexedFile {
	if len(literals) == 0 {
		return files
	}
	out := make([]store.IndexedFile, 0, len(files))
	for _, f := range files {
		if containsAll(f.Content, literals) {
			out = append(out, f)
		}
	}
	return out
}

func containsAll(content string, literals []string) bool {
	for _, lit := range literals {
		if !strings.Contains(content, lit) {
			return false
		}
	}
	return true
}

// matchIndexedFiles parses each file and reports every matching node. A file
// whose stored content no longer parses is skipped: indexing stores content,
// not ASTs, and an unparseable file cannot be matched.
func matchIndexedFiles(files []store.IndexedFile, compiled *pattern.Pattern, symbols symbolIndex) []PatternMatch {
	var matches []PatternMatch
	for _, f := range files {
		fset := token.NewFileSet()
		file, err := parse.File(fset, f.Path, []byte(f.Content))
		if err != nil {
			continue
		}
		matches = append(matches, matchFile(fset, file, f, compiled, symbols[f.Path])...)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].File != matches[j].File {
			return matches[i].File < matches[j].File
		}
		if matches[i].Line != matches[j].Line {
			return matches[i].Line < matches[j].Line
		}
		return matches[i].Text < matches[j].Text
	})
	return matches
}

// matchFile walks one parsed file, matching every node against the pattern.
// The enclosing-declaration stack is maintained so each match can be
// attributed to the innermost indexed symbol that contains it.
func matchFile(fset *token.FileSet, file *ast.File, f store.IndexedFile, compiled *pattern.Pattern, syms []store.Symbol) []PatternMatch {
	byLine := make(map[int][]store.Symbol, len(syms))
	for _, sym := range syms {
		byLine[sym.PosLine] = append(byLine[sym.PosLine], sym)
	}

	var stack []ast.Node
	var out []PatternMatch
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return false
		}
		if _, ok := compiled.Match(fset, n); ok {
			out = append(out, buildPatternMatch(fset, n, f, stack, byLine))
		}
		stack = append(stack, n)
		return true
	})
	return out
}

func buildPatternMatch(fset *token.FileSet, n ast.Node, f store.IndexedFile, stack []ast.Node, byLine map[int][]store.Symbol) PatternMatch {
	start := fset.Position(n.Pos()).Offset
	end := fset.Position(n.End()).Offset
	m := PatternMatch{File: f.Path, Line: fset.Position(n.Pos()).Line, Repo: f.Repo, Text: sourceSlice(f.Content, start, end)}
	if sym, ok := enclosingSymbol(fset, stack, byLine); ok {
		m.Symbol = sym.QualifiedName
		if sym.Repo != "" {
			m.Repo = sym.Repo
		}
	}
	return m
}

func sourceSlice(content string, start, end int) string {
	if start < 0 || end < start || end > len(content) {
		return ""
	}
	return content[start:end]
}

// enclosingSymbol returns the innermost declaration on the ancestor stack that
// maps to an indexed symbol.
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
