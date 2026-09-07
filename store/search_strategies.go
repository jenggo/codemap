package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// This file exposes the raw retrieval strategies behind the fused text
// search pipeline: a ranked FTS5 trigram pass, a case-insensitive substring
// scan, and the symbol lexicon used for typo correction. The strategies are
// deliberately shaped like FileMatch so the query layer can fuse them
// without re-reading contents; SearchFileContent keeps its original
// behavior for regex mode and direct callers.

// FileMatchesFTS returns ranked line-level candidates for pattern using the
// FTS5 trigram content index, without the regex fallback. Patterns the
// trigram tokenizer cannot serve (any alphanumeric run shorter than three
// characters) fall back to the direct substring full scan, matching the
// dispatch inside SearchFileContent. Rows are ordered by bm25 relevance when
// FTS served the match and by path, rowid otherwise; an alternative of the
// pattern must still occur verbatim on a line for that line to be returned.
// The flattened match list is capped at limit entries (limit <= 0 means
// uncapped) and each match carries contextLines of surrounding text.
func (s *Store) FileMatchesFTS(pattern, filePattern string, limit, contextLines int) ([]FileMatch, error) {
	sanitized := sanitizeFTSQuery(pattern)
	if sanitized == "" {
		return nil, nil
	}
	fullScan := ftsNeedsFullScan(pattern)

	query, args := literalContentQuery(pattern, sanitized, fullScan)

	if filePattern != "" {
		query += " AND " + likeMatch("f.path")
		args = append(args, likePattern(filePattern))
	}
	if fullScan {
		query += ` ORDER BY f.path, f.rowid`
	} else {
		query += ` ORDER BY bm25(file_content_fts), f.path, f.rowid`
	}
	return s.fileMatchesQuery(query, args, pattern, limit, contextLines)
}

// literalContentQuery builds the candidate query and bindings shared by the
// literal paths of FileMatchesFTS and SearchFileContent: the FTS5 MATCH
// expression (or a short-token full scan when the trigram tokenizer cannot
// serve the pattern) gated by a per-alternative verbatim instr OR. The
// per-alternative prefilter preserves the line-consistency invariant — a row
// kept here always contains one alternative verbatim, so the per-line
// extractMatches walk yields at least one visible match.
func literalContentQuery(pattern, sanitized string, fullScan bool) (string, []any) {
	cond, args := alternationInstrCond("f.content", SplitAlternatives(pattern))
	if fullScan {
		// The trigram tokenizer produces matches only for terms of at
		// least three characters; a shorter term silently matches nothing
		// in MATCH. Scan the files table directly so short patterns keep
		// returning the same verbatim matches they did under unicode61.
		return `SELECT f.path, f.content FROM files f WHERE ` + cond, args
	}
	// The FTS MATCH is a whole-content candidate prefilter; the verbatim
	// instr OR above ensures a row selected here always yields at least one
	// line from the per-line extractMatches walk.
	query := `SELECT f.path, f.content FROM file_content_fts fts JOIN files f ON f.rowid = fts.rowid WHERE file_content_fts MATCH ? AND ` + cond
	return query, append([]any{sanitized}, args...)
}

// alternationInstrCond builds a single SQL condition that keeps a row when
// any alternative occurs verbatim in the column: `(instr(col, ?) > 0 OR ...)`
// with one binding per alternative.
func alternationInstrCond(column string, alternatives []string) (string, []any) {
	conds := make([]string, len(alternatives))
	args := make([]any, len(alternatives))
	for i, alt := range alternatives {
		conds[i] = "instr(" + column + ", ?) > 0"
		args[i] = alt
	}
	return "(" + strings.Join(conds, " OR ") + ")", args
}

