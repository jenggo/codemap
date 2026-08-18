package contract

import (
	"strings"
	"sync/atomic"

	"codemap/store"
)

// shapeCompareCount counts CompareStructShapes calls so tests can prove the
// precomputed field-multiset filter really skips disjoint pairs.
var shapeCompareCount atomic.Int64

// CompareStructShapes performs wire-name-aware structural comparison between
// two structs. It returns whether they match and the list of field divergences.
// Comparison keys on effective wire name + Go type, order-agnostic.
func CompareStructShapes(a, b StructInfo) ShapeComparison {
	shapeCompareCount.Add(1)
	aByWire := wireFieldMap(a)
	bByWire := wireFieldMap(b)

	// Find common fields.
	both := make(map[string]bool)
	for wireName := range aByWire {
		if _, ok := bByWire[wireName]; ok {
			both[wireName] = true
		}
	}

	// Find fields only in A (producer additions → compatible).
	var onlyA []FieldInfo
	for wireName, f := range aByWire {
		if !both[wireName] {
			onlyA = append(onlyA, f)
		}
	}

	// Find fields only in B (consumer additions → compatible).
	var onlyB []FieldInfo
	for wireName, f := range bByWire {
		if !both[wireName] {
			onlyB = append(onlyB, f)
		}
	}

	// Check for type mismatches on common fields.
	var divergences []FieldDivergence
	for wireName := range both {
		fA := aByWire[wireName]
		fB := bByWire[wireName]
		if fA.GoType != fB.GoType {
			divergences = append(divergences, FieldDivergence{
				WireName: wireName,
				TypeA:    fA.GoType,
				TypeB:    fB.GoType,
				Status:   "type_changed",
			})
		}
	}

	matches := len(both)
	totalFields := len(a.Fields) + len(b.Fields)
	score := 0.0
	if totalFields > 0 {
		score = float64(matches*2) / float64(totalFields)
	}

	return ShapeComparison{
		Matches:     matches,
		OnlyInA:     onlyA,
		OnlyInB:     onlyB,
		Divergences: divergences,
		Score:       score,
		Identical:   matches == len(a.Fields) && matches == len(b.Fields) && len(divergences) == 0,
	}
}

// wireFieldMap indexes a struct's fields by effective wire name.
func wireFieldMap(s StructInfo) map[string]FieldInfo {
	out := make(map[string]FieldInfo, len(s.Fields))
	for _, f := range s.Fields {
		out[f.WireName] = f
	}
	return out
}

// ShapeComparison holds the result of comparing two struct shapes.
type ShapeComparison struct {
	OnlyInA     []FieldInfo
	OnlyInB     []FieldInfo
	Divergences []FieldDivergence
	Matches     int
	Score       float64
	Identical   bool
}

// FieldDivergence describes a type mismatch on a shared wire field.
type FieldDivergence struct {
	WireName string
	TypeA    string
	TypeB    string
	Status   string
}

// InferDirection determines the contract direction based on the position
// of encode/decode sites. If both are in the same package or direction
// cannot be inferred, returns ContractDirectionShared.
func InferDirection(fromRef, toRef string, analysis *Analysis) store.ContractDirection {
	// Heuristic: if the producer symbol name contains "encode", "send",
	// "publish", or "write", it's the producer side. If the consumer
	// symbol name contains "decode", "receive", "subscribe", or "read",
	// it's the consumer side.
	// This is a simplified heuristic; real inference would use call-graph.
	producerHints := []string{"encode", "send", "publish", "write", "marshal", "put"}
	consumerHints := []string{"decode", "receive", "subscribe", "read", "unmarshal", "get"}

	fromName := extractShortName(fromRef)
	toName := extractShortName(toRef)

	fromIsProducer := matchesAny(fromName, producerHints)
	toIsConsumer := matchesAny(toName, consumerHints)
	fromIsConsumer := matchesAny(fromName, consumerHints)
	toIsProducer := matchesAny(toName, producerHints)

	if fromIsProducer && toIsConsumer {
		return store.ContractDirectionProducer
	}
	if fromIsConsumer && toIsProducer {
		return store.ContractDirectionConsumer
	}
	return store.ContractDirectionShared
}

const (
	siteRoleProducer = "producer"
	siteRoleConsumer = "consumer"
)

var (
	encodeHints = []string{"marshal", "encode", "send", "publish", "write", "put"}
	decodeHints = []string{"unmarshal", "decode", "receive", "subscribe", "read", "get"}
)

