package contract

import (
	"fmt"
	"regexp"
	"strings"

	"codemap/store"
)

// RuntimeExtractor is a data-driven detector for one runtime contract kind.
// An extractor declares which method calls produce/consume an entity, which
// config literals name it, and how raw literals normalize to matchable
// patterns. Adding a new infrastructure (Postgres, RabbitMQ, Kafka, ...) is a
// new entry in the registry — not new analysis code.
type RuntimeExtractor struct {
	// Normalize canonicalizes a raw literal into a matchable pattern. When
	// nil, the identity normalizer (quote-strip only) is used.
	Normalize func(string) string
	// Kind is the runtime contract kind (redis, jetstream, ws_type, or custom).
	Kind store.RuntimeContractKind
	// Name identifies the extractor for config and diagnostics.
	Name string
	// ConstPrefix, when set, marks string constants whose symbol name starts
	// with it as shared entities (e.g. "MsgType" for WS message types).
	ConstPrefix string
	// ProducerMethods are method names whose first string-literal argument is a
	// produced entity (write/publish side).
	ProducerMethods []string
	// ConsumerMethods are method names whose first string-literal argument is a
	// consumed entity (read/subscribe side).
	ConsumerMethods []string
	// ConfigPatterns are regexes matching config literals that name a produced
	// entity (e.g. `StreamConfig{Name: "..."}`); capture group 1 is the literal.
	ConfigPatterns []string
	// ArgIndex selects which string-literal argument holds the entity, 0-based.
	// Default 0 captures the first string literal. RabbitMQ-style APIs put a
	// positional string (e.g. the exchange) before the routing key, so
	// `Publish(exchange, key, ...)` needs ArgIndex=1.
	ArgIndex int
	// DecodeSwitch, when true, captures `case "..."` literals as consumers.
	DecodeSwitch bool
}

// NormalizeRaw strips surrounding quotes/backticks from a literal.
func NormalizeRaw(s string) string {
	return strings.Trim(s, "\"`")
}

// NamedNormalizers lets config-driven extractors pick a built-in normalizer by
// name instead of wiring a Go function.
var NamedNormalizers = map[string]func(string) string{
	"raw":     NormalizeRaw,
	"redis":   NormalizeRedisKey,
	"subject": NormalizeJetStreamSubject,
}

// DefaultRuntimeExtractors returns the built-in extractors: Redis command
// calls, NATS JetStream publish/subscribe and stream config, and WS type
// constants / decode switches. Custom extractors are appended in Config.
func DefaultRuntimeExtractors() []RuntimeExtractor {
	return []RuntimeExtractor{
		{
			Kind:            store.RuntimeContractRedis,
			Name:            "redis",
			ProducerMethods: redisWriterMethods,
			ConsumerMethods: redisReaderMethods,
			Normalize:       NormalizeRedisKey,
		},
		{
			Kind:            store.RuntimeContractJetStream,
			Name:            "jetstream",
			ProducerMethods: []string{"Publish", "PublishResponse"},
			ConsumerMethods: []string{"Subscribe", "SubscribeSync"},
			ConfigPatterns: []string{
				`StreamConfig\s*\{[^}]*?(?:Name|Subject|FilterSubject)\s*:\s*\[?[^"]*"([^"\n]+)"`,
			},
			Normalize: NormalizeJetStreamSubject,
		},
		{
			Kind:         store.RuntimeContractWSType,
			Name:         "ws_type",
			ConstPrefix:  WSTypePrefix,
			DecodeSwitch: true,
			Normalize:    NormalizeRaw,
		},
	}
}

// redisWriterMethods are commands that write a key; readers consume it.
var redisWriterMethods = []string{
	"Set", "LPush", "RPush", "SAdd", "ZAdd", "Incr", "Decr", "MSet", "HSet", "Expire", "Del",
}

// redisReaderMethods are commands that read a key.
var redisReaderMethods = []string{
	"Get", "Exists", "Scan", "HGet", "LPop", "RPop", "SMembers", "ZRange", "MGet",
}

// JetStreamConfigFields are StreamConfig field names that hold stream/subject names.
var JetStreamConfigFields = []string{"Name", "Subject", "FilterSubject"}

// WSTypePrefix is the prefix for WS message type constants.
const WSTypePrefix = "MsgType"

// compiledExtractor is a RuntimeExtractor with its regexes precompiled.
type compiledExtractor struct {
	producer  *regexp.Regexp
	consumer  *regexp.Regexp
	config    *regexp.Regexp
	decode    *regexp.Regexp
	normalize func(string) string
	def       RuntimeExtractor
}

