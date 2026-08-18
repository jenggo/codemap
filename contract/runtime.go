package contract

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

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
	// NormalizeRules are ordered regex-replace rules applied after the (optional)
	// named Normalize function. Rules are precompiled at config-build time.
	NormalizeRules []NormalizeRule
	// Kind is the runtime contract kind (redis, jetstream, ws_type, or custom).
	Kind store.RuntimeContractKind
	// Name identifies the extractor for config and diagnostics.
	Name string
	// ConstPrefix, when set, marks string constants whose symbol name starts
	// with it as shared entities (e.g. "MsgType" for WS message types), and
	// scopes DecodeSwitch extraction to files that define or reference such a
	// constant.
	ConstPrefix string
	// ImportsMatch gates call-site extraction to files whose import block
	// contains at least one of these substrings. An empty list fires in any
	// file (backward compatible).
	ImportsMatch []string
	// ProducerMethods are method names whose string-literal arguments from
	// ArgIndex onward are produced entities (write/publish side).
	ProducerMethods []string
	// ConsumerMethods are method names whose string-literal arguments from
	// ArgIndex onward are consumed entities (read/subscribe side).
	ConsumerMethods []string
	// ConfigPatterns are regexes matching config literals that name a produced
	// entity (e.g. `StreamConfig{Name: "..."}`); capture group 1 is the literal.
	ConfigPatterns []string
	// ArgIndex selects the first string-literal argument holding the entity,
	// 0-based. Every string-literal argument from ArgIndex to the call end is
	// captured (multi-key commands). Default 0 captures the first string
	// literal. RabbitMQ-style APIs put a positional string (e.g. the exchange)
	// before the routing key, so `Publish(exchange, key, ...)` needs ArgIndex=1.
	ArgIndex int
	// DecodeSwitch, when true, captures `case "..."` literals as consumers,
	// scoped to files that define/reference a ConstPrefix constant.
	DecodeSwitch bool
}

// NormalizeRule applies one ordered regex replacement during normalization.
type NormalizeRule struct {
	Re      *regexp.Regexp
	Replace string
}

// NormalizeRaw strips surrounding quotes/backticks from a literal.
func NormalizeRaw(s string) string {
	return strings.Trim(s, "\"`")
}

// Extractor names for the built-in kinds.
const (
	extractorNameRedis     = "redis"
	extractorNameJetStream = "jetstream"
	extractorNameWSType    = "ws_type"
)

// NamedNormalizers lets config-driven extractors pick a built-in normalizer by
// name instead of wiring a Go function.
var NamedNormalizers = map[string]func(string) string{
	"raw":              NormalizeRaw,
	extractorNameRedis: NormalizeRedisKey,
	"subject":          NormalizeJetStreamSubject,
}

