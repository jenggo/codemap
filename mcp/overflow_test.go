package mcp

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"codemap/chunk"
)

var responseIDRe = regexp.MustCompile(`resp-[0-9]+-[0-9a-f]{16}`)

// oversizedPayload builds a rendered response comfortably above any small
// test limit, with a unique token buried in the last section.
func oversizedPayload(token string) string {
	var b strings.Builder
	b.WriteString("packages[2]:\n")
	for i := range 120 {
		fmt.Fprintf(&b, "  - QualifiedName: codemap/testdata/simple.Sym%d\n    Kind: function\n", i)
	}
	b.WriteString("summary:\n  note: " + token + "\n")
	return b.String()
}

func TestGateResponseSmallPassthroughByteIdentical(t *testing.T) {
	s := newTestServer(t)
	st, err := s.getStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	direct, _, _ := queryHandlers()["health"](st, nil, nil, map[string]any{})

	gated := s.gateResponse("health", direct)
	if gated != direct {
		t.Errorf("small response changed by gate:\n got: %q\nwant: %q", gated, direct)
	}
}

func TestGateResponseOversizedBecomesManifestAndStoresSections(t *testing.T) {
	s := newTestServer(t)
	t.Setenv(responseLimitEnv, "512")

	payload := oversizedPayload("uniqueDeepToken99")
	manifest := s.gateResponse("overview", payload)

	if !strings.Contains(manifest, "response_id: resp-") {
		t.Fatalf("manifest lacks response_id:\n%s", manifest)
	}
	if !strings.Contains(manifest, "tool: overview") {
		t.Errorf("manifest lacks tool name:\n%s", manifest)
	}
	if !strings.Contains(manifest, fmt.Sprintf("total_bytes: %d", len(payload))) {
		t.Errorf("manifest lacks total byte count:\n%s", manifest)
	}
	if strings.Contains(manifest, payload[:64]) {
		t.Errorf("manifest leaks raw oversized payload")
	}

	id := responseIDRe.FindString(manifest)
	if id == "" {
		t.Fatalf("no response_id in manifest:\n%s", manifest)
	}

	st, err := s.getStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sections, err := st.ResponseSections(id, nil)
	if err != nil {
		t.Fatalf("stored sections: %v", err)
	}
	var b strings.Builder
	for _, sec := range sections {
		b.WriteString(sec.Content)
	}
	if b.String() != payload {
		t.Errorf("stored sections do not reproduce the payload")
	}
}

func TestGateResponseViaHandleToolNotMarkedError(t *testing.T) {
	s := newTestServer(t)
	t.Setenv(responseLimitEnv, "512")

	// The full symbol inventory of the tiny testdata repo still exceeds 512 bytes.
	result, _, isErr := s.handleTool("overview", map[string]any{"symbols": true})
	if isErr {
		t.Fatalf("gated response flagged as tool error: %s", result)
	}
	if !strings.Contains(result, "read_response_section") {
		t.Errorf("manifest lacks read-back instructions:\n%s", result)
	}
}

func TestReadResponseSectionByIndex(t *testing.T) {
	s := newTestServer(t)
	st, err := s.getStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sections := []chunk.Section{
		{Title: "packages", Content: strings.Repeat("first section content\n", 60)},
		{Title: "summary", Content: strings.Repeat("second section content\n", 60)},
	}
	id, err := st.SaveResponseChunks("overview", sections)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	result, _, isErr := s.handleTool(responseSectionTool, map[string]any{
		"response_id": id,
		"section":     float64(1),
	})
	if isErr {
		t.Fatalf("index read failed: %s", result)
	}
	if !strings.Contains(result, "section 1: summary") ||
		!strings.Contains(result, sections[1].Content) {
		t.Errorf("index read result = %q", result)
	}

	// Out-of-range index names the valid range.
	result, _, isErr = s.handleTool(responseSectionTool, map[string]any{
		"response_id": id,
		"section":     float64(7),
	})
	if !isErr || !strings.Contains(result, "has no section 7") {
		t.Errorf("out-of-range read = (%q, %v)", result, isErr)
	}
}