// analysisIndex holds lookup structures precomputed once per analysis so the
// per-edge and pair loops are O(1) lookups instead of per-edge scans:
//   - sites: per-repo per-package per-constant usage sites (direction inference)
//   - constantsByPkg: per-repo-package set of constant values (shared-constant check)
//   - fieldSets: per-struct set of wire names (field-multiset pair prefilter)
//   - refRepo: symbol qualified name → repo (O(1) repo resolution)
func buildAnalysisIndex(analysis *Analysis) *analysisIndex {
	idx := &analysisIndex{
		sites:          buildSiteIndex(analysis),
		constantsByPkg: make(map[string]map[string]bool),
		fieldSets:      make(map[string]map[string]bool),
		refRepo:        make(map[string]string, len(analysis.SymbolsByRepo)),
	}
	for repo, consts := range analysis.ConstantsByRepo {
		for qn, value := range consts {
			key := repo + "\x00" + packagePathOf(analysis, repo, qn)
			if idx.constantsByPkg[key] == nil {
				idx.constantsByPkg[key] = make(map[string]bool)
			}
			idx.constantsByPkg[key][value] = true
		}
	}
	for _, byQN := range analysis.StructsByRepo {
		for qn, si := range byQN {
			set := make(map[string]bool, len(si.Fields))
			for _, f := range si.Fields {
				set[f.WireName] = true
			}
			idx.fieldSets[qn] = set
		}
	}
	for repo, syms := range analysis.SymbolsByRepo {
		for qn := range syms {
			idx.refRepo[qn] = repo
		}
	}
	return idx
}

type analysisIndex struct {
	sites          siteIndex
	constantsByPkg map[string]map[string]bool
	fieldSets      map[string]map[string]bool
	refRepo        map[string]string
}

// packagePathOf returns the package path owning a symbol in a repo.
func packagePathOf(analysis *Analysis, repo, qn string) string {
	if sym, ok := analysis.SymbolsByRepo[repo][qn]; ok {
		return sym.PackagePath
	}
	return ""
}

// repoOf returns the repo owning a symbol, resolved in O(1) via the index.
func (idx *analysisIndex) repoOf(qn string) string {
	return idx.refRepo[qn]
}

// InferContractDirection classifies the wire role of each side of a contract
// from the sites that reference the message type constant, then maps the pair
// to a direction. Falls back to InferDirection's name heuristics. The site
// index is precomputed per analysis, so each call is a couple of map lookups.
func InferContractDirection(idx *analysisIndex, analysis *Analysis, fromRef, toRef string) store.ContractDirection {
	roleFrom := wireSiteRole(idx, analysis, idx.repoOf(fromRef), fromRef)
	roleTo := wireSiteRole(idx, analysis, idx.repoOf(toRef), toRef)
	switch {
	case roleFrom == siteRoleProducer && roleTo == siteRoleConsumer:
		return store.ContractDirectionProducer
	case roleFrom == siteRoleConsumer && roleTo == siteRoleProducer:
		return store.ContractDirectionConsumer
	default:
		return InferDirection(fromRef, toRef, analysis)
	}
}

// siteInfo is one usage site of a message type constant outside its own
// declaration, with the wire role (producer/consumer/"") classified at build
// time from the enclosing symbol name plus the line content. Sites are stored
// in line order, so the last entry is the highest-line usage.
type siteInfo struct {
	role string
}

// siteIndex maps repo → package path → constant short name → usage sites,
// ordered by line number. Built once per analysis and read O(1) per edge.
type siteIndex map[string]map[string]map[string][]siteInfo

// buildSiteIndex inspects every indexed file once, recording where each message
// type constant of the file's package is referenced, skipping the constant's
// own declaration line. Direction inference reads this instead of re-scanning
// every line of every file per contract edge.
func buildSiteIndex(analysis *Analysis) siteIndex {
	idx := make(siteIndex)
	for repo, syms := range analysis.SymbolsByRepo {
		pkgByFile, constsByPkg := constantNameIndex(syms)
		for path, content := range analysis.FileContents[repo] {
			names := constsByPkg[pkgByFile[path]]
			if len(names) == 0 {
				continue
			}
			for i, line := range strings.Split(content, "\n") {
				recordUsageSites(idx, repo, path, i+1, line, names, syms)
			}
		}
	}
	return idx
}

// constantNameIndex groups a repo's symbols by (file → package) and
// (package → constant short name → qualified name).
func constantNameIndex(syms map[string]store.Symbol) (map[string]string, map[string]map[string]string) {
	pkgByFile := make(map[string]string)
	constsByPkg := make(map[string]map[string]string)
	for qn, sym := range syms {
		if sym.PosFile == "" {
			continue
		}
		if _, ok := pkgByFile[sym.PosFile]; !ok {
			pkgByFile[sym.PosFile] = sym.PackagePath
		}
		if sym.Kind == kindConst {
			short := extractShortName(qn)
			if constsByPkg[sym.PackagePath] == nil {
				constsByPkg[sym.PackagePath] = make(map[string]string)
			}
			constsByPkg[sym.PackagePath][short] = qn
		}
	}
	return pkgByFile, constsByPkg
}

