package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/alpkeskin/gotoon"

	"codemap/query"
	"codemap/store"
)

// statsKeyCalls is the calls column name shared by per-tool rows and totals.
// Distinct from edgeTypeCalls: this names a stats column, not an edge type.
const statsKeyCalls = "calls"

// toolStat holds the usage counters for one tool over the server lifetime.
type toolStat struct {
	calls     int64
	errors    int64
	respBytes int64
	rawBytes  int64
}

// usageStats accumulates per-tool-call usage counters for the server process
// lifetime. Counters are in-memory diagnostics: a server restart resets them
// by design. A nil *usageStats records nothing and snapshots to zeros, so
// servers built as struct literals stay safe.
type usageStats struct {
	byTool         map[string]*toolStat
	gatedResponses int64
	mu             sync.Mutex
}

func newUsageStats() *usageStats {
	return &usageStats{byTool: make(map[string]*toolStat)}
}

// record updates the counters for one completed tool call. Safe under
// concurrent tool calls: every update happens under one mutex.
func (u *usageStats) record(tool string, respBytes, rawBytes int64, isErr bool) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.byTool == nil {
		u.byTool = make(map[string]*toolStat)
	}
	ts := u.byTool[tool]
	if ts == nil {
		ts = &toolStat{}
		u.byTool[tool] = ts
	}
	ts.calls++
	ts.respBytes += respBytes
	ts.rawBytes += rawBytes
	if isErr {
		ts.errors++
	}
}

// recordGated counts one response the overflow gate replaced with a manifest.
func (u *usageStats) recordGated() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.gatedResponses++
	u.mu.Unlock()
}

// usageRow is one tool's row of the stats report.
type usageRow struct {
	Tool      string
	Calls     int64
	Errors    int64
	RespBytes int64
	RawBytes  int64
	Reduction float64
}

// usageSnapshot is a consistent copy of the usage counters for reporting.
type usageSnapshot struct {
	Rows           []usageRow
	TotalCalls     int64
	TotalErrors    int64
	TotalRespBytes int64
	TotalRawBytes  int64
	Reduction      float64
	GatedResponses int64
}