// compile precompiles the call-site regexes for an extractor.
func (e RuntimeExtractor) compile() *compiledExtractor {
	ce := &compiledExtractor{def: e, normalize: e.Normalize}
	if len(e.ProducerMethods) > 0 {
		ce.producer = callRegex(e.ProducerMethods, e.ArgIndex)
	}
	if len(e.ConsumerMethods) > 0 {
		ce.consumer = callRegex(e.ConsumerMethods, e.ArgIndex)
	}
	if len(e.ConfigPatterns) > 0 {
		ce.config = regexp.MustCompile(strings.Join(e.ConfigPatterns, "|"))
	}
	if e.DecodeSwitch {
		ce.decode = wsDecodeSwitchRe
	}
	if ce.normalize == nil {
		ce.normalize = NormalizeRaw
	}
	return ce
}

// callRegex matches a method call whose argIndex-th string-literal argument is
// the entity, e.g. `.Set("key")` → captures "key"; with argIndex=1,
// `.Publish("", "orders.created")` → captures "orders.created".
func callRegex(methods []string, argIndex int) *regexp.Regexp {
	// Skip argIndex string literals before the captured one. Each skipped
	// literal is a quoted, quote-free span; anything else is matched loosely.
	var prefix strings.Builder
	for range argIndex {
		prefix.WriteString(`[^"\n]*"[^"\n]*"`)
	}
	return regexp.MustCompile(`\.(?:` + strings.Join(methods, "|") + `)\s*\(` + prefix.String() + `[^"\n]*"([^"\n]+)"`)
}

// NormalizeRedisKey normalizes a Redis key by replacing interpolated segments
// with :name placeholders. Concrete dates/IPs/UUIDs and common markers map to a
// canonical form, e.g. "krucil_size:2024-01-01:192.168.1.1" →
// "krucil_size:{date}:{ip}".
func NormalizeRedisKey(key string) string {
	key = strings.Trim(key, "\"`")
	key = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`).ReplaceAllString(key, "{date}")
	key = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`).ReplaceAllString(key, "{ip}")
	key = regexp.MustCompile(`[a-f0-9-]{36}`).ReplaceAllString(key, "{id}")
	key = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(key, ":{name}")
	key = regexp.MustCompile(`%[sdv]`).ReplaceAllString(key, ":{name}")
	return key
}

// NormalizeJetStreamSubject normalizes a JetStream subject by replacing
// request-id-like interpolation segments.
func NormalizeJetStreamSubject(subject string) string {
	subject = strings.Trim(subject, "\"`")
	subject = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(subject, ":request_id")
	subject = regexp.MustCompile(`[a-f0-9-]{36}`).ReplaceAllString(subject, ":id")
	subject = regexp.MustCompile(`%[sdv]`).ReplaceAllString(subject, ":request_id")
	return subject
}

var wsDecodeSwitchRe = regexp.MustCompile(`case\s+"([^"\n]+)"\s*:`)

// ExtractRuntimeContracts scans symbols and indexed file contents for runtime
// contract patterns using the configured extractor registry (defaulting to the
// built-ins). Only call-site literals and ConstPrefix-style constants are
// captured.
func ExtractRuntimeContracts(analysis *Analysis, now string) []store.RuntimeContract {
	extractors := analysis.Config.RuntimeExtractors
	if len(extractors) == 0 {
		extractors = DefaultRuntimeExtractors()
	}
	compiled := make([]*compiledExtractor, 0, len(extractors))
	for _, e := range extractors {
		compiled = append(compiled, e.compile())
	}

	contracts := make([]store.RuntimeContract, 0, len(analysis.SymbolsByRepo))
	for repo, symbols := range analysis.SymbolsByRepo {
		contracts = append(contracts, extractRepoContracts(compiled, symbols, analysis.FileContents[repo], repo, now)...)
	}
	return contracts
}

func extractRepoContracts(extractors []*compiledExtractor, symbols map[string]store.Symbol, files map[string]string, repo, now string) []store.RuntimeContract {
	contracts := make([]store.RuntimeContract, 0, len(symbols)*len(extractors))
	for _, ce := range extractors {
		contracts = append(contracts, extractConstContracts(ce, symbols, repo, now)...)
		contracts = append(contracts, extractCallSiteContracts(ce, symbols, files, repo, now)...)
	}
	return contracts
}

