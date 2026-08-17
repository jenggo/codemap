// Package contract implements wire-contract intelligence: detecting producer/
// consumer relationships across repos by shared message-type constants and
// CBOR-name-aware structural shape comparison.
package contract

import (
	"sort"
	"strings"

	"codemap/store"
)

// Signal weights for confidence scoring.
const (
	WeightConstantMatch = 0.6
	WeightShapeMatch    = 0.4
)

// Symbol kind constants.
const (
	kindConst = "const"
	kindType  = "type"
)

// DefaultConfidenceThreshold is the minimum confidence for an edge to be
// non-suggested. Edges below this threshold get Suggested=true.
const DefaultConfidenceThreshold = 0.5

// Config controls contract analysis behavior.
type Config struct {
	// SuppressPairs lists from→to pairs to exclude from results.
	SuppressPairs map[string]bool
	// RuntimeExtractors drives runtime-contract detection. When empty, the
	// built-in extractors (redis, jetstream, ws_type) are used; entries are
	// appended to the defaults to add new infrastructures.
	RuntimeExtractors []RuntimeExtractor
	// ConfidenceThreshold is the minimum confidence for non-suggested edges.
	ConfidenceThreshold float64
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ConfidenceThreshold: DefaultConfidenceThreshold,
		SuppressPairs:       make(map[string]bool),
		RuntimeExtractors:   DefaultRuntimeExtractors(),
	}
}

// Analysis holds all data needed for contract analysis across repos.
type Analysis struct {
	// Symbols indexed by repo → qualified_name.
	SymbolsByRepo map[string]map[string]store.Symbol
	// Constants indexed by repo → const_name → value.
	ConstantsByRepo map[string]map[string]string
	// Structs indexed by repo → qualified_name → StructInfo.
	StructsByRepo map[string]map[string]StructInfo
	// FileContents indexed by repo → file_path → content.
	FileContents map[string]map[string]string
	// Config for this analysis.
	Config Config
}

// StructInfo holds parsed struct shape information.
type StructInfo struct {
	QualifiedName string
	PackagePath   string
	Repo          string
	Fields        []FieldInfo
}

// FieldInfo holds one struct field's wire mapping.
type FieldInfo struct {
	GoName   string
	WireName string
	GoType   string
}

// FindSharedTypeConstants detects Signal A: shared message type string
// constants across repos. It returns pairs of (producer_const, consumer_const)
// that share the same string value.
func FindSharedTypeConstants(analysis *Analysis) []ConstantPair {
	valueIndex := buildValueIndex(analysis)
	return findPairsAcrossRepos(valueIndex)
}

// buildValueIndex creates a reverse index: string_value → []ConstantRef across repos.
func buildValueIndex(analysis *Analysis) map[string][]ConstantPair {
	valueIndex := make(map[string][]ConstantPair)
	for repo, consts := range analysis.ConstantsByRepo {
		for name, value := range consts {
			if value == "" {
				continue
			}
			ref := ConstantRef{Repo: repo, Name: name, Value: value}
			valueIndex[value] = append(valueIndex[value], ConstantPair{A: ref})
		}
	}
	return valueIndex
}

// findPairsAcrossRepos finds pairs where the same value appears in different repos.
func findPairsAcrossRepos(valueIndex map[string][]ConstantPair) []ConstantPair {
	total := 0
	for _, refs := range valueIndex {
		total += len(refs)
	}
	pairs := make([]ConstantPair, 0, total)
	seen := make(map[string]bool)
	for _, refs := range valueIndex {
		pairs = append(pairs, collectCrossRepoPairs(refs, seen)...)
	}
	return pairs
}

func collectCrossRepoPairs(refs []ConstantPair, seen map[string]bool) []ConstantPair {
	// Canonicalize the side ordering so edge direction is deterministic across
	// runs (refs arrive in map-iteration order).
	sort.Slice(refs, func(i, j int) bool {
		a, b := refs[i].A, refs[j].A
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Name < b.Name
	})

	pairs := make([]ConstantPair, 0, len(refs))
	for i := range refs {
		for j := i + 1; j < len(refs); j++ {
			if refs[i].A.Repo == refs[j].A.Repo {
				continue
			}
			key := refs[i].A.Repo + ":" + refs[i].A.Name + "→" + refs[j].A.Repo + ":" + refs[j].A.Name
			keyRev := refs[j].A.Repo + ":" + refs[j].A.Name + "→" + refs[i].A.Repo + ":" + refs[i].A.Name
			if seen[key] || seen[keyRev] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, ConstantPair{A: refs[i].A, B: refs[j].A})
		}
	}
	return pairs
}

