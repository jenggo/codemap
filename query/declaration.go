package query

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"codemap/store"
)

// Declaration is one resolved symbol: where it is declared and what it is.
type Declaration struct {
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	File          string `json:"file"`
	Receiver      string `json:"receiver,omitempty"`
	Line          int    `json:"line"`
}

// DeclarationResult is the answer to a position or occurrence lookup. Message
// carries the "nothing here" explanations, so a caller can distinguish an
// unindexed file from a position outside every declaration without parsing an
// error string.
type DeclarationResult struct {
	Symbol     *Declaration  `json:"symbol,omitempty"`
	Mode       string        `json:"mode"`
	Message    string        `json:"message,omitempty"`
	Candidates []Declaration `json:"candidates,omitempty"`
}

const (
	declarationModePosition   = "position"
	declarationModeOccurrence = "occurrence"
)

// FindDeclaration resolves either a source position (line, optionally column)
// or a name occurrence in a file to the indexed symbol it belongs to. A
// position resolves by AST span containment; an occurrence resolves through the
// index's edges, so no scope rules are re-derived here.
func FindDeclaration(s *store.Store, filePath string, line, col int, symbol string) (*DeclarationResult, error) {
	indexed, err := indexedFilePath(s, filePath)
	if err != nil {
		return nil, err
	}
	if symbol != "" {
		return findDeclarationByOccurrence(s, indexed, symbol)
	}
	return findDeclarationByPosition(s, indexed, line, col)
}

// findDeclarationByPosition returns the innermost symbol whose span contains the
// position. A position outside every declaration is reported, not errored: it is
// a valid answer (package clause, import block, blank line).
func findDeclarationByPosition(s *store.Store, filePath string, line, col int) (*DeclarationResult, error) {
	result := &DeclarationResult{Mode: declarationModePosition}
	sym, ok, err := EnclosingSymbolAt(s, filePath, line, col)
	if err != nil {
		return nil, err
	}
	if !ok {
		result.Message = fmt.Sprintf("no indexed symbol contains %s:%d", filePath, line)
		return result, nil
	}
	result.Symbol = declarationOf(sym)
	return result, nil
}

// findDeclarationByOccurrence resolves name to a declaration by asking the index
// what the file's own symbols reference, plus what the file declares itself.
// Several matches are returned together so the caller can disambiguate.
func findDeclarationByOccurrence(s *store.Store, filePath, name string) (*DeclarationResult, error) {
	result := &DeclarationResult{Mode: declarationModeOccurrence}
	candidates, err := occurrenceCandidates(s, filePath, name)
	if err != nil {
		return nil, err
	}
	switch len(candidates) {
	case 1:
		result.Symbol = &candidates[0]
	case 0:
		result.Message = fmt.Sprintf("could not resolve %q in %s", name, filePath)
		if suggestions := symbolCandidates(s, name, ""); len(suggestions) > 0 {
			result.Message += "; nearest symbol names: " + strings.Join(suggestions, ", ")
		}
	default:
		result.Candidates = candidates
	}
	return result, nil
}

// occurrenceCandidates collects the indexed symbols the file's own declarations
// and references could name, deduplicated by qualified name.
func occurrenceCandidates(s *store.Store, filePath, name string) ([]Declaration, error) {
	inFile, err := symbolsInExactFile(s, filePath)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []Declaration
	add := func(sym store.Symbol) {
		if seen[sym.QualifiedName] {
			return
		}
		seen[sym.QualifiedName] = true
		out = append(out, *declarationOf(sym))
	}
	for _, sym := range inFile {
		if sym.Name == name {
			add(sym)
		}
	}
	for _, sym := range inFile {
		referenced, err := referencedByName(s, sym.QualifiedName, name)
		if err != nil {
			return nil, err
		}
		for _, target := range referenced {
			add(target)
		}
	}
	return out, nil
}

// referencedByName returns the live-edge targets of one symbol whose short name
// matches, which is how an occurrence resolves to a declaration in another file.
func referencedByName(s *store.Store, qualifiedName, name string) ([]store.Symbol, error) {
	edges, err := s.EdgesFrom(qualifiedName)
	if err != nil {
		return nil, err
	}
	var out []store.Symbol
	for _, e := range edges {
		if !slices.Contains(liveEdgeTypes, e.EdgeType) || shortRefName(e.ToRef) != name {
			continue
		}
		target, err := s.SymbolByName(e.ToRef)
		if err != nil {
			continue
		}
		out = append(out, *target)
	}
	return out, nil
}

// symbolsInExactFile returns the symbols declared in filePath. The store's file
// filter is a substring match, so the result is narrowed to exact paths.
func symbolsInExactFile(s *store.Store, filePath string) ([]store.Symbol, error) {
	syms, err := s.SearchSymbolsByFile(filePath, "", nil, true, store.GeneratedAny)
	if err != nil {
		return nil, err
	}
	out := syms[:0]
	for _, sym := range syms {
		if sym.PosFile == filePath {
			out = append(out, sym)
		}
	}
	return out, nil
}

func declarationOf(sym store.Symbol) *Declaration {
	return &Declaration{
		QualifiedName: sym.QualifiedName,
		Kind:          sym.Kind,
		File:          sym.PosFile,
		Line:          sym.PosLine,
		Receiver:      sym.Receiver,
	}
}

// indexedFilePath returns the path the index stores for filePath, accepting a
// relative path and reporting a clear error when the file was never indexed.
func indexedFilePath(s *store.Store, filePath string) (string, error) {
	candidates := []string{filePath}
	if abs, err := filepath.Abs(filePath); err == nil && abs != filePath {
		candidates = append(candidates, abs)
	}
	for _, candidate := range candidates {
		if _, err := s.FileContent(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", fmt.Errorf("file is not indexed: %s", filePath)
}

// shortRefName returns the last segment of a qualified name, which is the name
// the source writes at a reference site.
func shortRefName(qualifiedName string) string {
	_, short, ok := strings.CutLast(qualifiedName, ".")
	if !ok {
		return qualifiedName
	}
	return short
}
