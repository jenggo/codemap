package contract

import (
	"fmt"
	"time"

	"codemap/store"
)

// RunAnalysis performs contract extraction over a workspace store.
// It reads from the namespaced symbol table and indexed file contents and
// never re-walks source.
func RunAnalysis(st *store.Store, cfg Config) ([]store.Contract, []store.RuntimeContract, []store.DriftReport, error) {
	analysis, err := buildAnalysis(st)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("building analysis context: %w", err)
	}
	analysis.Config = cfg

	now := time.Now().UTC().Format(time.RFC3339)
	contracts, structuralDrifts := extractContracts(analysis, now)
	contracts = filterSuppressed(contracts, cfg.SuppressPairs)

	runtimeContracts := ExtractRuntimeContracts(analysis, now)

	drifts := append(FindConstantDrift(analysis, now), structuralDrifts...)
	drifts = filterSuppressedDrifts(drifts, cfg.SuppressPairs)

	return contracts, runtimeContracts, drifts, nil
}

// ReanalyzeStaleContracts reruns the global contract analysis once when any
// member's contract data is stale (older than the repo's last symbol index).
// The analysis is cross-repo, so one run rewrites all contract rows; per-row
// repo attribution comes from the from-side reference. Returns the list of
// repos whose stale data was refreshed.
func ReanalyzeStaleContracts(st *store.Store, cfg Config) ([]string, error) {
	repos, err := st.ListRepos()
	if err != nil {
		return nil, err
	}

	var staleRepos []string
	for _, repo := range repos {
		if repo.Missing {
			continue
		}
		stale, err := st.IsContractStale(repo.ModulePath)
		if err != nil {
			continue
		}
		if stale {
			staleRepos = append(staleRepos, repo.ModulePath)
		}
	}
	if len(staleRepos) == 0 {
		return nil, nil
	}

	contracts, runtimeContracts, drifts, err := RunAnalysis(st, cfg)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := st.WriteContractsAll(contracts, runtimeContracts, drifts, now); err != nil {
		return nil, err
	}
	return staleRepos, nil
}

// buildAnalysis reads from the namespaced symbol table to construct the
// analysis context. This never re-walks source files.
func buildAnalysis(st *store.Store) (*Analysis, error) {
	symbols, err := st.AllSymbols(false)
	if err != nil {
		return nil, err
	}

	analysis := &Analysis{
		SymbolsByRepo:   make(map[string]map[string]store.Symbol),
		ConstantsByRepo: make(map[string]map[string]string),
		StructsByRepo:   make(map[string]map[string]StructInfo),
		FileContents:    make(map[string]map[string]string),
	}

	for _, sym := range symbols {
		repo := sym.Repo
		if repo == "" {
			continue
		}
		if analysis.SymbolsByRepo[repo] == nil {
			analysis.SymbolsByRepo[repo] = make(map[string]store.Symbol)
		}
		analysis.SymbolsByRepo[repo][sym.QualifiedName] = sym

		// Index string constants by name.
		if sym.Kind == kindConst {
			if analysis.ConstantsByRepo[repo] == nil {
				analysis.ConstantsByRepo[repo] = make(map[string]string)
			}
			value := extractStringConst(sym.Signature)
			if value != "" {
				analysis.ConstantsByRepo[repo][sym.QualifiedName] = value
			}
		}

		// Index structs (with their persisted field shapes) by qualified name.
		if sym.Kind == kindType {
			if analysis.StructsByRepo[repo] == nil {
				analysis.StructsByRepo[repo] = make(map[string]StructInfo)
			}
			analysis.StructsByRepo[repo][sym.QualifiedName] = StructInfo{
				QualifiedName: sym.QualifiedName,
				PackagePath:   sym.PackagePath,
				Repo:          repo,
				Fields:        fieldsFromSymbol(sym.Fields),
			}
		}
	}

	files, err := st.FileContentsByRepo()
	if err != nil {
		return nil, fmt.Errorf("loading indexed file contents: %w", err)
	}
	analysis.FileContents = files

	return analysis, nil
}

// extractStringConst extracts the string value from a const signature.
// e.g., `= "agent_ask"` → "agent_ask"
func extractStringConst(sig string) string {
	sig = trimSpaces(sig)
	if len(sig) < 3 {
		return ""
	}
	if sig[0] == '=' {
		sig = sig[1:]
		sig = trimSpaces(sig)
	}
	if len(sig) >= 2 && sig[0] == '"' && sig[len(sig)-1] == '"' {
		return sig[1 : len(sig)-1]
	}
	return ""
}

