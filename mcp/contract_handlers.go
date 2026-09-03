package mcp

import (
	"encoding/json"
	"fmt"

	"codemap/query"
	"codemap/render"
	"codemap/store"
)

const (
	keyDirection = "direction"
	keySeverity  = "severity"
	keyFromRef   = "from_ref"
	keyToRef     = "to_ref"
)

func handleContracts(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, any, bool) {
	filter := store.ContractFilter{}

	if repo, ok := args["repo"].(string); ok {
		filter.Repo = repo
	}
	if dir, ok := args[keyDirection].(string); ok {
		filter.Direction = dir
	}
	if sev, ok := args[keySeverity].(string); ok {
		filter.Severity = sev
	}
	if conf, ok := args["min_confidence"].(float64); ok {
		filter.MinConfidence = conf
	}

	contracts, err := st.QueryContracts(filter)
	if err != nil {
		return fmt.Sprintf("Error querying contracts: %s", err), nil, true
	}

	data, err := json.MarshalIndent(contracts, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error marshaling contracts: %s", err), nil, true
	}
	return string(data), contracts, false
}

func handleContractDrift(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, any, bool) {
	var severity store.DriftSeverity
	if sev, ok := args[keySeverity].(string); ok {
		severity = store.DriftSeverity(sev)
	}

	var repo string
	if r, ok := args["repo"].(string); ok {
		repo = r
	}

	drifts, err := st.QueryDrift(severity, repo)
	if err != nil {
		return fmt.Sprintf("Error querying drift: %s", err), nil, true
	}

	data, err := json.MarshalIndent(drifts, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error marshaling drift: %s", err), nil, true
	}
	return string(data), drifts, false
}

func handleRuntimeContracts(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, any, bool) {
	var kind store.RuntimeContractKind
	if k, ok := args[keyKind].(string); ok {
		kind = store.RuntimeContractKind(k)
	}

	var repo string
	if r, ok := args["repo"].(string); ok {
		repo = r
	}

	contracts, err := st.QueryRuntimeContracts(kind, repo)
	if err != nil {
		return fmt.Sprintf("Error querying runtime contracts: %s", err), nil, true
	}

	data, err := json.MarshalIndent(contracts, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error marshaling runtime contracts: %s", err), nil, true
	}
	return string(data), contracts, false
}

func handleSuppressContract(st *store.Store, _ []query.Option, renderOpts []render.Option, args map[string]any) (string, any, bool) {
	fromRef, ok1 := args["from_ref"].(string)
	toRef, ok2 := args["to_ref"].(string)
	if !ok1 || !ok2 {
		return "Error: from_ref and to_ref are required", nil, true
	}

	if err := st.SuppressContracts(fromRef, toRef); err != nil {
		return fmt.Sprintf("Error suppressing contract: %s", err), nil, true
	}

	return fmt.Sprintf("Suppressed contract %s → %s", fromRef, toRef), nil, false
}

// contractTools returns the contract intelligence MCP tool definitions.
func contractTools() []map[string]any {
	return []map[string]any{
		toolDef(toolContracts,
			"List contract edges linking producer/consumer symbols across repos. Filterable by repo, direction, severity, and confidence. Contract edges are inferred from shared message-type constants and CBOR-name-aware structural shape matching.",
			map[string]any{
				keyRepo:          repoProp(),
				keyDirection:     stringProp("Filter by direction: producer, consumer, shared"),
				keySeverity:      stringProp("Filter by severity: compatible, breaking, unknown"),
				"min_confidence": intProp("Minimum confidence threshold (0-1, default 0)"),
			}),
		toolDef("contract_drift",
			"List structural drift reports between shape-matched types across repos. Groups by severity with field-level evidence (CBOR name, type before/after, repo).",
			map[string]any{
				keySeverity: stringProp("Filter by severity: compatible, breaking, unknown"),
				keyRepo:     repoProp(),
			}),
		toolDef("runtime_contracts",
			"List runtime contract entities: Redis key patterns, JetStream stream/subject names, and WS type strings. Each links to its producer/consumer sites.",
			map[string]any{
				keyKind: stringProp("Filter by kind: redis, jetstream, ws_type"),
				keyRepo: repoProp(),
			}),
		toolDef("suppress_contract",
			"Suppress a specific contract pair so it never appears in contracts, drift, or blast radius results. Use this to dismiss confirmed false positives.",
			map[string]any{
				keyFromRef: stringProp("Source symbol qualified name"),
				keyToRef:   stringProp("Target symbol qualified name"),
			},
			keyFromRef, keyToRef),
	}
}