func TestReadResponseSectionByKeyword(t *testing.T) {
	s := newTestServer(t)
	st, err := s.getStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	id, err := st.SaveResponseChunks("overview", []chunk.Section{
		{Title: "head", Content: strings.Repeat("nothing interesting here\n", 60)},
		{Title: "tail", Content: strings.Repeat("filler\n", 200) + "needleTokenInsideDeepSection\n"},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	result, _, isErr := s.handleTool(responseSectionTool, map[string]any{
		"response_id": id,
		"query":       "needleTokenInsideDeepSection",
	})
	if isErr {
		t.Fatalf("keyword search failed: %s", result)
	}
	if !strings.Contains(result, "#1 [1] tail") {
		t.Errorf("keyword result lacks ranked hit:\n%s", result)
	}

	// No match reports cleanly without an error flag.
	result, _, isErr = s.handleTool(responseSectionTool, map[string]any{
		"response_id": id,
		"query":       "zzzNoMatchzzz",
	})
	if isErr || !strings.Contains(result, "No sections") {
		t.Errorf("no-match result = (%q, %v)", result, isErr)
	}
}

func TestReadResponseSectionUnknownID(t *testing.T) {
	s := newTestServer(t)
	result, _, isErr := s.handleTool(responseSectionTool, map[string]any{
		"response_id": "resp-00000000000000000000-0000000000000000",
	})
	if !isErr {
		t.Fatalf("unknown id not flagged as error: %s", result)
	}
	if !strings.Contains(result, "unknown response_id") {
		t.Errorf("error text = %q, want unknown response_id distinction", result)
	}

	result, _, isErr = s.handleTool(responseSectionTool, map[string]any{})
	if !isErr || !strings.Contains(result, "response_id is required") {
		t.Errorf("missing id result = (%q, %v)", result, isErr)
	}
}

func TestResponseLimitEnvOverride(t *testing.T) {
	t.Setenv(responseLimitEnv, "4096")
	if got := responseLimit(); got != 4096 {
		t.Errorf("responseLimit() = %d, want 4096", got)
	}

	t.Setenv(responseLimitEnv, "not-a-number")
	if got := responseLimit(); got != defaultResponseLimit {
		t.Errorf("invalid override: responseLimit() = %d, want default", got)
	}

	t.Setenv(responseLimitEnv, "-1")
	if got := responseLimit(); got != defaultResponseLimit {
		t.Errorf("negative override: responseLimit() = %d, want default", got)
	}

	t.Setenv(responseLimitEnv, "")
	if got := responseLimit(); got != defaultResponseLimit {
		t.Errorf("empty override: responseLimit() = %d, want default", got)
	}
}

func TestGateRespectsEnvOverrideForStorageDecision(t *testing.T) {
	s := newTestServer(t)

	payload := oversizedPayload("token")
	// Default limit: the payload is far below 16 KiB and passes through.
	if got := s.gateResponse("overview", payload); got != payload {
		t.Errorf("payload below default limit was gated")
	}

	// Raised limit via env keeps larger payloads ungated.
	t.Setenv(responseLimitEnv, fmt.Sprintf("%d", len(payload)+1))
	if got := s.gateResponse("overview", payload); got != payload {
		t.Errorf("payload below env limit was gated")
	}

	// Lower limit triggers the manifest path.
	t.Setenv(responseLimitEnv, "512")
	if got := s.gateResponse("overview", payload); !strings.Contains(got, "response_id: resp-") {
		t.Errorf("payload above env limit was not gated:\n%s", got)
	}
}

func TestToolsListIncludesReadResponseSection(t *testing.T) {
	for _, tool := range buildToolsList() {
		if tool["name"] == responseSectionTool {
			return
		}
	}
	t.Errorf("tools list lacks %s", responseSectionTool)
}

func TestGateManifestUnknownToolWithStoreError(t *testing.T) {
	// A store that cannot be obtained: indexing is stubbed to fail, so the
	// gate degrades to an explicit error instead of silently returning the
	// oversized payload.
	s := &Server{
		dbPath:             t.TempDir() + "/missing.db",
		repoPath:           t.TempDir(),
		failedIndexes:      make(map[string]failedIndex),
		failedIndexBackoff: defaultFailedIndexBackoff,
	}
	s.indexFn = func(string) error { return errors.New("no index available") }
	t.Setenv(responseLimitEnv, "512")
	out := s.gateResponse("overview", oversizedPayload("token"))
	if !strings.HasPrefix(out, "Error:") {
		t.Errorf("gate without a store returned:\n%.80s", out)
	}
}

func TestReadAllSectionsConcatenation(t *testing.T) {
	s := newTestServer(t)
	st, err := s.getStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sections := []chunk.Section{
		{Title: "a", Content: strings.Repeat("alpha\n", 60)},
		{Title: "b", Content: strings.Repeat("beta\n", 60)},
	}
	id, err := st.SaveResponseChunks("tool", sections)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	result, _, isErr := s.handleTool(responseSectionTool, map[string]any{"response_id": id})
	if isErr {
		t.Fatalf("read all failed: %s", result)
	}
	for _, sec := range sections {
		if !strings.Contains(result, sec.Content) {
			t.Errorf("read-all result lacks section %q content", sec.Title)
		}
	}
}

// TestResponseSectionsStoreRoundTripThroughGate verifies the full loop:
// gate stores the payload, manifest points at it, and the store layer
// surfaces ErrUnknownResponse only for genuinely unknown ids.
func TestResponseSectionsStoreRoundTripThroughGate(t *testing.T) {
	s := newTestServer(t)
	t.Setenv(responseLimitEnv, "512")

	payload := oversizedPayload("roundTripToken7")
	manifest := s.gateResponse("overview", payload)
	id := responseIDRe.FindString(manifest)

	st, err := s.getStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := st.ResponseManifest(id); err != nil {
		t.Fatalf("manifest lookup: %v", err)
	}
	if _, err := st.ResponseManifest("resp-not-real"); !isUnknownResponseErr(err) {
		t.Errorf("unknown manifest err = %v, want ErrUnknownResponse", err)
	}
}

func isUnknownResponseErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown response_id")
}

// TestGatedResponseCountsDeliveredBytes pins that a gated call is scored at the
// manifest the client received, not at the stored payload. Recording the
// pre-gate string made stats report ~100 KB for a ~1.5 KB response.
func TestGatedResponseCountsDeliveredBytes(t *testing.T) {
	s := newTestServer(t)
	t.Setenv(responseLimitEnv, "512")

	manifest, _, isErr := s.handleTool("overview", map[string]any{"symbols": true})
	if isErr {
		t.Fatalf("gated response flagged as tool error: %s", manifest)
	}
	if !strings.Contains(manifest, "Oversized response stored") {
		t.Fatalf("expected a manifest, got:\n%s", manifest)
	}

	snap := s.usage.snapshot()
	for _, row := range snap.Rows {
		if row.Tool != "overview" {
			continue
		}
		if row.RespBytes != int64(len(manifest)) {
			t.Errorf("response_bytes = %d, want the delivered manifest size %d", row.RespBytes, len(manifest))
		}
		return
	}
	t.Fatalf("no usage row for overview: %+v", snap.Rows)
}

// TestReadAllSectionsStaysUnderGate pins that reading a whole oversized payload
// back does not itself overflow. Concatenating every section reproduced the
// 53 KB original, so the gate replaced the read-back with a fresh manifest: the
// manifest's own "returns every section" promise produced another manifest.
func TestReadAllSectionsStaysUnderGate(t *testing.T) {
	s := newTestServer(t)
	t.Setenv(responseLimitEnv, "512")

	st, err := s.getStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sections := make([]chunk.Section, 0, 40)
	for i := range 40 {
		sections = append(sections, chunk.Section{
			Title:   fmt.Sprintf("part%d", i),
			Content: strings.Repeat(fmt.Sprintf("row %d payload\n", i), 8),
		})
	}
	id, err := st.SaveResponseChunks("overview", sections)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	result, _, isErr := s.handleTool(responseSectionTool, map[string]any{"response_id": id})
	if isErr {
		t.Fatalf("read all failed: %s", result)
	}
	if strings.Contains(result, "Oversized response stored") {
		t.Errorf("read-all re-gated its own output (%d bytes):\n%.300s", len(result), result)
	}
	if len(result) > responseLimit() {
		t.Errorf("read-all returned %d bytes, above the %d byte limit", len(result), responseLimit())
	}
	if !strings.Contains(result, "stopped after section") {
		t.Errorf("truncated read-all must say where it stopped:\n%.300s", result)
	}
	if !strings.Contains(result, `"section": N`) {
		t.Errorf("truncated read-all must name the resume call:\n%.300s", result)
	}

	// The first section is still complete: truncation drops the tail only.
	if !strings.Contains(result, sections[0].Content) {
		t.Errorf("read-all dropped the first section's content")
	}
}

// TestReadAllSectionsTruncatesOversizedFirstSection pins that reading back a
// payload whose first section alone exceeds the budget does not re-gate. The
// old path wrote section 0 whole, the result crossed the limit again, and the
// gate stored it as a second manifest — so "return every section" cost one call
// per section and never terminated in a readable payload.
func TestReadAllSectionsTruncatesOversizedFirstSection(t *testing.T) {
	t.Setenv(responseLimitEnv, "2048")
	s := newTestServer(t)
	big := strings.Repeat("line of a very large section\n", 500)
	id, err := s.store.SaveResponseChunks("overview", []chunk.Section{{Title: "part-0", Content: big}})
	if err != nil {
		t.Fatalf("save sections: %v", err)
	}

	out, _, isErr := handleReadResponseSection(s.store, nil, nil, map[string]any{"response_id": id})
	if isErr {
		t.Fatalf("read failed: %s", out)
	}
	if len(out) > 2048 {
		t.Fatalf("read-back is %d bytes, above the 2048 limit: it would be gated again", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected a truncation notice naming the section to fetch, got:\n%s", out)
	}
	if !strings.Contains(out, id) {
		t.Errorf("truncation notice must name the response id, got:\n%s", out)
	}
}
