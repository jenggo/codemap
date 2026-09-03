package mcp

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"

	"codemap/query"
)

// parseStatsRows extracts the per-tool tabular rows of a stats report, keyed
// by tool name, so tests can assert on the rendered numbers.
func parseStatsRows(t *testing.T, out string) map[string]map[string]float64 {
	t.Helper()
	rows := map[string]map[string]float64{}
	var cols []string
	inRows := false
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "tools[") {
			start := strings.Index(line, "{")
			end := strings.LastIndex(line, "}")
			if start >= 0 && end > start {
				cols = strings.Split(line[start+1:end], ",")
			}
			inRows = true
			continue
		}
		if strings.HasPrefix(line, "totals") {
			break
		}
		if !inRows || len(cols) == 0 || !strings.HasPrefix(line, "  ") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) != len(cols) {
			t.Fatalf("row %q does not match header %v", line, cols)
		}
		row := map[string]float64{}
		toolIdx := -1
		for i, c := range cols {
			if c == "tool" {
				toolIdx = i
				continue
			}
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				t.Fatalf("row %q field %s: %v", line, c, err)
			}
			row[c] = v
		}
		if toolIdx < 0 {
			t.Fatalf("header %v lacks a tool column", cols)
		}
		rows[fields[toolIdx]] = row
	}
	return rows
}

// parseStatsTotals extracts the totals section of a stats report into a map.
func parseStatsTotals(t *testing.T, out string) map[string]float64 {
	t.Helper()
	totals := map[string]float64{}
	inTotals := false
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "totals") {
			inTotals = true
			continue
		}
		if !inTotals || !strings.HasPrefix(line, "  ") {
			continue
		}
		kv := strings.SplitN(strings.TrimSpace(line), ": ", 2)
		if len(kv) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(kv[1], 64)
		if err != nil {
			t.Fatalf("totals line %q: %v", line, err)
		}
		totals[kv[0]] = v
	}
	return totals
}

// --- 1.3 concurrency ---

func TestUsageStatsConcurrentRecordsExactTotals(t *testing.T) {
	u := newUsageStats()
	const goroutines = 16
	const perGoroutine = 250
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				u.record("load_test_tool", 10, 20, i%10 == 0)
			}
		})
	}
	wg.Wait()

	wantCalls := int64(goroutines * perGoroutine)
	snap := u.snapshot()
	if len(snap.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(snap.Rows))
	}
	row := snap.Rows[0]
	if row.Calls != wantCalls {
		t.Errorf("calls = %d, want %d (no lost updates)", row.Calls, wantCalls)
	}
	if row.Errors != wantCalls/10 {
		t.Errorf("errors = %d, want %d", row.Errors, wantCalls/10)
	}
	if row.RespBytes != wantCalls*10 {
		t.Errorf("respBytes = %d, want %d", row.RespBytes, wantCalls*10)
	}
	if row.RawBytes != wantCalls*20 {
		t.Errorf("rawBytes = %d, want %d", row.RawBytes, wantCalls*20)
	}
	if row.Reduction != 0.5 {
		t.Errorf("reduction = %v, want 0.5", row.Reduction)
	}
}

// --- 2.3 estimators ---

func TestReductionPctClampsToUnitRange(t *testing.T) {
	cases := []struct {
		resp, raw int64
		want      float64
	}{
		{50, 100, 0.5},
		{0, 100, 1},
		{100, 50, 0},  // raw < resp: clamped, no negative savings
		{200, 100, 0}, // same
		{10, 0, 0},    // no raw basis: no savings claimed
		{0, 0, 0},
	}
	for _, tc := range cases {
		if got := reductionPct(tc.resp, tc.raw); got != tc.want {
			t.Errorf("reductionPct(%d, %d) = %v, want %v", tc.resp, tc.raw, got, tc.want)
		}
	}
}

func TestEstimateSymbolBodyRawSumsSpans(t *testing.T) {
	res := &query.SymbolBodyResult{Body: "0123456789", ContextBefore: "abc", ContextAfter: "de"}
	if got := estimateSymbolBodyRaw(nil, res); got != 15 {
		t.Errorf("estimate = %d, want 15 (body + context window)", got)
	}
	// Wrong or missing typed result: no basis, zero estimate.
	if got := estimateSymbolBodyRaw(nil, "not-a-result"); got != 0 {
		t.Errorf("wrong type estimate = %d, want 0", got)
	}
	if got := estimateSymbolBodyRaw(nil, nil); got != 0 {
		t.Errorf("nil estimate = %d, want 0", got)
	}
}

func TestEstimateBundleRawUsesTokenEstimateTimes4(t *testing.T) {
	bundle := &query.Bundle{TokenEstimate: 100}
	if got := estimateBundleRaw(nil, bundle); got != 400 {
		t.Errorf("estimate = %d, want 400", got)
	}
	if got := estimateBundleRaw(nil, "not-a-bundle"); got != 0 {
		t.Errorf("wrong type estimate = %d, want 0", got)
	}
}

