// Package fuse merges ranked candidate lists from multiple retrieval
// strategies into a single ranked list via Reciprocal Rank Fusion, reranks
// the fusion by term proximity, and provides Levenshtein-based query typo
// correction. All functions are pure and deterministic: every ordering has
// explicit tiebreaks so identical inputs produce identical outputs.
package fuse

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// DefaultK is the standard RRF damping constant. Larger values flatten the
// rank-weight curve; 60 is the canonical choice from the literature.
const DefaultK = 60.0

// Candidate is one line-level match produced by a retrieval strategy. The
// fields mirror store.FileMatch so conversion is a plain field copy; Score
// carries the fused (and proximity-multiplied) ranking weight and is not
// part of the match output shape.
type Candidate struct {
	FilePath      string
	Line          string
	ContextBefore string
	ContextAfter  string
	LineNumber    int
	Score         float64
}

// Fuse merges two ranked candidate lists with Reciprocal Rank Fusion:
// score(c) = sum over lists containing c of 1/(k + rank), where rank is the
// 1-based position within that list. Candidates are identified by
// (FilePath, LineNumber); a duplicate within one list contributes only its
// first occurrence. The result is sorted by fused score descending with
// deterministic tiebreaks (file path, then line number). A candidate present
// in only one list still appears — fusion widens recall, it never drops a
// single-strategy hit. A candidate ranked in both lists outranks
// single-strategy candidates of comparable rank because its contributions
// add. k <= 0 falls back to DefaultK.
func Fuse(a, b []Candidate, k float64) []Candidate {
	if k <= 0 {
		k = DefaultK
	}
	scores := make(map[string]float64)
	first := make(map[string]Candidate)
	add := func(list []Candidate) {
		seen := make(map[string]bool, len(list))
		for i, c := range list {
			kk := c.FilePath + "\x00" + strconv.Itoa(c.LineNumber)
			if seen[kk] {
				continue
			}
			seen[kk] = true
			scores[kk] += 1.0 / (k + float64(i+1))
			if _, ok := first[kk]; !ok {
				first[kk] = c
			}
		}
	}
	add(a)
	add(b)

	out := make([]Candidate, 0, len(first))
	for kk, c := range first {
		c.Score = scores[kk]
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return lessCandidate(out[i], out[j]) })
	return out
}

// lessCandidate orders candidates by fused score descending, then file path
// and line number ascending, giving a total order and therefore a stable
// ranking across runs regardless of map iteration order.
func lessCandidate(a, b Candidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.FilePath != b.FilePath {
		return a.FilePath < b.FilePath
	}
	return a.LineNumber < b.LineNumber
}

const (
	// proximityWeight scales the proximity bonus: multiplier = 1 + w/window.
	proximityWeight = 1.0
	// proximityCap bounds the proximity multiplier so a single-line match
	// can at most double a candidate's fused score.
	proximityCap = 2.0
)

// RerankByProximity reorders fused results so candidates from files whose
// query terms occur close together rank above candidates from files where
// the terms are scattered. For each file it computes the minimum span of
// lines that covers at least one occurrence of every term (over the matched
// lines present in the results) and multiplies each candidate's score by
// min(proximityCap, 1 + proximityWeight/window); a file that never covers
// every term gets no boost. Queries with fewer than two word-bearing terms
// leave the results unchanged. The output is re-sorted with the same
// deterministic tiebreaks as Fuse, so reranking reorders entries without
// adding or dropping any.
func RerankByProximity(results []Candidate, terms []string) []Candidate {
	if len(WordTerms(terms)) < 2 || len(results) < 2 {
		return results
	}
	return RerankByAlternatives(results, [][]string{terms})
}

// RerankByAlternatives is RerankByProximity over per-alternative term lists:
// a file is boosted by the alternative that matched it best — the smallest
// positive minimum-window across alternatives — so an alternation query
// reranks against the alternative that actually matched. Alternatives with
// fewer than two word-bearing terms produce no window of their own; a file
// matching none of the multi-term alternatives gets no boost. The output is
// re-sorted with the same deterministic tiebreaks as Fuse.
func RerankByAlternatives(results []Candidate, alternatives [][]string) []Candidate {
	if len(results) < 2 {
		return results
	}
	norms := multiTermAlternatives(alternatives)
	if len(norms) == 0 {
		return results
	}

	termLines := alternativeTermLines(results, norms)
	out := make([]Candidate, len(results))
	copy(out, results)
	for i := range out {
		w := bestAlternativeWindow(termLines[out[i].FilePath], norms)
		if w <= 0 {
			continue
		}
		mult := 1.0 + proximityWeight/float64(w)
		if mult > proximityCap {
			mult = proximityCap
		}
		out[i].Score *= mult
	}
	sort.Slice(out, func(i, j int) bool { return lessCandidate(out[i], out[j]) })
	return out
}

// multiTermAlternatives normalizes each alternative via WordTerms and keeps
// only the ones with at least two word-bearing terms — the only alternatives
// that can produce a proximity window.
func multiTermAlternatives(alternatives [][]string) [][]string {
	norms := make([][]string, 0, len(alternatives))
	for _, terms := range alternatives {
		if terms = WordTerms(terms); len(terms) >= 2 {
			norms = append(norms, terms)
		}
	}
	return norms
}