// DefaultRuntimeExtractors returns the built-in extractors: Redis command
// calls, NATS JetStream publish/subscribe and stream config, and WS type
// constants / decode switches. Custom extractors are merged in Config.
//
// The redis extractor gates on the whole Redis-protocol client family
// (go-redis, valkey, rueidis, keydb): they all speak the Redis wire protocol,
// so their key patterns live in the same semantic namespace and stay under the
// redis kind for cross-repo linking. JetStream gates on nats. The ws_type
// extractor requires no import gate — it is scoped by ConstPrefix instead.
func DefaultRuntimeExtractors() []RuntimeExtractor {
	return []RuntimeExtractor{
		{
			Kind:            store.RuntimeContractRedis,
			Name:            extractorNameRedis,
			ImportsMatch:    []string{extractorNameRedis, "valkey", "rueidis", "keydb"},
			ProducerMethods: redisWriterMethods,
			ConsumerMethods: redisReaderMethods,
			Normalize:       NormalizeRedisKey,
		},
		{
			Kind:            store.RuntimeContractJetStream,
			Name:            extractorNameJetStream,
			ImportsMatch:    []string{"nats"},
			ProducerMethods: []string{"Publish", "PublishResponse"},
			ConsumerMethods: []string{"Subscribe", "SubscribeSync"},
			ConfigPatterns: []string{
				`StreamConfig\s*\{[^}]*?(?:Name|Subject|FilterSubject)\s*:\s*\[?[^"]*"([^"\n]+)"`,
			},
			Normalize: NormalizeJetStreamSubject,
		},
		{
			Kind:         store.RuntimeContractWSType,
			Name:         extractorNameWSType,
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

// compiledExtractor is a RuntimeExtractor with its regexes precompiled and its
// call-site methods indexed by role.
type compiledExtractor struct {
	config    *regexp.Regexp
	normalize func(string) string
	methods   map[string]store.ContractDirection
	def       RuntimeExtractor
	decode    bool
}

// compile precompiles the call-site extraction data for an extractor.
func (e RuntimeExtractor) compile() *compiledExtractor {
	ce := &compiledExtractor{def: e, normalize: e.Normalize}
	if len(e.ConfigPatterns) > 0 {
		ce.config = regexp.MustCompile(strings.Join(e.ConfigPatterns, "|"))
	}
	ce.decode = e.DecodeSwitch
	ce.methods = make(map[string]store.ContractDirection, len(e.ProducerMethods)+len(e.ConsumerMethods))
	for _, m := range e.ProducerMethods {
		ce.methods[m] = store.ContractDirectionProducer
	}
	for _, m := range e.ConsumerMethods {
		if _, ok := ce.methods[m]; !ok {
			ce.methods[m] = store.ContractDirectionConsumer
		}
	}
	if ce.normalize == nil {
		ce.normalize = NormalizeRaw
	}
	return ce
}

// MergeRuntimeExtractors applies custom extractors on top of base ones. A
// custom extractor whose Kind matches an existing extractor merges into it
// (union of import gates, methods, and config patterns, keeping the built-in
// fields when the custom entry does not set them); other entries are appended.
func MergeRuntimeExtractors(base, custom []RuntimeExtractor) []RuntimeExtractor {
	if len(custom) == 0 {
		return base
	}
	out := make([]RuntimeExtractor, 0, len(base)+len(custom))
	index := make(map[store.RuntimeContractKind]int, len(base)+len(custom))
	for _, b := range base {
		index[b.Kind] = len(out)
		out = append(out, b)
	}
	for _, c := range custom {
		if pos, ok := index[c.Kind]; ok {
			out[pos] = mergeRuntimeExtractor(out[pos], c)
			continue
		}
		index[c.Kind] = len(out)
		out = append(out, c)
	}
	return out
}

func mergeRuntimeExtractor(base, c RuntimeExtractor) RuntimeExtractor {
	m := base
	if c.Name != "" {
		m.Name = c.Name
	}
	if len(c.ImportsMatch) > 0 {
		m.ImportsMatch = unionStrings(base.ImportsMatch, c.ImportsMatch)
	}
	if len(c.ProducerMethods) > 0 {
		m.ProducerMethods = unionStrings(base.ProducerMethods, c.ProducerMethods)
	}
	if len(c.ConsumerMethods) > 0 {
		m.ConsumerMethods = unionStrings(base.ConsumerMethods, c.ConsumerMethods)
	}
	if len(c.ConfigPatterns) > 0 {
		m.ConfigPatterns = unionStrings(base.ConfigPatterns, c.ConfigPatterns)
	}
	if c.ConstPrefix != "" {
		m.ConstPrefix = c.ConstPrefix
	}
	m.DecodeSwitch = base.DecodeSwitch || c.DecodeSwitch
	if c.ArgIndex != 0 {
		m.ArgIndex = c.ArgIndex
	}
	if c.Normalize != nil || len(c.NormalizeRules) > 0 {
		m.Normalize = c.Normalize
		m.NormalizeRules = c.NormalizeRules
	}
	return m
}

func unionStrings(base, add []string) []string {
	if len(base) == 0 {
		return append([]string(nil), add...)
	}
	seen := make(map[string]bool, len(base)+len(add))
	out := make([]string, 0, len(base)+len(add))
	for _, s := range base {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range add {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// NormalizeWithRules chains an optional named built-in normalizer with ordered
// regex-replace rules. Rules are precompiled, so repeated normalization never
// recompiles a regex.
func NormalizeWithRules(base func(string) string, rules []NormalizeRule) func(string) string {
	return func(s string) string {
		if base != nil {
			s = base(s)
		}
		for _, r := range rules {
			s = r.Re.ReplaceAllString(s, r.Replace)
		}
		return s
	}
}

// Hoisted normalizer regexes: compiled once at package init so the hot path
// never calls regexp.MustCompile. The compile counter exists so tests can
// assert zero recompilation during repeated normalization.
var normalizerCompileCount atomic.Int64

func compileNormalizerRe(pattern string) *regexp.Regexp {
	normalizerCompileCount.Add(1)
	return regexp.MustCompile(pattern)
}

var (
	redisDateRe = compileNormalizerRe(`\d{4}-\d{2}-\d{2}`)
	redisIPRe   = compileNormalizerRe(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
	redisIDRe   = compileNormalizerRe(`[a-f0-9-]{36}`)
	redisNameRe = compileNormalizerRe(`<[^>]+>`)
	redisFmtRe  = compileNormalizerRe(`%[sdv]`)
	jsReqIDRe   = compileNormalizerRe(`<[^>]+>`)
	jsIDRe      = compileNormalizerRe(`[a-f0-9-]{36}`)
	jsFmtRe     = compileNormalizerRe(`%[sdv]`)
)

// NormalizeRedisKey normalizes a Redis key by replacing interpolated segments
// with :name placeholders. Concrete dates/IPs/UUIDs and common markers map to a
// canonical form, e.g. "krucil_size:2024-01-01:192.168.1.1" →
// "krucil_size:{date}:{ip}".
func NormalizeRedisKey(key string) string {
	key = strings.Trim(key, "\"`")
	key = redisDateRe.ReplaceAllString(key, "{date}")
	key = redisIPRe.ReplaceAllString(key, "{ip}")
	key = redisIDRe.ReplaceAllString(key, "{id}")
	key = redisNameRe.ReplaceAllString(key, ":{name}")
	key = redisFmtRe.ReplaceAllString(key, ":{name}")
	return key
}

// NormalizeJetStreamSubject normalizes a JetStream subject by replacing
// request-id-like interpolation segments.
func NormalizeJetStreamSubject(subject string) string {
	subject = strings.Trim(subject, "\"`")
	subject = jsReqIDRe.ReplaceAllString(subject, ":request_id")
	subject = jsIDRe.ReplaceAllString(subject, ":id")
	subject = jsFmtRe.ReplaceAllString(subject, ":request_id")
	return subject
}

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
		files := analysis.FileContents[repo]
		contracts = append(contracts, extractRepoContracts(compiled, symbols, files, fileImportPaths(files), repo, now)...)
	}
	return contracts
}

// fileImportPaths collects each file's import paths once per repo so the
// import gate never re-scans source for the same file.
func fileImportPaths(files map[string]string) map[string][]string {
	if len(files) == 0 {
		return nil
	}
	out := make(map[string][]string, len(files))
	for path, content := range files {
		out[path] = importPaths([]byte(content))
	}
	return out
}

func extractRepoContracts(extractors []*compiledExtractor, symbols map[string]store.Symbol, files map[string]string, imports map[string][]string, repo, now string) []store.RuntimeContract {
	contracts := make([]store.RuntimeContract, 0, len(symbols)*len(extractors))
	for _, ce := range extractors {
		contracts = append(contracts, extractConstContracts(ce, symbols, repo, now)...)
		contracts = append(contracts, extractCallSiteContracts(ce, symbols, files, imports, repo, now)...)
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

// extractCallSiteContracts scans each file for the extractor's call-site,
// config-literal, and decode-switch literals, gated by the extractor's import
// list and (for decode switches) ConstPrefix presence in the file.
func extractCallSiteContracts(ce *compiledExtractor, symbols map[string]store.Symbol, files map[string]string, imports map[string][]string, repo, now string) []store.RuntimeContract {
	if len(files) == 0 {
		return nil
	}
	contracts := make([]store.RuntimeContract, 0)
	for path, content := range files {
		if !allowedByImports(ce, imports[path]) {
			continue
		}
		if len(ce.methods) > 0 {
			contracts = append(contracts, extractCallLiterals(ce, symbols, []byte(content), path, repo, now)...)
		}
		if ce.config != nil {
			contracts = append(contracts, extractConfigLiterals(ce, symbols, content, path, repo, now)...)
		}
		if ce.decode && constPrefixPresent(ce, content, symbols, path) {
			contracts = append(contracts, extractDecodeSwitchLiterals(ce, symbols, content, path, repo, now)...)
		}
	}
	return contracts
}

// allowedByImports reports whether the extractor's import gate permits scanning
// a file. An empty ImportsMatch fires in any file.
func allowedByImports(ce *compiledExtractor, imports []string) bool {
	if len(ce.def.ImportsMatch) == 0 {
		return true
	}
	for _, imp := range imports {
		for _, sub := range ce.def.ImportsMatch {
			if strings.Contains(imp, sub) {
				return true
			}
		}
	}
	return false
}

// constPrefixPresent reports whether the file defines or references a constant
// whose name starts with the extractor's ConstPrefix. Decode-switch extraction
// is scoped to such files so unrelated string switches (HTTP statuses, error
// codes) are never treated as WS type contracts.
func constPrefixPresent(ce *compiledExtractor, content string, symbols map[string]store.Symbol, path string) bool {
	prefix := ce.def.ConstPrefix
	if prefix == "" {
		return true
	}
	for _, sym := range symbols {
		if sym.Kind == kindConst && sym.PosFile == path && strings.HasPrefix(sym.Name, prefix) {
			return true
		}
	}
	return contentHasIdentPrefix([]byte(content), prefix)
}

func extractCallLiterals(ce *compiledExtractor, symbols map[string]store.Symbol, src []byte, path, repo, now string) []store.RuntimeContract {
	var contracts []store.RuntimeContract
	for _, site := range findCallSites(src, ce.methods, ce.def.ArgIndex) {
		lineNo := bytes.Count(src[:site.offset], []byte{'\n'}) + 1
		fromRef := enclosingSymbol(symbols, path, lineNo)
		dir := ce.methods[site.method]
		for _, lit := range site.literals {
			contracts = append(contracts, newCallContract(ce, dir, lit, fromRef, path, lineNo, repo, now))
		}
	}
	return contracts
}

func extractConfigLiterals(ce *compiledExtractor, symbols map[string]store.Symbol, content, path, repo, now string) []store.RuntimeContract {
	var contracts []store.RuntimeContract
	for i, line := range strings.Split(content, "\n") {
		if m := ce.config.FindStringSubmatch(line); m != nil {
			fromRef := enclosingSymbol(symbols, path, i+1)
			contracts = append(contracts, newCallContract(ce, store.ContractDirectionProducer, m[1], fromRef, path, i+1, repo, now))
		}
	}
	return contracts
}

func extractDecodeSwitchLiterals(ce *compiledExtractor, symbols map[string]store.Symbol, content, path, repo, now string) []store.RuntimeContract {
	var contracts []store.RuntimeContract
	for i, line := range strings.Split(content, "\n") {
		lineNo := i + 1
		for _, lit := range collectCaseLiterals([]byte(line)) {
			fromRef := enclosingSymbol(symbols, path, lineNo)
			contracts = append(contracts, decodeCaseContract(ce, lit, fromRef, path, lineNo, repo, now))
		}
	}
	return contracts
}

// newCallContract builds a runtime contract for a call-site or config-literal
// match.
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

func decodeCaseContract(ce *compiledExtractor, literal, fromRef, path string, lineNo int, repo, now string) store.RuntimeContract {
	kind, name := ce.def.Kind, ce.def.Name
	if name == "" {
		name = string(kind)
	}
	return store.RuntimeContract{
		Kind:      kind,
		Pattern:   ce.normalize(literal),
		FromRef:   fromRef,
		Direction: store.ContractDirectionConsumer,
		Evidence:  fmt.Sprintf("%s decode switch case %q at %s:%d", name, literal, path, lineNo),
		IndexedAt: now,
		Repo:      repo,
	}
}

// callSite is one method call found by the literal scanner.
type callSite struct {
	method   string
	literals []string
	offset   int
}

// findCallSites scans source for every `.<method>(` call whose method is in the
// given set, and collects each call's string-literal arguments at positions
// >= argIndex. Arguments nested inside function calls or composite literals are
// skipped; interpolation never truncates a literal.
func findCallSites(src []byte, methods map[string]store.ContractDirection, argIndex int) []callSite {
	if len(src) == 0 || len(methods) == 0 {
		return nil
	}
	if argIndex < 0 {
		argIndex = 0 // runtime guard; config validation rejects negatives upstream
	}
	var sites []callSite
	for i := 0; i < len(src); {
		i = skipIgnored(src, i)
		if i >= len(src) {
			break
		}
		if src[i] != '.' {
			i++
			continue
		}
		m := i + 1
		j := m
		for j < len(src) && isIdentPart(src[j]) {
			j++
		}
		method := string(src[m:j])
		if _, ok := methods[method]; !ok {
			i = m
			continue
		}
		k := skipBrackets(src, j)
		k = skipWSAndComments(src, k)
		if k >= len(src) || src[k] != '(' || k == len(src)-1 {
			i = m
			continue
		}
		literals, end := scanCallArgs(src, k, argIndex)
		if len(literals) > 0 {
			sites = append(sites, callSite{method: method, offset: i, literals: literals})
		}
		i = end
	}
	return sites
}

// skipBrackets advances past an optional balanced generic type-argument list
// immediately following a method name (`.Set[K](...)`).
func skipBrackets(src []byte, i int) int {
	if i >= len(src) || src[i] != '[' {
		return i
	}
	depth := 0
	for i < len(src) {
		switch src[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '"', '`', '\'':
			i = skipStringLiteral(src, i)
			continue
		case '/':
			next := skipWSAndComments(src, i)
			if next == i {
				i++
			} else {
				i = next
			}
			continue
		}
		i++
	}
	return i
}

// scanCallArgs walks a call's balanced arguments starting at the opening paren
// and collects string-literal arguments at positions >= argIndex. Braces and
// brackets (composite literals, slices) are treated as nested scopes so their
// strings are not mistaken for arguments.
func scanCallArgs(src []byte, open, argIndex int) ([]string, int) {
	n := len(src)
	i := open + 1
	depth := 1
	arg := 0
	var literals []string
	for i < n {
		c := src[i]
		switch {
		case c == '(' || c == '[' || c == '{':
			depth++
			i++
		case c == ')' || c == ']' || c == '}':
			depth--
			if depth == 0 && c == ')' {
				return literals, i + 1
			}
			i++
		case c == '"' || c == '`':
			if depth == 1 && arg >= argIndex {
				lit, end := scanStringLiteral(src, i)
				literals = append(literals, lit)
				i = end
			} else {
				i = skipStringLiteral(src, i)
			}
		case c == '\'':
			i = skipStringLiteral(src, i)
		case c == ',' && depth == 1:
			arg++
			i++
		case c == '/':
			next := skipWSAndComments(src, i)
			if next == i {
				i++
			} else {
				i = next
			}
		default:
			i++
		}
	}
	return literals, i
}

// skipLineComment advances past a `//` comment starting at src[i].
func skipLineComment(src []byte, i int) int {
	for i < len(src) && src[i] != '\n' {
		i++
	}
	return i
}

// skipBlockComment advances past a `/* */` comment starting at src[i].
func skipBlockComment(src []byte, i int) int {
	i += 2
	for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
		i++
	}
	return i + 2
}

// skipWSAndComments advances past whitespace and comments only. String/rune
// literals are left in place (the caller decides what to do with them).
func skipWSAndComments(src []byte, i int) int {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '/':
			if i+1 >= len(src) {
				return i
			}
			switch src[i+1] {
			case '/':
				i = skipLineComment(src, i)
			case '*':
				i = skipBlockComment(src, i)
			default:
				return i
			}
		default:
			return i
		}
	}
	return i
}

// skipIgnored advances past whitespace, comments, and string/rune literals,
// leaving identifiers and punctuation for the caller.
func skipIgnored(src []byte, i int) int {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '/':
			if i+1 >= len(src) {
				return i
			}
			switch src[i+1] {
			case '/':
				i = skipLineComment(src, i)
			case '*':
				i = skipBlockComment(src, i)
			default:
				return i
			}
		case '"', '`', '\'':
			i = skipStringLiteral(src, i)
		default:
			return i
		}
	}
	return i
}

// skipStringLiteral returns the index just past the string literal that starts
// at src[i] (double-quoted, backtick, or rune literal), honoring backslash
// escapes for quoted literals.
func skipStringLiteral(src []byte, i int) int {
	quote := src[i]
	i++
	for i < len(src) {
		c := src[i]
		if c == '\\' && quote != '`' {
			i += 2
			continue
		}
		if c == quote {
			return i + 1
		}
		i++
	}
	return i
}

// scanStringLiteral returns the verbatim content of the literal that starts at
// src[i] and the index just past it. Content is returned as-is (no escape
// decoding) — the point is completeness, not Go-semantic values.
func scanStringLiteral(src []byte, i int) (string, int) {
	j := skipStringLiteral(src, i)
	return string(src[i+1 : j-1]), j
}

var caseKwRe = regexp.MustCompile(`case\s+`)

// collectCaseLiterals extracts the string-literal operand of every `case "x":`
// (or backtick) clause on a single line, using the shared literal scanner.
// Comment-only lines are skipped to avoid capturing commented-out cases.
func collectCaseLiterals(line []byte) []string {
	if len(line) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("//")) || bytes.HasPrefix(trimmed, []byte("/*")) {
		return nil
	}
	var out []string
	for _, idx := range caseKwRe.FindAllIndex(line, -1) {
		i := idx[1]
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i < len(line) && (line[i] == '"' || line[i] == '`') {
			lit, _ := scanStringLiteral(line, i)
			out = append(out, lit)
		}
	}
	return out
}

// contentHasIdentPrefix reports whether the source contains an identifier
// token starting with prefix (skipping strings and comments).
func contentHasIdentPrefix(src []byte, prefix string) bool {
	if prefix == "" {
		return false
	}
	for i := 0; i < len(src); {
		i = skipIgnored(src, i)
		if i >= len(src) {
			return false
		}
		if !isIdentStart(src[i]) {
			i++
			continue
		}
		j := i
		for j < len(src) && isIdentPart(src[j]) {
			j++
		}
		if len(src[i:j]) >= len(prefix) && string(src[i:i+len(prefix)]) == prefix {
			return true
		}
		i = j
	}
	return false
}

// importPaths extracts the import paths declared by Go source, stopping after
// the import section (at the first non-package, non-import top-level keyword).
func importPaths(src []byte) []string {
	for i := 0; i < len(src); {
		i = skipIgnored(src, i)
		if i >= len(src) || !isIdentStart(src[i]) {
			return nil
		}
		j := i
		for j < len(src) && isIdentPart(src[j]) {
			j++
		}
		switch string(src[i:j]) {
		case "import":
			return parseImportBlock(src, j)
		case "func", "type", "var", "const":
			return nil
		}
		i = j
	}
	return nil
}

func parseImportBlock(src []byte, i int) []string {
	i = skipWSAndComments(src, i)
	if i >= len(src) {
		return nil
	}
	switch src[i] {
	case '"', '`':
		lit, _ := scanStringLiteral(src, i)
		return []string{lit}
	case '(':
		i++
	default:
		return nil
	}
	depth := 1
	var paths []string
	for i < len(src) {
		switch src[i] {
		case '(':
			depth++
			i++
		case ')':
			depth--
			i++
			if depth == 0 {
				return paths
			}
		case '"', '`':
			lit, end := scanStringLiteral(src, i)
			paths = append(paths, lit)
			i = end
		default:
			next := skipWSAndComments(src, i)
			if next == i {
				i++
			} else {
				i = next
			}
		}
	}
	return paths
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
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
