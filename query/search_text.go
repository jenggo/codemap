package query

import (
	"fmt"
	"strings"

	"codemap/fuse"
	"codemap/store"
)

// Tuning constants for the fused text search pipeline.
const (
	// fuseK is the RRF damping constant passed to fuse.Fuse.
	fuseK = fuse.DefaultK
	// strategyLimit caps each strategy's ranked candidate list before fusion.
	strategyLimit = 100
	// resultFloor is the minimum fused result count below which the
	// correction pass runs.
	resultFloor = 1
	// maxScanTerms bounds the O(total bytes) substring scan; broader term
	// sets skip the scan and rely on the FTS pass alone.
	maxScanTerms = 4
	// maxCorrectionDist is the Levenshtein budget for term correction.
	maxCorrectionDist = 2
)

// SearchText runs the search_text pipeline: regex mode dispatches to the
// original store path untouched, while FTS mode fuses the ranked FTS5
// trigram pass with a case-insensitive substring scan, reranks by term
// proximity, and applies one Levenshtein correction pass when the fused
// result count falls below the floor.
func SearchText(s *store.Store, pattern, filePattern string, isRegex bool, contextLines int) ([]store.FileMatch, error) {
	matches, _, _, err := SearchTextWithCorrections(s, pattern, filePattern, isRegex, contextLines)
	return matches, err
}

// SearchTextWithCorrections is SearchText with the correction pass surfaced:
// the second return value reports the term replacements applied during the
// single re-run (or, when results already exist, the replacements suggested
// for alternatives that matched nothing), and is nil whenever no correction
// ran. The third return value is a hint for empty literal results on
// patterns containing regex metacharacters, empty otherwise.
func SearchTextWithCorrections(s *store.Store, pattern, filePattern string, isRegex bool, contextLines int) ([]store.FileMatch, []fuse.Correction, string, error) {
	if isRegex {
		matches, err := s.SearchFileContent(pattern, filePattern, true, contextLines)
		return matches, nil, "", err
	}

	matches, err := runFusedTextSearch(s, pattern, filePattern, contextLines)
	if err != nil {
		return nil, nil, "", err
	}
	if len(matches) >= resultFloor {
		return matches, alternationCorrections(s, pattern, matches), "", nil
	}

	// Below the floor: correct misspelled terms against the indexed symbol
	// lexicon and re-run the full fused pipeline exactly once.
	correctedAlternatives, applied, err := correctAlternatives(s, store.SplitAlternatives(pattern))
	if err != nil {
		return nil, nil, "", err
	}
	if len(applied) == 0 {
		return matches, nil, regexMetacharHint(pattern), nil
	}
	correctedMatches, err := runFusedTextSearch(s, strings.Join(correctedAlternatives, "|"), filePattern, contextLines)
	if err != nil {
		return nil, nil, "", err
	}
	if len(correctedMatches) >= resultFloor {
		return correctedMatches, applied, "", nil
	}
	return correctedMatches, applied, regexMetacharHint(pattern), nil
}

// correctAlternatives corrects the whitespace terms of every alternative
// against the symbol lexicon, returning the corrected alternatives (ready to
// be re-joined with `|` for a re-run) and the applied corrections in order.
func correctAlternatives(s *store.Store, alternatives []string) ([]string, []fuse.Correction, error) {
	lexicon, err := s.SearchLexicon()
	if err != nil {
		return nil, nil, fmt.Errorf("search lexicon: %w", err)
	}
	out := make([]string, len(alternatives))
	var applied []fuse.Correction
	for i, alt := range alternatives {
		corrected, c := fuse.CorrectTerms(lexicon, strings.Fields(alt), maxCorrectionDist)
		out[i] = strings.Join(corrected, " ")
		applied = append(applied, c...)
	}
	return out, applied, nil
}