// alternativeTermLines maps file path -> term -> matched line numbers over
// the matched lines present in the results, for every term of every
// alternative term list.
func alternativeTermLines(results []Candidate, alternatives [][]string) map[string]map[string]map[int]bool {
	termLines := make(map[string]map[string]map[int]bool)
	for _, c := range results {
		byTerm := termLines[c.FilePath]
		if byTerm == nil {
			byTerm = make(map[string]map[int]bool)
			termLines[c.FilePath] = byTerm
		}
		lower := strings.ToLower(c.Line)
		for _, terms := range alternatives {
			for _, t := range terms {
				if strings.Contains(lower, t) {
					if byTerm[t] == nil {
						byTerm[t] = make(map[int]bool)
					}
					byTerm[t][c.LineNumber] = true
				}
			}
		}
	}
	return termLines
}

// bestAlternativeWindow returns the smallest positive minimum-window across
// the alternative term lists for one file's term-occurrence map — the span
// of the alternative that matched the file best — or 0 when no alternative
// covers all of its terms.
func bestAlternativeWindow(byTerm map[string]map[int]bool, alternatives [][]string) int {
	best := 0
	for _, terms := range alternatives {
		if w := minTermWindow(byTerm, terms); w > 0 && (best == 0 || w < best) {
			best = w
		}
	}
	return best
}

// minTermWindow returns the smallest span of lines b-a+1 covering at least
// one occurrence of every term, or 0 when some term never occurs. The
// sliding window runs over the merged per-term occurrence events, so the
// cost is linear in the number of events.
func minTermWindow(byTerm map[string]map[int]bool, terms []string) int {
	type event struct {
		line int
		term int
	}
	events := make([]event, 0, len(terms)*4)
	for idx, t := range terms {
		lines := byTerm[t]
		if len(lines) == 0 {
			return 0
		}
		for line := range lines {
			events = append(events, event{line, idx})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].line != events[j].line {
			return events[i].line < events[j].line
		}
		return events[i].term < events[j].term
	})

	have := make([]int, len(terms))
	covered := 0
	best := math.MaxInt
	for start, end := 0, 0; end < len(events); end++ {
		if have[events[end].term] == 0 {
			covered++
		}
		have[events[end].term]++
		for covered == len(terms) {
			if span := events[end].line - events[start].line + 1; span < best {
				best = span
			}
			have[events[start].term]--
			if have[events[start].term] == 0 {
				covered--
			}
			start++
		}
	}
	if best == math.MaxInt {
		return 0
	}
	return best
}

// Correction records one query term replaced during the correction pass.
type Correction struct {
	Original  string
	Corrected string
}

// minCorrectionLen is the shortest term eligible for correction. Shorter
// tokens sit below the FTS trigram floor and have too many near neighbors
// in a symbol lexicon to correct reliably.
const minCorrectionLen = 3

// CorrectTerms applies Levenshtein typo correction to query terms against a
// lexicon of known identifiers. A term is corrected only when all of the
// following hold: it carries at least one letter or digit, its lowercase
// form is at least minCorrectionLen characters, it is not already in the
// lexicon, and exactly one lexicon entry lies within maxDist edits — a tie
// between candidates is ambiguous and leaves the term as-is. The returned
// term list preserves input order and replaces only corrected terms; the
// corrections list records each replacement in order.
func CorrectTerms(lexicon, terms []string, maxDist int) ([]string, []Correction) {
	if maxDist < 0 {
		maxDist = 0
	}
	dict := make(map[string]bool, len(lexicon))
	entries := make([]string, 0, len(lexicon))
	for _, e := range lexicon {
		e = strings.ToLower(e)
		if e == "" || dict[e] {
			continue
		}
		dict[e] = true
		entries = append(entries, e)
	}

	out := make([]string, len(terms))
	var applied []Correction
	for i, term := range terms {
		corrected, fixed := correctTerm(entries, dict, term, maxDist)
		if fixed {
			applied = append(applied, Correction{Original: term, Corrected: corrected})
		}
		out[i] = corrected
	}
	return out, applied
}

// correctTerm returns the lexicon replacement for term when exactly one
// lexicon entry lies within maxDist edits, plus whether a correction was
// made. A term is eligible only when maxDist is positive, it carries at
// least one letter or digit, its lowercase form is at least
// minCorrectionLen characters, and it is not already in the lexicon; a tie
// between candidates is ambiguous and leaves the term as-is.
func correctTerm(entries []string, dict map[string]bool, term string, maxDist int) (string, bool) {
	if maxDist <= 0 || !wordy(term) {
		return term, false
	}
	lower := strings.ToLower(term)
	if len([]rune(lower)) < minCorrectionLen || dict[lower] {
		return term, false
	}
	best, bestDist, ties := "", maxDist+1, 0
	for _, e := range entries {
		if absLenDiff(lower, e) > maxDist {
			continue
		}
		d := levenshtein(lower, e)
		if d > maxDist {
			continue
		}
		if d < bestDist {
			best, bestDist, ties = e, d, 1
			continue
		}
		if d == bestDist {
			ties++
		}
	}
	if bestDist <= maxDist && ties == 1 {
		return best, true
	}
	return term, false
}

// levenshtein computes the edit distance between a and b with the standard
// two-row dynamic program.
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// wordy reports whether s contains at least one letter or digit.
func wordy(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// WordTerms lowercases and filters a term list: empty and punctuation-only
// terms (no letter or digit) are dropped, matching how the FTS sanitizer
// treats such tokens.
func WordTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || !wordy(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func absLenDiff(a, b string) int {
	ar, br := len([]rune(a)), len([]rune(b))
	if ar > br {
		return ar - br
	}
	return br - ar
}