// snapshot returns a consistent copy of the counters. Rows are sorted by tool
// name so repeated calls with identical usage render byte-identically.
func (u *usageStats) snapshot() usageSnapshot {
	if u == nil {
		return usageSnapshot{Rows: []usageRow{}}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	snap := usageSnapshot{Rows: make([]usageRow, 0, len(u.byTool))}
	for tool, ts := range u.byTool {
		snap.Rows = append(snap.Rows, usageRow{
			Tool:      tool,
			Calls:     ts.calls,
			Errors:    ts.errors,
			RespBytes: ts.respBytes,
			RawBytes:  ts.rawBytes,
			Reduction: reductionPct(ts.respBytes, ts.rawBytes),
		})
		snap.TotalCalls += ts.calls
		snap.TotalErrors += ts.errors
		snap.TotalRespBytes += ts.respBytes
		snap.TotalRawBytes += ts.rawBytes
	}
	sort.Slice(snap.Rows, func(i, j int) bool { return snap.Rows[i].Tool < snap.Rows[j].Tool })
	snap.Reduction = reductionPct(snap.TotalRespBytes, snap.TotalRawBytes)
	snap.GatedResponses = u.gatedResponses
	return snap
}

// reductionPct computes 1 - resp/raw clamped to [0, 1]. raw <= 0 yields 0:
// without a raw-cost basis there is no savings to claim. Clamping keeps
// percentages meaningful when an estimator's raw estimate lands below the
// rendered response (documented per estimator).
func reductionPct(respBytes, rawBytes int64) float64 {
	if rawBytes <= 0 {
		return 0
	}
	r := 1 - float64(respBytes)/float64(rawBytes)
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

// rawEstimator estimates the byte cost of the naive alternative reading for
// one successful tool call, derived from the typed handler result.
// Estimators are approximations by design; the identity fallback keeps every
// tool without one honest at 0% reduction.
type rawEstimator func(st *store.Store, result any) int64

// rawEstimators registers the per-tool raw-cost estimators, keyed by tool
// name. Counting itself needs zero per-handler edits; registering an estimator
// for a new tool only requires that handler to surface its typed result.
var rawEstimators = map[string]rawEstimator{
	toolSymbolBody:    estimateSymbolBodyRaw,
	keyToolPackage:    estimatePackageRaw,
	toolContextBundle: estimateBundleRaw,
}

// estimateSymbolBodyRaw models the naive alternative for get_symbol_body: the
// reader pulls the symbol's declaration plus the requested context window,
// both already computed by the handler as SymbolBodyResult spans. Rendering
// adds headers and fences, so a tiny symbol's rendered response can exceed
// the estimate; those calls clamp to 0% reduction instead of claiming savings.
func estimateSymbolBodyRaw(_ *store.Store, result any) int64 {
	body, ok := result.(*query.SymbolBodyResult)
	if !ok || body == nil {
		return 0
	}
	return int64(len(body.Body) + len(body.ContextBefore) + len(body.ContextAfter))
}

// estimatePackageRaw models the naive alternative for package: opening every
// symbol the response lists and reading its declaration body. Bodies come
// from the same span extraction get_symbol_body uses; a body that cannot be
// fetched contributes nothing, so the estimate understates rather than
// overstates savings.
func estimatePackageRaw(st *store.Store, result any) int64 {
	pkg, ok := result.(*query.PackageResult)
	if !ok || pkg == nil || st == nil {
		return 0
	}
	var total int64
	for _, sym := range pkg.ExportedSymbols {
		body, err := query.GetSymbolBody(st, sym.QualifiedName, 0, false)
		if err != nil {
			continue
		}
		total += int64(len(body.Body))
	}
	return total
}

// estimateBundleRaw models the naive alternative for get_context_bundle using
// the bundle's own token estimate at ~4 bytes per token — the same
// bytes-per-token heuristic the bundle uses to size itself against its
// budget. The rendered bundle adds headers per section, so small bundles can
// render larger than the estimate; those calls clamp to 0% reduction.
func estimateBundleRaw(_ *store.Store, result any) int64 {
	bundle, ok := result.(*query.Bundle)
	if !ok || bundle == nil {
		return 0
	}
	return int64(bundle.TokenEstimate) * 4
}

// renderUsageStats renders the session usage report as TOON: per-tool rows
// sorted by name, totals row last. Calls, errors, response bytes, estimated
// raw bytes, and clamped reduction appear per tool; the totals row adds the
// number of oversized responses the gate replaced with manifests.
func renderUsageStats(u *usageStats) string {
	snap := u.snapshot()
	rows := make([]any, 0, len(snap.Rows))
	for _, r := range snap.Rows {
		rows = append(rows, map[string]any{
			"tool":           r.Tool,
			statsKeyCalls:    r.Calls,
			"errors":         r.Errors,
			"response_bytes": r.RespBytes,
			"raw_bytes":      r.RawBytes,
			"reduction":      r.Reduction,
		})
	}
	totals := map[string]any{
		statsKeyCalls:     snap.TotalCalls,
		"errors":          snap.TotalErrors,
		"response_bytes":  snap.TotalRespBytes,
		"raw_bytes":       snap.TotalRawBytes,
		"reduction":       snap.Reduction,
		"gated_responses": snap.GatedResponses,
	}
	report := map[string]any{keyTools: rows, "totals": totals}
	out, err := gotoon.Encode(report)
	if err != nil {
		data, jerr := json.Marshal(report)
		if jerr != nil {
			return fmt.Sprintf("Error: encoding stats: %v", err)
		}
		return string(data)
	}
	return out
}

// statsTools returns the usage-reporting tool definitions.
func statsTools() []map[string]any {
	return []map[string]any{
		toolDef("stats",
			"Session per-tool usage accounting: calls, errors, rendered response bytes, estimated raw-read bytes, and reduction per tool, plus totals. Raw bytes estimate what reading the source instead would cost: per-tool estimators model the natural raw alternative (symbol body plus context window for get_symbol_body, exported symbol bodies for package, token estimate x 4 for get_context_bundle); every other tool reports raw = response bytes, an honest 0% reduction. Reduction is 1 - response/raw clamped to [0, 1]. gated_responses counts oversized responses replaced by manifests. Counters live for the server process lifetime and reset on restart.",
			map[string]any{}),
	}
}