// alternationCorrections evaluates typo corrections per alternative when the
// search already returned results: an alternative whose terms appear in no
// result line matched nothing, so its misspelled terms are corrected against
// the symbol lexicon and reported without a re-run — the matched
// alternatives already produced the results. Alternatives that matched are
// left untouched, and lexicon failures are swallowed: corrections are
// best-effort metadata on an otherwise valid result set.
func alternationCorrections(s *store.Store, pattern string, matches []store.FileMatch) []fuse.Correction {
	alternatives := substringAlternativesTerms(pattern)
	var unmatched [][]string
	for _, terms := range alternatives {
		if alternativeMatched(terms, matches) {
			continue
		}
		unmatched = append(unmatched, terms)
	}
	if len(unmatched) == 0 {
		return nil
	}
	lexicon, err := s.SearchLexicon()
	if err != nil {
		return nil
	}
	var applied []fuse.Correction
	for _, terms := range unmatched {
		_, c := fuse.CorrectTerms(lexicon, terms, maxCorrectionDist)
		applied = append(applied, c...)
	}
	return applied
}

// alternativeMatched reports whether any result line contains any of the
// alternative's word-bearing terms, case-insensitively. A punctuation-only
// alternative is treated as satisfied.
func alternativeMatched(terms []string, matches []store.FileMatch) bool {
	terms = fuse.WordTerms(terms)
	if len(terms) == 0 {
		return true
	}
	for _, m := range matches {
		lower := strings.ToLower(m.Line)
		for _, t := range terms {
			if strings.Contains(lower, t) {
				return true
			}
		}
	}
	return false
}

// regexMetacharacters are the metacharacters whose presence in an empty
// literal result most likely means the caller passed grep-style syntax.
const regexMetacharacters = "|()[].*+?{"

// regexMetacharHintMsg is surfaced when literal mode matches nothing and the
// pattern carries regex metacharacters: it points the caller at regex mode
// without ever switching modes for it.
const regexMetacharHintMsg = "pattern contains regex metacharacters; literal mode matched nothing - retry with is_regex: true"

// regexMetacharHint returns the empty-result hint for patterns containing
// regex metacharacters, or "" for clean patterns.
func regexMetacharHint(pattern string) string {
	if strings.ContainsAny(pattern, regexMetacharacters) {
		return regexMetacharHintMsg
	}
	return ""
}

// runFusedTextSearch executes one pass of the fused pipeline: both
// retrieval strategies, RRF fusion, and proximity reranking against the
// alternative that matched. The output carries the FileMatch shape
// unchanged.
func runFusedTextSearch(s *store.Store, pattern, filePattern string, contextLines int) ([]store.FileMatch, error) {
	fts, err := s.FileMatchesFTS(pattern, filePattern, strategyLimit, contextLines)
	if err != nil {
		return nil, err
	}

	alternatives := substringAlternativesTerms(pattern)
	var sub []store.FileMatch
	if totalTerms(alternatives) <= maxScanTerms {
		sub, err = s.FileMatchesSubstring(alternatives, filePattern, strategyLimit, contextLines)
		if err != nil {
			return nil, err
		}
	}

	fused := fuse.RerankByAlternatives(fuse.Fuse(toCandidates(fts), toCandidates(sub), fuseK), alternatives)
	return fromCandidates(fused), nil
}

// substringAlternativesTerms splits the pattern into alternatives and their
// whitespace-separated terms, for the substring scan and per-alternative
// proximity reranking.
func substringAlternativesTerms(pattern string) [][]string {
	alts := store.SplitAlternatives(pattern)
	out := make([][]string, len(alts))
	for i, alt := range alts {
		out[i] = strings.Fields(alt)
	}
	return out
}

// totalTerms sums the term count across alternatives, bounding the O(total
// bytes) substring scan the same way the single-list term count did.
func totalTerms(alternatives [][]string) int {
	n := 0
	for _, terms := range alternatives {
		n += len(terms)
	}
	return n
}

func toCandidates(matches []store.FileMatch) []fuse.Candidate {
	if len(matches) == 0 {
		return nil
	}
	out := make([]fuse.Candidate, len(matches))
	for i, m := range matches {
		out[i] = fuse.Candidate{
			FilePath:      m.FilePath,
			LineNumber:    m.LineNumber,
			Line:          m.Line,
			ContextBefore: m.ContextBefore,
			ContextAfter:  m.ContextAfter,
		}
	}
	return out
}

func fromCandidates(candidates []fuse.Candidate) []store.FileMatch {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]store.FileMatch, len(candidates))
	for i, c := range candidates {
		out[i] = store.FileMatch{
			FilePath:      c.FilePath,
			LineNumber:    c.LineNumber,
			Line:          c.Line,
			ContextBefore: c.ContextBefore,
			ContextAfter:  c.ContextAfter,
		}
	}
	return out
}