func TestRecordUsageIdentityFallbackForUnregisteredTool(t *testing.T) {
	s := newTestServer(t)
	s.recordUsage("overview", nil, "hello", false)
	s.recordUsage("overview", nil, "boom", true)
	snap := s.usage.snapshot()
	if len(snap.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(snap.Rows))
	}
	row := snap.Rows[0]
	if row.Tool != "overview" || row.Calls != 2 || row.Errors != 1 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.RespBytes != 9 || row.RawBytes != 9 {
		t.Errorf("identity fallback: resp=%d raw=%d, want 9/9", row.RespBytes, row.RawBytes)
	}
	if row.Reduction != 0 {
		t.Errorf("identity fallback reduction = %v, want 0", row.Reduction)
	}
}

// TestStatsRecordsSymbolBodyEstimator proves the typed handler result reaches
// the estimator through the dispatch wrapper. The fixture symbol is tiny, so
// the rendered payload (headers + fences) exceeds the raw span estimate — the
// documented exception that clamps reduction to 0.
func TestStatsRecordsSymbolBodyEstimator(t *testing.T) {
	s := newTestServer(t)
	qn := "codemap/testdata/simple.NewParser"
	out, isErr := s.handleTool("get_symbol_body", map[string]any{"qualified_name": qn})
	if isErr {
		t.Fatalf("get_symbol_body failed: %s", out)
	}
	body, err := query.GetSymbolBody(s.store, qn, 0, true)
	if err != nil {
		t.Fatalf("GetSymbolBody: %v", err)
	}
	wantRaw := int64(len(body.Body) + len(body.ContextBefore) + len(body.ContextAfter))

	snap := s.usage.snapshot()
	if len(snap.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(snap.Rows))
	}
	row := snap.Rows[0]
	if row.Tool != "get_symbol_body" || row.Calls != 1 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.RawBytes != wantRaw {
		t.Errorf("rawBytes = %d, want %d (body + context spans)", row.RawBytes, wantRaw)
	}
	if row.RespBytes != int64(len(out)) {
		t.Errorf("respBytes = %d, want %d (rendered payload)", row.RespBytes, len(out))
	}
	if row.Reduction != 0 {
		t.Errorf("reduction = %v, want 0 (rendered headers exceed raw span: clamp)", row.Reduction)
	}
}

func TestStatsRecordsPackageEstimator(t *testing.T) {
	s := newTestServer(t)
	pkgPath := "codemap/testdata/simple"
	out, isErr := s.handleTool(keyToolPackage, map[string]any{"path": pkgPath})
	if isErr {
		t.Fatalf("package failed: %s", out)
	}
	pkg, err := query.Package(s.store, pkgPath)
	if err != nil {
		t.Fatalf("query.Package: %v", err)
	}
	var wantRaw int64
	for _, sym := range pkg.ExportedSymbols {
		body, berr := query.GetSymbolBody(s.store, sym.QualifiedName, 0, false)
		if berr != nil {
			continue
		}
		wantRaw += int64(len(body.Body))
	}
	if wantRaw == 0 {
		t.Fatal("fixture package has no exported symbol bodies; test would be vacuous")
	}

	snap := s.usage.snapshot()
	if len(snap.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(snap.Rows))
	}
	row := snap.Rows[0]
	if row.Tool != keyToolPackage || row.Calls != 1 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.RawBytes != wantRaw {
		t.Errorf("rawBytes = %d, want %d (sum of exported symbol bodies)", row.RawBytes, wantRaw)
	}
}

func TestStatsRecordsContextBundleEstimator(t *testing.T) {
	s := newTestServer(t)
	qn := "codemap/testdata/simple.NewParser"
	out, isErr := s.handleTool("get_context_bundle", map[string]any{"qualified_name": qn})
	if isErr {
		t.Fatalf("get_context_bundle failed: %s", out)
	}
	bundle, err := query.ContextBundle(s.store, qn, 8000)
	if err != nil {
		t.Fatalf("ContextBundle: %v", err)
	}
	wantRaw := int64(bundle.TokenEstimate) * 4
	if wantRaw == 0 {
		t.Fatal("bundle token estimate is 0; test would be vacuous")
	}

	snap := s.usage.snapshot()
	if len(snap.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(snap.Rows))
	}
	row := snap.Rows[0]
	if row.Tool != "get_context_bundle" || row.Calls != 1 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.RawBytes != wantRaw {
		t.Errorf("rawBytes = %d, want %d (TokenEstimate x 4)", row.RawBytes, wantRaw)
	}
}

// --- 3.3 stats tool ---

func TestToolsListIncludesStats(t *testing.T) {
	for _, tool := range buildToolsList() {
		if tool["name"] == "stats" {
			return
		}
	}
	t.Errorf("tools list lacks stats")
}

func TestStatsEmptySessionZeroed(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("stats", map[string]any{})
	if isErr {
		t.Fatalf("stats failed: %s", out)
	}
	if !strings.Contains(out, "tools[0]") {
		t.Errorf("empty session should have no tool rows, got:\n%s", out)
	}
	totals := parseStatsTotals(t, out)
	for _, key := range []string{"calls", "errors", "response_bytes", "raw_bytes", "reduction", "gated_responses"} {
		if totals[key] != 0 {
			t.Errorf("totals.%s = %v, want 0", key, totals[key])
		}
	}
}

