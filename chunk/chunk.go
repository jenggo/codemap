// Package chunk splits rendered tool responses into sections so an oversized
// payload can be stored and returned as a searchable manifest instead of
// being truncated client-side.
package chunk

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// MaxChunk is the maximum size of one section, in bytes. A section above
	// this size only occurs as a single oversized fallback section for a
	// payload that cannot be split further (no structural boundary and no
	// line breaks).
	MaxChunk = 8 * 1024

	// MinChunk is the minimum desirable section size, in bytes. Adjacent
	// sections below it are merged until they reach it or merging would
	// exceed MaxChunk.
	MinChunk = 512
)

// Section is one contiguous slice of a rendered response. Concatenating every
// section's Content reproduces the input exactly.
type Section struct {
	Title   string
	Content string
}

// toonKeyRe matches a TOON top-level key line such as "packages[3]:" or
// "summary:". TOON nests everything else with indentation, so a column-0 key
// always starts a new section. Text renderings share the "Label: value" shape
// for their own top-level sections, so the same rule serves both.
var toonKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*(\[\d+\])?:`)

// RenderedSections splits s into sections. Structural boundaries are used
// when they yield sections no larger than MaxChunk: JSON top-level object
// keys for JSON payloads, TOON keys and blank-line-separated blocks for
// TOON/text payloads. Otherwise s falls back to fixed ~MaxChunk byte chunks
// split at line boundaries. Sections smaller than MinChunk are merged into
// the following neighbor while the result stays within MaxChunk.
func RenderedSections(s string) []Section {
	if s == "" {
		return nil
	}
	sections := splitStructural(s)
	if sections == nil || exceedsMax(sections) {
		sections = splitFixed(s)
	}
	return mergeSmall(sections)
}

func exceedsMax(sections []Section) bool {
	for _, sec := range sections {
		if len(sec.Content) > MaxChunk {
			return true
		}
	}
	return false
}

func splitStructural(s string) []Section {
	if sections, ok := splitJSON(s); ok {
		return sections
	}
	return splitTOONOrText(s)
}

// splitTOONOrText starts a new section at each column-0 TOON key line and at
// each non-indented line that follows a blank line (the paragraph boundary
// text renderings use between top-level blocks).
func splitTOONOrText(s string) []Section {
	lines := strings.SplitAfter(s, "\n")
	var sections []Section
	var cur strings.Builder
	curTitle := ""
	prevBlank := false
	started := false

	flush := func() {
		if cur.Len() == 0 {
			return
		}
		sections = append(sections, Section{Title: curTitle, Content: cur.String()})
		cur.Reset()
	}

	for _, line := range lines {
		blank := strings.TrimSpace(line) == ""
		if started && !blank && (toonKeyRe.MatchString(line) || (prevBlank && !isIndented(line))) {
			flush()
			curTitle = sectionTitle(line)
		} else if !started {
			started = true
			curTitle = sectionTitle(line)
		}
		cur.WriteString(line)
		prevBlank = blank
	}
	flush()
	return sections
}

func isIndented(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

// sectionTitle derives a display title from a section's first line.
func sectionTitle(line string) string {
	title := strings.TrimSpace(line)
	if len(title) > 80 {
		title = truncateBytes(title, 80)
	}
	if title == "" {
		return "part"
	}
	return title
}

// truncateBytes cuts s to at most max bytes without splitting a UTF-8 rune.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			return cut
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

// splitFixed chunks s at line boundaries into pieces of at most MaxChunk
// bytes. A payload with no line breaks — or a single line longer than
// MaxChunk — yields one oversized section; it is the only case where a
// returned section may exceed MaxChunk.
func splitFixed(s string) []Section {
	lines := strings.SplitAfter(s, "\n")
	var sections []Section
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		sections = append(sections, Section{
			Title:   "part-" + strconv.Itoa(len(sections)),
			Content: cur.String(),
		})
		cur.Reset()
	}
	for _, line := range lines {
		if cur.Len() > 0 && cur.Len()+len(line) > MaxChunk {
			flush()
		}
		cur.WriteString(line)
	}
	flush()
	return sections
}

// mergeSmall folds sections smaller than MinChunk into the following
// neighbor while the combined size stays within MaxChunk, so the manifest
// does not degenerate into dozens of one-line sections.
func mergeSmall(sections []Section) []Section {
	for {
		merged := false
		out := sections[:0]
		for i := 0; i < len(sections); i++ {
			cur := sections[i]
			if len(cur.Content) < MinChunk && i+1 < len(sections) &&
				len(cur.Content)+len(sections[i+1].Content) <= MaxChunk {
				sections[i+1] = Section{
					Title:   sections[i+1].Title,
					Content: cur.Content + sections[i+1].Content,
				}
				merged = true
				continue
			}
			out = append(out, cur)
		}
		sections = out
		if !merged {
			return sections
		}
	}
}

// jsonMember is one top-level object key and the byte offset of its opening
// quote in the payload.
type jsonMember struct {
	name   string
	offset int
}

// splitJSON splits a JSON object payload on its top-level keys, preserving
// key order. The first section additionally carries the opening brace and
// the last one the closing brace, so the split stays lossless. It reports
// ok=false when s is not a JSON object; the caller then tries other rules.
func splitJSON(s string) ([]Section, bool) {
	members, ok := topLevelJSONKeys(s)
	if !ok || len(members) == 0 {
		return nil, false
	}
	sections := make([]Section, 0, len(members))
	for i, m := range members {
		start := m.offset
		if i == 0 {
			start = 0
		}
		end := len(s)
		if i+1 < len(members) {
			end = members[i+1].offset
		}
		sections = append(sections, Section{Title: m.name, Content: s[start:end]})
	}
	return sections, true
}

// topLevelJSONKeys walks a JSON object with a minimal scanner and returns its
// top-level keys in order, tracking strings and escapes so braces inside
// values never confuse the depth count.
func topLevelJSONKeys(s string) ([]jsonMember, bool) {
	i := skipJSONWS(s, 0)
	if i >= len(s) || s[i] != '{' {
		return nil, false
	}
	i = skipJSONWS(s, i+1)

	var members []jsonMember
	for {
		if i >= len(s) {
			return nil, false
		}
		if s[i] == '}' {
			return members, true
		}
		if s[i] != '"' {
			return nil, false
		}
		start := i
		name, next, ok := scanJSONString(s, i)
		if !ok {
			return nil, false
		}
		i = skipJSONWS(s, next)
		if i >= len(s) || s[i] != ':' {
			return nil, false
		}
		vEnd, ok := scanJSONValue(s, i+1)
		if !ok {
			return nil, false
		}
		members = append(members, jsonMember{name: name, offset: start})
		i = skipJSONWS(s, vEnd)
		if i < len(s) && s[i] == ',' {
			i = skipJSONWS(s, i+1)
		}
	}
}

func skipJSONWS(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// scanJSONString scans the JSON string starting at s[i] == '"' and returns
// the decoded value and the index just past the closing quote.
func scanJSONString(s string, i int) (string, int, bool) {
	if i >= len(s) || s[i] != '"' {
		return "", i, false
	}
	var b strings.Builder
	i++
	for i < len(s) {
		c := s[i]
		switch c {
		case '"':
			return b.String(), i + 1, true
		case '\\':
			if i+1 >= len(s) {
				return "", i, false
			}
			esc := s[i+1]
			i += 2
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if i+4 > len(s) {
					return "", i, false
				}
				v, err := strconv.ParseUint(s[i:i+4], 16, 32)
				if err != nil {
					return "", i, false
				}
				b.WriteRune(rune(v))
				i += 4
			default:
				return "", i, false
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", i, false
}

// scanJSONValue returns the index just past the JSON value starting at s[i].
func scanJSONValue(s string, i int) (int, bool) {
	i = skipJSONWS(s, i)
	if i >= len(s) {
		return i, false
	}
	switch s[i] {
	case '"':
		_, next, ok := scanJSONString(s, i)
		return next, ok
	case '{', '[':
		depth := 0
		for i < len(s) {
			switch s[i] {
			case '"':
				_, next, ok := scanJSONString(s, i)
				if !ok {
					return i, false
				}
				i = next
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return i, false
	default:
		// Number, true, false, null: runs until a structural character.
		start := i
		for i < len(s) {
			c := s[i]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				break
			}
			i++
		}
		if i == start {
			return i, false
		}
		return i, true
	}
}