// recordUsageSites records one line's constant usages (outside declarations)
// into the site index with their classified wire role.
func recordUsageSites(idx siteIndex, repo, path string, lineNo int, line string, names map[string]string, syms map[string]store.Symbol) {
	for short, qn := range names {
		if !strings.Contains(line, short) {
			continue
		}
		encl := enclosingSymbol(syms, path, lineNo)
		if encl == qn || encl == path {
			continue // the constant's own declaration, or no enclosing symbol
		}
		role := classifySiteRole(encl, line)
		if idx[repo] == nil {
			idx[repo] = make(map[string]map[string][]siteInfo)
		}
		if idx[repo][symPackagePath(syms, qn)] == nil {
			idx[repo][symPackagePath(syms, qn)] = make(map[string][]siteInfo)
		}
		idx[repo][symPackagePath(syms, qn)][short] = append(idx[repo][symPackagePath(syms, qn)][short], siteInfo{role: role})
	}
}

func classifySiteRole(enclosing, line string) string {
	lower := strings.ToLower(enclosing + " " + line)
	switch {
	case matchesAny(lower, encodeHints):
		return siteRoleProducer
	case matchesAny(lower, decodeHints):
		return siteRoleConsumer
	}
	return ""
}

func symPackagePath(syms map[string]store.Symbol, qn string) string {
	if sym, ok := syms[qn]; ok {
		return sym.PackagePath
	}
	return ""
}

// wireSiteRole returns the wire role at the highest-line usage site of a
// message type constant (last usage wins, mirroring the original scan order).
func wireSiteRole(idx *analysisIndex, analysis *Analysis, repo, qn string) string {
	if repo == "" {
		return ""
	}
	pkg := packagePathOf(analysis, repo, qn)
	short := extractShortName(qn)
	sites := idx.sites[repo][pkg][short]
	if len(sites) == 0 {
		return ""
	}
	return sites[len(sites)-1].role
}

// shareWireField reports whether two structs share at least one wire name.
// The S² pair loop uses this to skip shape comparisons between disjoint structs.
func shareWireField(idx *analysisIndex, a, b StructInfo) bool {
	as := idx.fieldSets[a.QualifiedName]
	bs := idx.fieldSets[b.QualifiedName]
	if len(as) == 0 || len(bs) == 0 {
		return false
	}
	for w := range as {
		if bs[w] {
			return true
		}
	}
	return false
}

// haveSharedConstantIndex reports whether two structs' packages share a message
// type string constant (i.e. they are already linked by Signal A). Both
// packages' constant sets are precomputed, so this never re-scans repository
// state per pair.
func haveSharedConstantIndex(idx *analysisIndex, a, b StructInfo) bool {
	akey := a.Repo + "\x00" + a.PackagePath
	aConsts := idx.constantsByPkg[akey]
	if len(aConsts) == 0 {
		return false
	}
	bConsts := idx.constantsByPkg[b.Repo+"\x00"+b.PackagePath]
	for value := range aConsts {
		if bConsts[value] {
			return true
		}
	}
	return false
}

// extractShortName gets the last path component from a qualified name.
func extractShortName(qualifiedName string) string {
	for i := len(qualifiedName) - 1; i >= 0; i-- {
		if qualifiedName[i] == '.' {
			return qualifiedName[i+1:]
		}
	}
	return qualifiedName
}

// matchesAny checks if s contains any of the given substrings (case-insensitive).
func matchesAny(s string, hints []string) bool {
	s = strings.ToLower(s)
	for _, h := range hints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

// ClassifyDriftSeverity classifies the structural divergence between two
// shape-matched types.
//   - breaking: field removed, renamed, or type-changed on the consumer side
//   - compatible: producer-only field additions (lenient consumer decode)
//   - unknown: opaque payload (no shape comparison performed)
func ClassifyDriftSeverity(comp ShapeComparison) store.DriftSeverity {
	if comp.Identical {
		return store.DriftSeverityCompatible
	}
	// Type changes on shared fields are always breaking.
	if len(comp.Divergences) > 0 {
		return store.DriftSeverityBreaking
	}
	// A consumer requiring fields the producer never sends cannot decode.
	if len(comp.OnlyInB) > 0 {
		// Fields present on both sides in a swapped arrangement is a rename.
		if len(comp.OnlyInA) > 0 {
			return store.DriftSeverityBreaking
		}
		return store.DriftSeverityBreaking
	}
	// Producer-only additions are compatible (lenient decode).
	return store.DriftSeverityCompatible
}