func TestStatsReductionMathOnScriptedUsage(t *testing.T) {
	s := newTestServer(t)
	s.usage.record("alpha", 100, 400, false) // 75% reduction
	s.usage.record("beta", 50, 50, false)    // identity: 0%
	s.usage.record("beta", 10, 10, true)     // error: identity, counted

	out, isErr := s.handleTool("stats", map[string]any{})
	if isErr {
		t.Fatalf("stats failed: %s", out)
	}

	rows := parseStatsRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d:\n%s", len(rows), out)
	}
	if strings.Index(out, "alpha") >= strings.Index(out, "beta") {
		t.Errorf("rows not sorted by tool name:\n%s", out)
	}

	alpha := rows["alpha"]
	if alpha["calls"] != 1 || alpha["errors"] != 0 || alpha["response_bytes"] != 100 || alpha["raw_bytes"] != 400 {
		t.Errorf("alpha row = %v, want calls=1 errors=0 resp=100 raw=400", alpha)
	}
	if math.Abs(alpha["reduction"]-0.75) > 1e-9 {
		t.Errorf("alpha reduction = %v, want 0.75", alpha["reduction"])
	}

	beta := rows["beta"]
	if beta["calls"] != 2 || beta["errors"] != 1 || beta["response_bytes"] != 60 || beta["raw_bytes"] != 60 {
		t.Errorf("beta row = %v, want calls=2 errors=1 resp=60 raw=60", beta)
	}
	if beta["reduction"] != 0 {
		t.Errorf("beta reduction = %v, want 0", beta["reduction"])
	}

	totals := parseStatsTotals(t, out)
	if totals["calls"] != 3 || totals["errors"] != 1 {
		t.Errorf("totals = %v, want calls=3 errors=1", totals)
	}
	if totals["response_bytes"] != 160 || totals["raw_bytes"] != 460 {
		t.Errorf("totals = %v, want resp=160 raw=460", totals)
	}
	want := 1 - 160.0/460.0
	if math.Abs(totals["reduction"]-want) > 1e-9 {
		t.Errorf("totals reduction = %v, want %v", totals["reduction"], want)
	}
}

// TestStatsByteIdenticalRepeatCalls pins the determinism requirement: two
// consecutive stats calls with no intervening tool calls render identically.
func TestStatsByteIdenticalRepeatCalls(t *testing.T) {
	s := newTestServer(t)
	s.usage.record("zeta", 12, 340, false)
	s.usage.record("kappa", 5, 5, true)

	out1, isErr := s.handleTool("stats", map[string]any{})
	if isErr {
		t.Fatalf("stats failed: %s", out1)
	}
	out2, isErr := s.handleTool("stats", map[string]any{})
	if isErr {
		t.Fatalf("stats failed: %s", out2)
	}
	if out1 != out2 {
		t.Errorf("repeat stats calls differ:\nfirst:\n%s\nsecond:\n%s", out1, out2)
	}
}

func TestStatsErrorCallsCounted(t *testing.T) {
	s := newTestServer(t)
	out, isErr := s.handleTool("show", map[string]any{})
	if !isErr {
		t.Fatalf("expected error from show without qualified_name, got: %s", out)
	}
	snap := s.usage.snapshot()
	if len(snap.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(snap.Rows))
	}
	row := snap.Rows[0]
	if row.Tool != "show" || row.Calls != 1 || row.Errors != 1 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.RespBytes != int64(len(out)) || row.RawBytes != int64(len(out)) {
		t.Errorf("error payload not counted: resp=%d raw=%d, want %d", row.RespBytes, row.RawBytes, len(out))
	}
}

func TestStatsGatedResponseCount(t *testing.T) {
	s := newTestServer(t)
	t.Setenv(responseLimitEnv, "512")
	s.gateResponse("overview", oversizedPayload("gatedStatsToken"))

	out, isErr := s.handleTool("stats", map[string]any{})
	if isErr {
		t.Fatalf("stats failed: %s", out)
	}
	totals := parseStatsTotals(t, out)
	if totals["gated_responses"] != 1 {
		t.Errorf("gated_responses = %v, want 1\ntotals: %v", totals["gated_responses"], totals)
	}
}

func TestStatsResetOnNewServerInstance(t *testing.T) {
	s1 := newTestServer(t)
	s1.recordUsage("search", nil, "abc", false)
	if snap := s1.usage.snapshot(); snap.TotalCalls != 1 {
		t.Fatalf("first server should have 1 call, got %d", snap.TotalCalls)
	}

	s2 := newTestServer(t)
	out, isErr := s2.handleTool("stats", map[string]any{})
	if isErr {
		t.Fatalf("stats failed: %s", out)
	}
	if !strings.Contains(out, "tools[0]") {
		t.Errorf("fresh server instance should report no rows, got:\n%s", out)
	}
	totals := parseStatsTotals(t, out)
	if totals["calls"] != 0 || totals["response_bytes"] != 0 {
		t.Errorf("fresh server totals = %v, want zeroed", totals)
	}
}