// FileMatchesSubstring returns candidates by scanning indexed file contents
// case-insensitively: alternatives is a list of term lists, and a file
// matches when every term of at least one alternative occurs somewhere in
// its content. Every line containing at least one term of any alternative
// becomes a candidate line, so terms scattered across lines are still
// recalled even when no single line holds a whole alternative.
// Punctuation-only terms and alternatives left without any word-bearing
// term are ignored. Files are ranked by match count descending (ties keep
// path order), lines within a file in ascending order; the flattened list
// is capped at limit entries (limit <= 0 means uncapped).
func (s *Store) FileMatchesSubstring(alternatives [][]string, filePattern string, limit, contextLines int) ([]FileMatch, error) {
	alternatives = substringAlternatives(alternatives)
	if len(alternatives) == 0 {
		return nil, nil
	}

	query := `SELECT f.path, f.content FROM files f`
	var args []any
	if filePattern != "" {
		query += " WHERE " + likeMatch("f.path")
		args = append(args, likePattern(filePattern))
	}
	query += ` ORDER BY f.path, f.rowid`

	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, fmt.Errorf("file content search failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type fileMatches struct {
		matches []FileMatch
	}
	var files []fileMatches
	for rows.Next() {
		var filePath, content string
		if err := rows.Scan(&filePath, &content); err != nil {
			return nil, err
		}
		if !matchesAnyAlternative(content, alternatives) {
			continue
		}
		ms := extractMatchesFunc(filePath, content, substringLineMatcher(alternatives), contextLines)
		if len(ms) > 0 {
			files = append(files, fileMatches{matches: ms})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Stable sort keeps the path, rowid order for equal match counts.
	sort.SliceStable(files, func(i, j int) bool {
		return len(files[i].matches) > len(files[j].matches)
	})

	var out []FileMatch
	for _, fm := range files {
		for _, m := range fm.matches {
			out = append(out, m)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// SearchLexicon returns the deduplicated, lowercased short names of all
// indexed symbols in sorted order, for use as a typo-correction lexicon.
func (s *Store) SearchLexicon() ([]string, error) {
	rows, err := s.db.QueryContext(context.Background(), `SELECT DISTINCT name FROM symbols WHERE name <> ''`)
	if err != nil {
		return nil, fmt.Errorf("search lexicon query failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[string]bool)
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		lower := strings.ToLower(name)
		if lower == "" || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, lower)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// fileMatchesQuery runs a ranked file-content query, extracts per-line
// matches for every selected row, and caps the flattened list at limit.
func (s *Store) fileMatchesQuery(query string, args []any, pattern string, limit, contextLines int) ([]FileMatch, error) {
	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, fmt.Errorf("file content search failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var matches []FileMatch
	for rows.Next() {
		var filePath, content string
		if err := rows.Scan(&filePath, &content); err != nil {
			return nil, err
		}
		for _, m := range extractMatches(filePath, content, pattern, nil, false, contextLines) {
			matches = append(matches, m)
			if limit > 0 && len(matches) >= limit {
				return matches, rows.Err()
			}
		}
	}
	return matches, rows.Err()
}

// SplitAlternatives splits a literal search pattern on top-level `|`
// into alternatives: each segment is whitespace-trimmed, and empty or
// punctuation-only segments are dropped, so `A||B` behaves as `A|B` and a
// punctuation-only pattern yields no alternatives. Within an alternative,
// whitespace-separated terms keep their AND semantics; alternatives are OR.
// No other regex grammar is parsed — a plain split is the whole rule.
func SplitAlternatives(pattern string) []string {
	out := make([]string, 0, 2)
	for alt := range strings.SplitSeq(pattern, "|") {
		alt = strings.TrimSpace(alt)
		if alt == "" || isPunctuationOnly(alt) {
			continue
		}
		out = append(out, alt)
	}
	return out
}

// substringTerms lowercases and filters the term list: empty and
// punctuation-only terms (no letter or digit) are dropped, mirroring how
// sanitizeFTSQuery treats such tokens.
func substringTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || isPunctuationOnly(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// substringAlternatives normalizes a list of alternatives: each term list is
// lowercased and filtered via substringTerms, and alternatives left without
// any word-bearing term are dropped.
func substringAlternatives(alternatives [][]string) [][]string {
	out := make([][]string, 0, len(alternatives))
	for _, terms := range alternatives {
		terms = substringTerms(terms)
		if len(terms) == 0 {
			continue
		}
		out = append(out, terms)
	}
	return out
}

// matchesAnyAlternative reports whether every term of at least one
// alternative occurs somewhere in the case-folded content.
func matchesAnyAlternative(content string, alternatives [][]string) bool {
	for _, terms := range alternatives {
		if containsAllTerms(content, terms) {
			return true
		}
	}
	return false
}

// containsAllTerms reports whether every term occurs somewhere in the
// case-folded content.
func containsAllTerms(content string, loweredTerms []string) bool {
	lower := strings.ToLower(content)
	for _, t := range loweredTerms {
		if !strings.Contains(lower, t) {
			return false
		}
	}
	return true
}

// substringLineMatcher returns a per-line predicate: the line matches when
// it contains any term of any alternative, case-insensitively.
func substringLineMatcher(alternatives [][]string) func(string) bool {
	return func(line string) bool {
		lower := strings.ToLower(line)
		for _, terms := range alternatives {
			for _, t := range terms {
				if strings.Contains(lower, t) {
					return true
				}
			}
		}
		return false
	}
}
