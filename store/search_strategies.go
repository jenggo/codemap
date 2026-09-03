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
// FTS served the match and by path, rowid otherwise; the raw pattern must
// still occur verbatim on a line for that line to be returned. The
// flattened match list is capped at limit entries (limit <= 0 means
// uncapped) and each match carries contextLines of surrounding text.
func (s *Store) FileMatchesFTS(pattern, filePattern string, limit, contextLines int) ([]FileMatch, error) {
	sanitized := sanitizeFTSQuery(pattern)
	if sanitized == "" {
		return nil, nil
	}
	fullScan := ftsNeedsFullScan(pattern)

	var query string
	var args []any
	if fullScan {
		// The trigram tokenizer produces matches only for terms of at
		// least three characters; a shorter term silently matches nothing
		// in MATCH. Scan the files table directly so short patterns keep
		// returning the same verbatim matches they did under unicode61.
		query = `SELECT f.path, f.content FROM files f WHERE instr(f.content, ?) > 0`
		args = []any{pattern}
	} else {
		// The FTS MATCH is a whole-content candidate prefilter; the raw pattern
		// must still occur verbatim in the content so a row selected here always
		// yields at least one line from the per-line extractMatches walk.
		query = `SELECT f.path, f.content FROM file_content_fts fts JOIN files f ON f.rowid = fts.rowid WHERE file_content_fts MATCH ? AND instr(f.content, ?) > 0`
		args = []any{sanitized, pattern}
	}

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

// FileMatchesSubstring returns candidates by scanning indexed file contents
// case-insensitively: a file matches when every word-bearing term occurs
// somewhere in its content, and every line containing at least one term
// becomes a candidate, so terms scattered across lines are still recalled
// even when no single line holds the whole pattern. Punctuation-only terms
// are ignored. Files are ranked by match count descending (ties keep path
// order), lines within a file in ascending order; the flattened list is
// capped at limit entries (limit <= 0 means uncapped).
func (s *Store) FileMatchesSubstring(terms []string, filePattern string, limit, contextLines int) ([]FileMatch, error) {
	terms = substringTerms(terms)
	if len(terms) == 0 {
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
		if !containsAllTerms(content, terms) {
			continue
		}
		ms := extractMatchesFunc(filePath, content, substringLineMatcher(terms), contextLines)
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
// it contains any term, case-insensitively.
func substringLineMatcher(loweredTerms []string) func(string) bool {
	return func(line string) bool {
		lower := strings.ToLower(line)
		for _, t := range loweredTerms {
			if strings.Contains(lower, t) {
				return true
			}
		}
		return false
	}
}