// extractConstContracts captures shared constants whose name starts with the
// extractor's ConstPrefix (e.g. MsgTypeAgentAsk → ws_type).
func extractConstContracts(ce *compiledExtractor, symbols map[string]store.Symbol, repo, now string) []store.RuntimeContract {
	if ce.def.ConstPrefix == "" {
		return nil
	}
	contracts := make([]store.RuntimeContract, 0, len(symbols))
	for qn, sym := range symbols {
		if sym.Kind != kindConst || !strings.HasPrefix(sym.Name, ce.def.ConstPrefix) {
			continue
		}
		value := extractStringConst(sym.Signature)
		if value == "" {
			continue
		}
		contracts = append(contracts, store.RuntimeContract{
			Kind:      ce.def.Kind,
			Pattern:   ce.normalize(value),
			FromRef:   qn,
			Direction: store.ContractDirectionShared,
			Evidence:  fmt.Sprintf("%s type constant %q in %s", ce.def.Name, value, repo),
			IndexedAt: now,
			Repo:      repo,
		})
	}
	return contracts
}

// extractCallSiteContracts scans each file once for the extractor's call-site
// literals, attributing each site to the nearest enclosing symbol.
func extractCallSiteContracts(ce *compiledExtractor, symbols map[string]store.Symbol, files map[string]string, repo, now string) []store.RuntimeContract {
	contracts := make([]store.RuntimeContract, 0)
	for path, content := range files {
		for i, line := range strings.Split(content, "\n") {
			lineNo := i + 1
			rc := matchCallSite(ce, symbols, line, path, lineNo, repo, now)
			if rc != nil {
				contracts = append(contracts, *rc)
			}
		}
	}
	return contracts
}

// matchCallSite checks a single line for the extractor's call-site literals.
// The first matching pattern wins; direction follows producer → consumer →
// config → decode-switch priority.
func matchCallSite(ce *compiledExtractor, symbols map[string]store.Symbol, line, path string, lineNo int, repo, now string) *store.RuntimeContract {
	fromRef := enclosingSymbol(symbols, path, lineNo)

	if ce.producer != nil {
		if m := ce.producer.FindStringSubmatch(line); m != nil {
			rc := newCallContract(ce, store.ContractDirectionProducer, m[1], fromRef, path, lineNo, repo, now)
			return &rc
		}
	}
	if ce.consumer != nil {
		if m := ce.consumer.FindStringSubmatch(line); m != nil {
			rc := newCallContract(ce, store.ContractDirectionConsumer, m[1], fromRef, path, lineNo, repo, now)
			return &rc
		}
	}
	if ce.config != nil {
		if m := ce.config.FindStringSubmatch(line); m != nil {
			rc := newCallContract(ce, store.ContractDirectionProducer, m[1], fromRef, path, lineNo, repo, now)
			return &rc
		}
	}
	if ce.decode != nil {
		if m := ce.decode.FindStringSubmatch(line); m != nil {
			rc := store.RuntimeContract{
				Kind:      ce.def.Kind,
				Pattern:   ce.normalize(m[1]),
				FromRef:   fromRef,
				Direction: store.ContractDirectionConsumer,
				Evidence:  fmt.Sprintf("%s decode switch case %q at %s:%d", ce.def.Name, m[1], path, lineNo),
				IndexedAt: now,
				Repo:      repo,
			}
			return &rc
		}
	}
	return nil
}

// newCallContract builds a runtime contract for a call-site literal match.
func newCallContract(ce *compiledExtractor, direction store.ContractDirection, literal, fromRef, path string, lineNo int, repo, now string) store.RuntimeContract {
	kind, name := ce.def.Kind, ce.def.Name
	if name == "" {
		name = string(kind)
	}
	return store.RuntimeContract{
		Kind:      kind,
		Pattern:   ce.normalize(literal),
		FromRef:   fromRef,
		Direction: direction,
		Evidence:  fmt.Sprintf("%s %s literal %q at %s:%d", name, direction, literal, path, lineNo),
		IndexedAt: now,
		Repo:      repo,
	}
}

// enclosingSymbol returns the qualified name of the nearest declaration at or
// above the given line in the file, or the file path when none is found.
func enclosingSymbol(symbols map[string]store.Symbol, path string, line int) string {
	best := ""
	bestLine := 0
	for _, sym := range symbols {
		if sym.PosFile != path || sym.PosLine <= 0 || sym.PosLine > line {
			continue
		}
		if sym.PosLine > bestLine {
			bestLine = sym.PosLine
			best = sym.QualifiedName
		}
	}
	if best != "" {
		return best
	}
	return path
}
