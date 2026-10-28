package mcp

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"codemap/chunk"
	"codemap/query"
	"codemap/render"
	"codemap/store"
)

// defaultResponseLimit caps every tool response at 16 KiB of rendered text
// before the server replaces it with a searchable manifest.
const defaultResponseLimit = 16 * 1024

// responseLimitEnv overrides defaultResponseLimit at process startup.
const responseLimitEnv = "CODEMAP_RESPONSE_LIMIT"

// manifestPreviewBytes bounds each per-section preview in a manifest.
const manifestPreviewBytes = 200

// responseSectionTool is the read-back tool for gated responses.
const responseSectionTool = "read_response_section"

// responseLimit returns the configured response byte threshold: the
// CODEMAP_RESPONSE_LIMIT value when set to a positive integer, otherwise
// defaultResponseLimit.
func responseLimit() int {
	if v := os.Getenv(responseLimitEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultResponseLimit
}

// gateResponse is the single choke point every tool response passes through:
// responses at or below the limit are returned byte-identical; larger ones
// are chunked, stored under a response_id, and replaced by a manifest that
// lets the agent pull back exactly the sections it needs.
func (s *Server) gateResponse(tool, rendered string) string {
	limit := responseLimit()
	if len(rendered) <= limit {
		return rendered
	}
	st, err := s.getStore()
	if err != nil {
		return gateStoreError(len(rendered), limit, err)
	}
	sections := chunk.RenderedSections(rendered, limit)
	if len(sections) == 0 {
		return gateStoreError(len(rendered), limit, errors.New("chunking produced no sections"))
	}
	id, err := st.SaveResponseChunks(tool, sections)
	if err != nil {
		return gateStoreError(len(rendered), limit, err)
	}
	s.usage.recordGated()
	return renderResponseManifest(id, tool, sections, len(rendered), limit)
}

func gateStoreError(size, limit int, err error) string {
	return fmt.Sprintf("Error: response is %d bytes, above the %d byte limit, and it could not be stored for section reads: %v", size, limit, err)
}

// renderResponseManifest describes a stored oversized response: the
// response_id, the tool, total bytes, and one entry per section with title,
// byte count, and a bounded preview.
func renderResponseManifest(id, tool string, sections []chunk.Section, total, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Oversized response stored instead of returned: %d bytes exceeds the %d byte limit.\n", total, limit)
	fmt.Fprintf(&b, "response_id: %s\n", id)
	fmt.Fprintf(&b, "tool: %s\n", tool)
	fmt.Fprintf(&b, "total_bytes: %d\n", total)
	fmt.Fprintf(&b, "sections: %d\n\n", len(sections))
	for i, sec := range sections {
		fmt.Fprintf(&b, "[%d] %s — %d bytes\n", i, sec.Title, len(sec.Content))
		if pv := preview(sec.Content, manifestPreviewBytes); pv != "" {
			b.WriteString(indentLines(pv, "    "))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Fetch the full content with `read_response_section`:\n"+
		"- {\"response_id\": %q, \"section\": N} returns section N verbatim\n"+
		"- {\"response_id\": %q, \"query\": \"keyword\"} returns matching sections ranked by relevance\n"+
		"- {\"response_id\": %q} returns every section in order\n", id, id, id)
	return b.String()
}

// preview cuts s to at most max bytes without splitting a UTF-8 rune,
// reserving room for the ellipsis marker when truncation happens.
func preview(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit-3]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// handleReadResponseSection returns stored content of an oversized response's
// sections: one verbatim section by index, ranked matches by keyword, or
// every section in order when neither is given.
func handleReadResponseSection(st *store.Store, _ []query.Option, _ []render.Option, args map[string]any) (string, any, bool) {
	id := requiredString(args, "response_id")
	if id == "" {
		return "Error: response_id is required", nil, true
	}

	if v, ok := args["section"].(float64); ok {
		return readResponseSectionByIndex(st, id, int(v))
	}

	if q := requiredString(args, "query"); q != "" {
		limit := 5
		if v, ok := args["limit"].(float64); ok {
			limit = int(v)
		}
		return searchResponseSections(st, id, q, limit)
	}

	return readAllResponseSections(st, id)
}

func readResponseSectionByIndex(st *store.Store, id string, idx int) (string, any, bool) {
	sections, err := st.ResponseSections(id, &idx)
	if err != nil {
		if errors.Is(err, store.ErrUnknownResponse) {
			return unknownResponseError(id), nil, true
		}
		return fmt.Sprintf("Error: %v", err), nil, true
	}
	if len(sections) == 0 {
		manifest, mErr := st.ResponseManifest(id)
		if mErr != nil {
			return fmt.Sprintf("Error: %v", mErr), nil, true
		}
		return fmt.Sprintf("Error: response %s has no section %d (it has %d sections, indexes 0-%d)", id, idx, manifest.Sections, manifest.Sections-1), nil, true
	}
	sec := sections[0]
	var b strings.Builder
	fmt.Fprintf(&b, "section %d: %s — %d bytes\n\n", sec.SectionIdx, sec.Title, sec.ByteCount)
	b.WriteString(sec.Content)
	return b.String(), nil, false
}
func searchResponseSections(st *store.Store, id, query string, limit int) (string, any, bool) {
	hits, err := st.SearchResponseSections(id, query, limit)
	if err != nil {
		if errors.Is(err, store.ErrUnknownResponse) {
			return unknownResponseError(id), nil, true
		}
		return fmt.Sprintf("Error: %v", err), nil, true
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No sections of response %s match %q.", id, query), nil, false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Matches for %q in response %s, ranked by relevance:\n\n", query, id)
	for i, sec := range hits {
		fmt.Fprintf(&b, "#%d [%d] %s — %d bytes (score %.2f)\n", i+1, sec.SectionIdx, sec.Title, sec.ByteCount, sec.Score)
		if pv := preview(sec.Content, manifestPreviewBytes); pv != "" {
			b.WriteString(indentLines(pv, "    "))
			b.WriteString("\n\n")
		}
	}
	fmt.Fprintf(&b, "Read a full section with {\"response_id\": %q, \"section\": <index>}.", id)
	return b.String(), nil, false
}

// readAllResponseSections returns every section in order, stopping once the
// output would exceed the response limit. Concatenating a whole oversized
// payload reproduced it above the limit, so the gate stored the read-back as a
// new manifest: the manifest's own "returns every section" promise produced
// another manifest, and reading a large result needed one call per section.
// The tail is named instead of gated.
func readAllResponseSections(st *store.Store, id string) (string, any, bool) {
	sections, err := st.ResponseSections(id, nil)
	if err != nil {
		if errors.Is(err, store.ErrUnknownResponse) {
			return unknownResponseError(id), nil, true
		}
		return fmt.Sprintf("Error: %v", err), nil, true
	}
	limit := responseLimit()
	// Measure the stop notice before spending the budget, so appending it
	// cannot push the result back over the limit the notice is there to respect.
	notice := stopNotice(0, len(sections), limit, id)
	budget := limit - len(notice)
	var b strings.Builder
	for i, sec := range sections {
		header := fmt.Sprintf("section %d: %s — %d bytes\n\n", sec.SectionIdx, sec.Title, sec.ByteCount)
		if b.Len()+len(header)+len(sec.Content) > budget {
			if i == 0 {
				// The first section alone exceeds the budget. Writing it whole
				// reproduced an oversized payload, which the gate then stored as
				// a new manifest — reading a large result cost one call per
				// section. Truncate it instead and name the section to fetch.
				b.WriteString(header)
				b.WriteString(truncateUTF8(sec.Content, budget-b.Len()))
				fmt.Fprintf(&b, "\n… truncated; read the whole section with {\"response_id\": %q, \"section\": %d}.\n", id, sec.SectionIdx)
				return b.String(), nil, false
			}
			b.WriteString(stopNotice(i-1, len(sections), limit, id))
			break
		}
		b.WriteString(header)
		b.WriteString(sec.Content)
		b.WriteString("\n\n")
	}
	return b.String(), nil, false
}

// truncateUTF8 cuts s to at most limit bytes without splitting a rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:max(limit, 0)]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

// stopNotice names the last section included and the call that resumes.
func stopNotice(last, total, limit int, id string) string {
	return fmt.Sprintf("… stopped after section %d of %d to stay within the %d byte response limit. "+
		"Read the rest with {\"response_id\": %q, \"section\": N} or narrow it with {\"query\": \"keyword\"}.\n",
		last, total, limit, id)
}

func unknownResponseError(id string) string {
	return fmt.Sprintf("Error: unknown response_id %s — it was never stored or has been pruned (overflow data is pruned on re-index). Re-run the original tool to regenerate it.", id)
}

// overflowTools registers the read-back tool for gated responses.
func overflowTools() []map[string]any {
	return []map[string]any{
		toolDef(responseSectionTool,
			"Reads back sections of an oversized response replaced by a manifest: one by index, keyword-matched, or all in order.",
			map[string]any{
				"response_id": stringProp("response_id from the manifest"),
				"section":     intProp("0-based section index to return verbatim"),
				"query":       stringProp("Keyword query for ranked section search"),
				"limit":       intProp("Max sections in query mode (default 5)"),
			},
			"response_id"),
	}
}
