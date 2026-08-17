package contract

import (
	"strings"

	"codemap/store"
)

// CompareStructShapes performs wire-name-aware structural comparison between
// two structs. It returns whether they match and the list of field divergences.
// Comparison keys on effective wire name + Go type, order-agnostic.
func CompareStructShapes(a, b StructInfo) ShapeComparison {
	// Build maps by wire name.
	aByWire := make(map[string]FieldInfo, len(a.Fields))
	bByWire := make(map[string]FieldInfo, len(b.Fields))

	for _, f := range a.Fields {
		aByWire[f.WireName] = f
	}
	for _, f := range b.Fields {
		bByWire[f.WireName] = f
	}

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

// InferContractDirection classifies the wire role of each side of a contract
// from the sites that reference the message type constant, then maps the pair
// to a direction. Falls back to InferDirection's name heuristics.
func InferContractDirection(analysis *Analysis, fromRef, toRef string) store.ContractDirection {
	roleFrom := wireSiteRole(analysis, repoOfRef(analysis, fromRef), fromRef)
	roleTo := wireSiteRole(analysis, repoOfRef(analysis, toRef), toRef)
	switch {
	case roleFrom == siteRoleProducer && roleTo == siteRoleConsumer:
		return store.ContractDirectionProducer
	case roleFrom == siteRoleConsumer && roleTo == siteRoleProducer:
		return store.ContractDirectionConsumer
	default:
		return InferDirection(fromRef, toRef, analysis)
	}
}

// repoOfRef returns the repo owning a symbol, or "".
func repoOfRef(analysis *Analysis, qn string) string {
	for repo, syms := range analysis.SymbolsByRepo {
		if _, ok := syms[qn]; ok {
			return repo
		}
	}
	return ""
}

// wireSiteRole inspects indexed file contents for the closest usage of a
// message type constant outside its own declaration and classifies that site
// as producer (encode/publish) or consumer (decode/subscribe).
func wireSiteRole(analysis *Analysis, repo, qn string) string {
	short := extractShortName(qn)
	bestRole := ""
	bestLine := 0
	for path, content := range analysis.FileContents[repo] {
		for i, line := range strings.Split(content, "\n") {
			lineNo := i + 1
			if !strings.Contains(line, short) || lineNo <= bestLine {
				continue
			}
			encl := enclosingSymbol(analysis.SymbolsByRepo[repo], path, lineNo)
			if encl == qn || encl == path {
				continue // the constant's own declaration, or no enclosing symbol
			}
			bestLine = lineNo
			lower := strings.ToLower(encl + " " + line)
			switch {
			case matchesAny(lower, encodeHints):
				bestRole = siteRoleProducer
			case matchesAny(lower, decodeHints):
				bestRole = siteRoleConsumer
			default:
				bestRole = ""
			}
		}
	}
	return bestRole
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