// ConstantRef identifies a constant in a specific repo.
type ConstantRef struct {
	Repo  string
	Name  string
	Value string
}

// ConstantPair links two constants with the same value across repos.
type ConstantPair struct {
	A ConstantRef
	B ConstantRef
}

// ConfidenceScore computes the combined confidence for a contract edge.
// signalA=true means a shared type constant was found; signalB=true means
// structural shape match was found.
func ConfidenceScore(signalA, signalB bool) float64 {
	var score float64
	if signalA {
		score += WeightConstantMatch
	}
	if signalB {
		score += WeightShapeMatch
	}
	return score
}

// IsSuggested returns true if the confidence is below the threshold.
func IsSuggested(confidence, threshold float64) bool {
	return confidence < threshold
}

// FilterSuppressed removes suppressed pairs from the results.
func FilterSuppressed(pairs []ConstantPair, suppress map[string]bool) []ConstantPair {
	if len(suppress) == 0 {
		return pairs
	}
	var filtered []ConstantPair
	for _, p := range pairs {
		key := p.A.Repo + ":" + p.A.Name + " → " + p.B.Repo + ":" + p.B.Name
		keyRev := p.B.Repo + ":" + p.B.Name + " → " + p.A.Repo + ":" + p.A.Name
		if suppress[key] || suppress[keyRev] {
			continue
		}
		filtered = append(filtered, p)
	}
	return filtered
}

// fieldsFromSymbol converts persisted symbol fields into analysis field info.
func fieldsFromSymbol(fs []store.SymbolField) []FieldInfo {
	if len(fs) == 0 {
		return nil
	}
	out := make([]FieldInfo, 0, len(fs))
	for _, f := range fs {
		out = append(out, FieldInfo{GoName: f.GoName, WireName: f.WireName, GoType: f.GoType})
	}
	return out
}

// associatedStruct resolves the struct most likely used alongside a message
// type constant: same package, preferring the struct named after the constant
// with the "MsgType" prefix stripped (MsgTypeAgentAsk → AgentAsk), falling back
// to the package's sole struct.
func associatedStruct(analysis *Analysis, repo, constQN string) (StructInfo, bool) {
	structs := analysis.StructsByRepo[repo]
	if len(structs) == 0 {
		return StructInfo{}, false
	}
	dot := strings.LastIndex(constQN, ".")
	if dot < 0 {
		return StructInfo{}, false
	}
	pkg := constQN[:dot]
	base := constQN[dot+1:]

	if s, ok := structs[pkg+"."+strings.TrimPrefix(base, "MsgType")]; ok {
		return s, true
	}

	var only StructInfo
	count := 0
	for qn, si := range structs {
		if strings.HasPrefix(qn, pkg+".") {
			count++
			only = si
		}
	}
	if count == 1 {
		return only, true
	}
	for _, si := range structs {
		if strings.HasPrefix(si.QualifiedName, pkg+".") {
			return si, true
		}
	}
	return StructInfo{}, false
}

// constantsInPackage returns the set of string-constant values declared in the
// given package of a repo.
func constantsInPackage(analysis *Analysis, repo, pkg string) map[string]bool {
	out := make(map[string]bool)
	for qn, value := range analysis.ConstantsByRepo[repo] {
		if qn == pkg || strings.HasPrefix(qn, pkg+".") {
			out[value] = true
		}
	}
	return out
}

// haveSharedConstant reports whether two structs' packages share a message type
// string constant (i.e. they are already linked by Signal A).
func haveSharedConstant(analysis *Analysis, a, b StructInfo) bool {
	aConsts := constantsInPackage(analysis, a.Repo, a.PackagePath)
	if len(aConsts) == 0 {
		return false
	}
	for value := range aConsts {
		if _, ok := constantsInPackage(analysis, b.Repo, b.PackagePath)[value]; ok {
			return true
		}
	}
	return false
}