func trimSpaces(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// extractContracts produces contract edges and structural drift reports from
// the analysis context. Signal A pairs (shared type constant) are always
// linked; a CBOR-name-aware shape match on the associated structs upgrades the
// edge to high confidence and classifies drift. Structurally identical pairs
// with no shared constant produce low-confidence suggested edges.
func extractContracts(analysis *Analysis, now string) ([]store.Contract, []store.DriftReport) {
	pairs := FindSharedTypeConstants(analysis)
	contracts := make([]store.Contract, 0, len(pairs))
	var drifts []store.DriftReport

	for _, pair := range pairs {
		fromRef, toRef := pair.A.Name, pair.B.Name

		aStruct, aOK := associatedStruct(analysis, pair.A.Repo, fromRef)
		bStruct, bOK := associatedStruct(analysis, pair.B.Repo, toRef)

		var comp ShapeComparison
		signalB := false
		severity := store.ContractSeverityUnknown
		if aOK && bOK {
			comp = CompareStructShapes(aStruct, bStruct)
			signalB = len(comp.Divergences) == 0
			// Severity reflects the drift even when shapes diverge: a
			// shared constant with divergent fields is still a contract,
			// reported breaking, never silently downgraded to unknown.
			severity = store.ContractSeverity(ClassifyDriftSeverity(comp))
			if d := DetectDrift(fromRef, toRef, aStruct, bStruct, now); d != nil {
				drifts = append(drifts, *d)
			}
		}

		confidence := ConfidenceScore(true, signalB)
		suggested := IsSuggested(confidence, analysis.Config.ConfidenceThreshold)

		evidence := fmt.Sprintf("shared constant value %q; repos: %s, %s",
			pair.A.Value, pair.A.Repo, pair.B.Repo)
		if signalB {
			evidence += fmt.Sprintf("; structural match on %d CBOR fields", comp.Matches)
		}

		contracts = append(contracts, store.Contract{
			FromRef:    fromRef,
			ToRef:      toRef,
			Direction:  InferContractDirection(analysis, fromRef, toRef),
			Confidence: confidence,
			Severity:   severity,
			Suggested:  suggested,
			Evidence:   evidence,
			IndexedAt:  now,
		})
	}

	contracts = append(contracts, suggestedShapePairs(analysis, now)...)

	return contracts, drifts
}

// suggestedShapePairs creates low-confidence suggested edges between
// structurally identical structs across repos that share no type constant.
func suggestedShapePairs(analysis *Analysis, now string) []store.Contract {
	structs := shapeCandidates(analysis)

	seen := make(map[string]bool)
	var out []store.Contract
	for i := range structs {
		for j := i + 1; j < len(structs); j++ {
			a, b := structs[i], structs[j]
			if a.Repo == b.Repo {
				continue
			}
			key := a.QualifiedName + "\u0000" + b.QualifiedName
			if seen[key] {
				continue
			}
			seen[key] = true
			if c, ok := shapeOnlyContract(a, b, analysis, now); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// shapeCandidates returns the structs eligible for shape-only matching.
func shapeCandidates(analysis *Analysis) []StructInfo {
	var structs []StructInfo
	for _, byQN := range analysis.StructsByRepo {
		for _, si := range byQN {
			if len(si.Fields) == 0 {
				continue
			}
			structs = append(structs, si)
		}
	}
	return structs
}

// shapeOnlyContract builds a suggested contract for two structs when they are
// structurally identical, unrelated by a shared constant, and cross-repo.
func shapeOnlyContract(a, b StructInfo, analysis *Analysis, now string) (store.Contract, bool) {
	if haveSharedConstant(analysis, a, b) {
		return store.Contract{}, false
	}
	comp := CompareStructShapes(a, b)
	if !comp.Identical {
		return store.Contract{}, false
	}
	// Canonicalize direction so results are deterministic; shape-only edges
	// carry a shared direction regardless of anchor order.
	from, to := a.QualifiedName, b.QualifiedName
	if from > to {
		from, to = to, from
	}
	return store.Contract{
		FromRef:    from,
		ToRef:      to,
		Direction:  store.ContractDirectionShared,
		Confidence: ConfidenceScore(false, true),
		Severity:   store.ContractSeverityCompatible,
		Suggested:  true,
		Evidence: fmt.Sprintf("identical struct shape (%d fields), no shared constant; repos: %s, %s",
			comp.Matches, a.Repo, b.Repo),
		IndexedAt: now,
	}, true
}

// filterSuppressed removes suppressed contract pairs.
func filterSuppressed(contracts []store.Contract, suppress map[string]bool) []store.Contract {
	if len(suppress) == 0 {
		return contracts
	}
	var filtered []store.Contract
	for _, c := range contracts {
		key := c.FromRef + " → " + c.ToRef
		keyRev := c.ToRef + " → " + c.FromRef
		if suppress[key] || suppress[keyRev] {
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered
}

// filterSuppressedDrifts removes drift reports for suppressed pairs.
func filterSuppressedDrifts(drifts []store.DriftReport, suppress map[string]bool) []store.DriftReport {
	if len(suppress) == 0 {
		return drifts
	}
	var filtered []store.DriftReport
	for _, d := range drifts {
		key := d.FromRef + " → " + d.ToRef
		keyRev := d.ToRef + " → " + d.FromRef
		if suppress[key] || suppress[keyRev] {
			continue
		}
		filtered = append(filtered, d)
	}
	return filtered
}
