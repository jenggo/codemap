package query

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"codemap/store"
)

// WriteResult describes one symbol write: what was touched, where, and whether
// the index or a reindex needs the caller's attention.
type WriteResult struct {
	Action        string `json:"action"`
	QualifiedName string `json:"qualified_name"`
	File          string `json:"file"`
	Kind          string `json:"kind"`
	ReindexError  string `json:"reindex_error,omitempty"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	NewLines      int    `json:"new_lines"`
	Unchanged     bool   `json:"unchanged,omitempty"`
	StaleIndex    bool   `json:"stale_index,omitempty"`
}

// ReplaceSymbolBody replaces a symbol's declaration span with body. The span
// begins at the declaration, or at its doc comment when includeDoc is set, and
// is located by the same helper the read path uses so reads and writes can
// never disagree about where a declaration starts or ends.
func ReplaceSymbolBody(s *store.Store, qualifiedName, body string, includeDoc bool) (*WriteResult, error) {
	sym, span, path, err := writeSpan(s, qualifiedName)
	if err != nil {
		return nil, err
	}
	if err := groupRefusal(sym, span, "replace"); err != nil {
		return nil, err
	}

	start := span.startOff
	if includeDoc && span.docStartOff >= 0 {
		start = span.docStartOff
	}
	result := newWriteResult("replace", sym, span, start, span.endOff)

	if string(span.data[start:span.endOff]) == body {
		result.Unchanged = true
		result.NewLines = lineCount(body)
		return result, nil
	}

	newData, err := spliceAndValidate(span.data, start, span.endOff, body)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, newData); err != nil {
		return nil, err
	}
	result.NewLines = lineCount(body)
	return result, nil
}

// InsertSymbol inserts text immediately before or after a symbol's declaration.
// An insert before a documented declaration goes before its doc comment, so the
// comment keeps documenting the declaration it was written for.
func InsertSymbol(s *store.Store, qualifiedName, text string, after bool) (*WriteResult, error) {
	sym, span, path, err := writeSpan(s, qualifiedName)
	if err != nil {
		return nil, err
	}
	if err := validateSnippet(text); err != nil {
		return nil, err
	}

	off := span.startOff
	if after {
		off = span.endOff
	} else if span.docStartOff >= 0 {
		off = span.docStartOff
	}

	newData := insertText(span.data, off, text)
	if err := parseWholeFile(newData, path, "insert"); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, newData); err != nil {
		return nil, err
	}

	result := newWriteResult("insert", sym, span, off, off)
	result.NewLines = lineCount(strings.Trim(text, "\n"))
	return result, nil
}

// DeleteSymbol removes a symbol's declaration, refusing when the index records
// a live incoming reference, when the symbol is exported, or when its span is a
// whole grouped declaration. The doc comment is removed with the declaration.
func DeleteSymbol(s *store.Store, qualifiedName string, allowExported, allowTestOnly bool) (*WriteResult, error) {
	sym, span, path, err := writeSpan(s, qualifiedName)
	if err != nil {
		return nil, err
	}
	if err := groupRefusal(sym, span, "delete"); err != nil {
		return nil, err
	}
	if sym.Exported && !allowExported {
		return nil, fmt.Errorf("refusing to delete exported symbol %s: a consumer outside this index may use it; pass allow_exported to override", sym.QualifiedName)
	}
	if err := deleteRefusal(s, sym, allowTestOnly); err != nil {
		return nil, err
	}

	start := span.startOff
	if span.docStartOff >= 0 {
		start = span.docStartOff
	}
	end := span.endOff
	if end < len(span.data) && span.data[end] == '\n' {
		end++
	}

	result := newWriteResult("delete", sym, span, start, span.endOff)
	newData := collapseJoin(splice(span.data, start, end, ""), start)
	if err := parseWholeFile(newData, path, "deletion"); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, newData); err != nil {
		return nil, err
	}
	return result, nil
}

// deleteRefusal refuses a delete whose symbol the index still points at,
// listing each referrer with its file:line so the caller can judge the
// references itself. Test-only references are refusable by default and
// overridable, since production code does not depend on them.
func deleteRefusal(s *store.Store, sym *store.Symbol, allowTestOnly bool) error {
	refs, err := liveReferences(s, sym.QualifiedName)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	testOnly := refsAreTestOnly(s, refs)
	if testOnly && allowTestOnly {
		return nil
	}
	msg := fmt.Sprintf("refusing to delete %s: %d incoming reference(s): %s",
		sym.QualifiedName, len(refs), formatRefs(refs))
	if testOnly {
		msg += "; all references are test-only, pass allow_test_only to override"
	}
	return errors.New(msg)
}

// liveReferences returns the incoming edges that keep a symbol alive.
func liveReferences(s *store.Store, qualifiedName string) ([]store.Edge, error) {
	edges, err := s.EdgesTo(qualifiedName)
	if err != nil {
		return nil, err
	}
	var live []store.Edge
	for _, e := range edges {
		if slices.Contains(liveEdgeTypes, e.EdgeType) {
			live = append(live, e)
		}
	}
	return live, nil
}

// refsAreTestOnly reports whether every referencing symbol is test code. An
// unresolvable referrer counts as live, so a delete is never allowed on the
// strength of a missing row.
func refsAreTestOnly(s *store.Store, refs []store.Edge) bool {
	for _, e := range refs {
		sym, err := s.SymbolByName(e.FromRef)
		if err != nil || !sym.IsTest {
			return false
		}
	}
	return true
}

func formatRefs(refs []store.Edge) string {
	parts := make([]string, 0, len(refs))
	for _, e := range refs {
		parts = append(parts, fmt.Sprintf("%s (%s:%d)", e.FromRef, e.PosFile, e.PosLine))
	}
	return strings.Join(parts, ", ")
}

// writeSpan resolves the symbol a write names and locates its declaration,
// returning the file path with symlinks resolved so a rename replaces the file
// itself rather than the link pointing at it.
func writeSpan(s *store.Store, qualifiedName string) (*store.Symbol, *declSpan, string, error) {
	sym, err := s.SymbolByName(qualifiedName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, "", symbolMissError(s, qualifiedName)
		}
		return nil, nil, "", err
	}
	span, err := symbolSpan(sym.PosFile, sym.PosLine, sym.Receiver, sym.Name, sym.Kind)
	if err != nil {
		return nil, nil, "", err
	}
	path, err := filepath.EvalSymlinks(sym.PosFile)
	if err != nil {
		return nil, nil, "", err
	}
	return sym, span, path, nil
}

// groupRefusal rejects a span-rewriting write on a member of a grouped
// declaration: the indexed span is the whole group, so replacing or deleting it
// would drop the sibling members the caller never named.
func groupRefusal(sym *store.Symbol, span *declSpan, action string) error {
	if !span.partOfGroup {
		return nil
	}
	return fmt.Errorf("refusing to %s %s: its indexed span is the whole grouped %s declaration (%d members); use insert_before_symbol or insert_after_symbol instead",
		action, sym.QualifiedName, sym.Kind, span.groupMembers)
}

func newWriteResult(action string, sym *store.Symbol, span *declSpan, start, end int) *WriteResult {
	offsets := computeLineOffsets(span.data)
	return &WriteResult{
		Action:        action,
		QualifiedName: sym.QualifiedName,
		File:          sym.PosFile,
		Kind:          sym.Kind,
		StartLine:     offsetToLine(offsets, start),
		EndLine:       offsetToLine(offsets, end),
		StaleIndex:    span.stale,
	}
}

// spliceAndValidate validates text on its own, splices it over data[start:end],
// and requires the spliced file to parse. Both checks run before any write, so
// a rejected edit leaves the file untouched.
func spliceAndValidate(data []byte, start, end int, text string) ([]byte, error) {
	if err := validateSnippet(text); err != nil {
		return nil, err
	}
	out := splice(data, start, end, text)
	if err := parseWholeFile(out, "", "replacement"); err != nil {
		return nil, err
	}
	return out, nil
}

func splice(data []byte, start, end int, text string) []byte {
	out := make([]byte, 0, len(data)-(end-start)+len(text))
	out = append(out, data[:start]...)
	out = append(out, text...)
	out = append(out, data[end:]...)
	return out
}

// validateSnippet requires text to parse as a Go declaration, so bad input is
// reported against the snippet rather than against the file it would corrupt.
func validateSnippet(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("replacement text is empty")
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "snippet.go", "package snippet\n"+text, parser.ParseComments); err != nil {
		return fmt.Errorf("replacement text does not parse: %w", err)
	}
	return nil
}

func parseWholeFile(data []byte, path, action string) error {
	if _, err := parser.ParseFile(token.NewFileSet(), path, data, parser.ParseComments); err != nil {
		return fmt.Errorf("%s would break the file: %w", action, err)
	}
	return nil
}

// collapseJoin removes the blank line a deletion leaves behind when the text
// around the cut already ends and resumes with one, so removing a declaration
// never widens the gap between its neighbours.
func collapseJoin(data []byte, off int) []byte {
	if off == 0 || off >= len(data) {
		return data
	}
	if !bytes.HasSuffix(data[:off], []byte("\n\n")) || !bytes.HasPrefix(data[off:], []byte("\n")) {
		return data
	}
	out := make([]byte, 0, len(data)-1)
	out = append(out, data[:off]...)
	return append(out, data[off+1:]...)
}

// insertText splices text at off, keeping exactly one blank line between the
// inserted text and the declarations on either side.
func insertText(data []byte, off int, text string) []byte {
	body := strings.Trim(text, "\n")
	var b bytes.Buffer
	b.Grow(len(data) + len(body) + 4)
	b.Write(data[:off])
	switch {
	case off == 0:
	case bytes.HasSuffix(data[:off], []byte("\n\n")):
	case bytes.HasSuffix(data[:off], []byte("\n")):
		b.WriteString("\n")
	default:
		b.WriteString("\n\n")
	}
	b.WriteString(body)
	b.WriteString("\n")
	if rest := bytes.TrimLeft(data[off:], "\n"); len(rest) > 0 {
		b.WriteString("\n")
		b.Write(rest)
	}
	return b.Bytes()
}

// lineCount counts the lines text occupies, ignoring a trailing newline.
func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
}

// writeFileAtomic replaces path's content with data via a temp file in the same
// directory, so a failure can never leave a truncated source file. The original
// permission bits are preserved and the temp file never survives a failure.
func writeFileAtomic(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".codemap-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Chmod(tmpPath, info.Mode().Perm()); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fail(err)
	}
	return nil
}
