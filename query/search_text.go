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
	matches, _, err := SearchTextWithCorrections(s, pattern, filePattern, isRegex, contextLines)
	return matches, err
}

// SearchTextWithCorrections is SearchText with the correction pass surfaced:
// the second return value reports the term replacements applied during the
// single re-run, and is nil whenever no correction ran.
func SearchTextWithCorrections(s *store.Store, pattern, filePattern string, isRegex bool, contextLines int) ([]store.FileMatch, []fuse.Correction, error) {
	if isRegex {
		matches, err := s.SearchFileContent(pattern, filePattern, true, contextLines)
		return matches, nil, err
	}

	matches, err := runFusedTextSearch(s, pattern, filePattern, contextLines)
	if err != nil {
		return nil, nil, err
	}
	if len(matches) >= resultFloor {
		return matches, nil, nil
	}

	// Below the floor: correct misspelled terms against the indexed symbol
	// lexicon and re-run the full fused pipeline exactly once.
	terms := strings.Fields(pattern)
	lexicon, err := s.SearchLexicon()
	if err != nil {
		return nil, nil, fmt.Errorf("search lexicon: %w", err)
	}
	corrected, applied := fuse.CorrectTerms(lexicon, terms, maxCorrectionDist)
	if len(applied) == 0 {
		return matches, nil, nil
	}
	correctedMatches, err := runFusedTextSearch(s, strings.Join(corrected, " "), filePattern, contextLines)
	if err != nil {
		return nil, nil, err
	}
	return correctedMatches, applied, nil
}

// runFusedTextSearch executes one pass of the fused pipeline: both
// retrieval strategies, RRF fusion, and proximity reranking. The output
// carries the FileMatch shape unchanged.
func runFusedTextSearch(s *store.Store, pattern, filePattern string, contextLines int) ([]store.FileMatch, error) {
	fts, err := s.FileMatchesFTS(pattern, filePattern, strategyLimit, contextLines)
	if err != nil {
		return nil, err
	}

	var sub []store.FileMatch
	terms := strings.Fields(pattern)
	if len(terms) <= maxScanTerms {
		sub, err = s.FileMatchesSubstring(terms, filePattern, strategyLimit, contextLines)
		if err != nil {
			return nil, err
		}
	}

	fused := fuse.RerankByProximity(fuse.Fuse(toCandidates(fts), toCandidates(sub), fuseK), terms)
	return fromCandidates(fused), nil
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
