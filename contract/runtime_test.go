package contract

import (
	"reflect"
	"testing"

	"codemap/store"
)

func TestNormalizeRedisKey(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"krucil_size:2024-01-01:192.168.1.1", "krucil_size:{date}:{ip}"},
		{"user:abc12345-1234-1234-1234-123456789abc", "user:{id}"},
		{"simple:key", "simple:key"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := NormalizeRedisKey(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestNormalizeJetStreamSubject(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"agent.response.<request_id>", "agent.response.:request_id"},
		{"agent.dlq.<server>", "agent.dlq.:request_id"},
		{"simple.subject", "simple.subject"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := NormalizeJetStreamSubject(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestDefaultRuntimeExtractorsIncludeBuiltins(t *testing.T) {
	extractors := DefaultRuntimeExtractors()
	kinds := make(map[store.RuntimeContractKind]bool)
	for _, e := range extractors {
		kinds[e.Kind] = true
	}
	for _, want := range []store.RuntimeContractKind{
		store.RuntimeContractRedis,
		store.RuntimeContractJetStream,
		store.RuntimeContractWSType,
	} {
		if !kinds[want] {
			t.Errorf("default extractors missing kind %s", want)
		}
	}
}

func TestExtractRuntimeContractsWSType(t *testing.T) {
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/types.MsgTypeAgentAsk": {
					QualifiedName: "repo-a/types.MsgTypeAgentAsk",
					Name:          "MsgTypeAgentAsk",
					Kind:          "const",
					Signature:     `= "agent_ask"`,
					Repo:          "repo-a",
				},
			},
		},
		Config: DefaultConfig(),
	}

	contracts := ExtractRuntimeContracts(analysis, "2024-01-01T00:00:00Z")
	if len(contracts) != 1 {
		t.Fatalf("expected 1 contract, got %d", len(contracts))
	}

	rc := contracts[0]
	if rc.Kind != store.RuntimeContractWSType {
		t.Errorf("expected ws_type kind, got %s", rc.Kind)
	}
	if rc.Pattern != "agent_ask" {
		t.Errorf("expected pattern 'agent_ask', got %q", rc.Pattern)
	}
}

func TestCustomExtractor(t *testing.T) {
	// A RabbitMQ-style extractor declared without any new Go code. The exchange
	// is a positional string before the routing key, so ArgIndex=1 targets the
	// routing key.
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/orders.publish": {
					QualifiedName: "repo-a/orders.publish",
					Name:          "publish",
					Kind:          "function",
					Signature:     `func ()`,
					PosFile:       "orders.go",
					PosLine:       1,
					Repo:          "repo-a",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {
				"orders.go": "func publish() {\n\tch.Publish(\"\", \"orders.created\", false, false)\n}\n",
			},
		},
		Config: Config{
			RuntimeExtractors: []RuntimeExtractor{
				{
					Kind:            store.RuntimeContractKind("rabbitmq"),
					Name:            "rabbitmq",
					ProducerMethods: []string{"Publish"},
					ArgIndex:        1,
					Normalize:       NormalizeRaw,
				},
			},
		},
	}

	contracts := ExtractRuntimeContracts(analysis, "2024-01-01T00:00:00Z")
	if len(contracts) != 1 {
		t.Fatalf("expected 1 contract, got %d", len(contracts))
	}
	rc := contracts[0]
	if rc.Kind != store.RuntimeContractKind("rabbitmq") {
		t.Errorf("expected rabbitmq kind, got %s", rc.Kind)
	}
	if rc.Pattern != "orders.created" {
		t.Errorf("expected pattern 'orders.created', got %q", rc.Pattern)
	}
	if rc.Direction != store.ContractDirectionProducer {
		t.Errorf("expected producer direction, got %s", rc.Direction)
	}
	if rc.FromRef != "repo-a/orders.publish" {
		t.Errorf("expected attribution to orders.publish, got %q", rc.FromRef)
	}
}

func TestFindCallSitesArgIndex(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		method   string
		argIndex int
		want     []string
	}{
		{"default captures first literal", `rdb.Set("cache:key", v)`, "Set", 0, []string{"cache:key"}},
		{"argIndex 1 skips positional prefix", `ch.Publish("", "orders.created", false, false)`, "Publish", 1, []string{"orders.created"}},
		{"argIndex 2 skips two literals", `q.F("a", "b", "orders.created", 3)`, "F", 2, []string{"orders.created"}},
		{"multi-key captures all keys", `rdb.MSet("k1", v1, "k2", v2)`, "MSet", 0, []string{"k1", "k2"}},
		{"interpolated literal not truncated", `rdb.Set("user:"+id, val)`, "Set", 0, []string{"user:"}},
		{"nested call strings are not arguments", `rdb.Set(fmt.Sprintf("skip:%d", n), "key")`, "Set", 0, []string{"key"}},
		{"composite literal strings are not arguments", `db.ExecContext(ctx, "SELECT * FROM t", map[string]any{"x": "y"})`, "ExecContext", 0, []string{"SELECT * FROM t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sites := findCallSites([]byte(tt.src), map[string]store.ContractDirection{tt.method: store.ContractDirectionProducer}, tt.argIndex)
			if len(sites) != 1 {
				t.Fatalf("expected 1 site, got %d", len(sites))
			}
			if !reflect.DeepEqual(sites[0].literals, tt.want) {
				t.Errorf("literals = %v, want %v", sites[0].literals, tt.want)
			}
		})
	}
}

func TestFindCallSitesIgnoresCommentsAndStrings(t *testing.T) {
	src := `package p

// rdb.Set("commented:key", 1)
var s = "str.Set(\"in:str\", 1)"
func f() { rdb.Set("real:key", 1) }
`
	sites := findCallSites([]byte(src), map[string]store.ContractDirection{"Set": store.ContractDirectionProducer}, 0)
	if len(sites) != 1 {
		t.Fatalf("expected exactly 1 site (comment/string hits are ignored), got %d", len(sites))
	}
	if !reflect.DeepEqual(sites[0].literals, []string{"real:key"}) {
		t.Errorf("literals = %v, want [real:key]", sites[0].literals)
	}
}

func TestImportPaths(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			"single import",
			`package p
import "github.com/redis/go-redis/v9"
func f() {}`,
			[]string{"github.com/redis/go-redis/v9"},
		},
		{
			"grouped with aliases and blank",
			`package p
import (
	"database/sql"
	alias "github.com/valkey-io/valkey-go"
	_ "embed"
)
func f() {}`,
			[]string{"database/sql", "github.com/valkey-io/valkey-go", "embed"},
		},
		{
			"no imports",
			`package p
func f() {}`,
			nil,
		},
		{
			"import word in comment/string ignored",
			`package p
// import "fake"
var s = "import \"fake2\""
func f() {}`,
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := importPaths([]byte(tt.src))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("importPaths = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReceiverGating(t *testing.T) {
	mkAnalysis := func(content string) *Analysis {
		return &Analysis{
			SymbolsByRepo: map[string]map[string]store.Symbol{
				"repo-a": {
					"repo-a/app.func": {
						QualifiedName: "repo-a/app.func",
						Name:          "func",
						Kind:          "function",
						Signature:     "func ()",
						PosFile:       "app.go",
						PosLine:       1,
						Repo:          "repo-a",
					},
				},
			},
			FileContents: map[string]map[string]string{
				"repo-a": {"app.go": content},
			},
			Config: DefaultConfig(),
		}
	}

	// 1. UI struct in a file without a Redis-protocol import must NOT fire.
	c := ExtractRuntimeContracts(mkAnalysis(`package app
type UI struct{}
func (u UI) Set(k, v string) {}
func run(u UI) { u.Set("theme", "dark") }
`), "2024-01-01T00:00:00Z")
	for _, rc := range c {
		if rc.Kind == store.RuntimeContractRedis {
			t.Errorf("redis fired on a non-redis file: %s at %s", rc.Pattern, rc.Evidence)
		}
	}

	// 2. go-redis file fires.
	c = ExtractRuntimeContracts(mkAnalysis(`package app
import "github.com/redis/go-redis/v9"
func run(r *redis.Client) { r.Set("krucil", "val") }
`), "2024-01-01T00:00:00Z")
	if !hasContract(c, store.RuntimeContractRedis, "krucil") {
		t.Errorf("go-redis file did not fire the redis extractor: %+v", c)
	}

	// 3. valkey-go file fires as redis kind (protocol-family gating).
	c = ExtractRuntimeContracts(mkAnalysis(`package app
import "github.com/valkey-io/valkey-go"
func run(client *valkey.Client) { client.Set(ctx, "user:1", v) }
`), "2024-01-01T00:00:00Z")
	if !hasContract(c, store.RuntimeContractRedis, "user:1") {
		t.Errorf("valkey-go file did not fire the redis extractor: %+v", c)
	}
}

func hasContract(contracts []store.RuntimeContract, kind store.RuntimeContractKind, pattern string) bool {
	for _, c := range contracts {
		if c.Kind == kind && c.Pattern == pattern {
			return true
		}
	}
	return false
}

func TestCrossProtocolSeparation(t *testing.T) {
	// A rabbitmq file must NOT fire the jetstream extractor, and an mqtt file
	// must only fire an mqtt extractor — never jetstream.
	rabbitmq := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/orders.publish": {
					QualifiedName: "repo-a/orders.publish",
					Name:          "publish",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "orders.go",
					PosLine:       1,
					Repo:          "repo-a",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {"orders.go": "import \"github.com/rabbitmq/amqp091-go\"\nfunc publish(ch *amqp091.Channel) {\n\tch.Publish(\"\", \"orders.created\", false, false)\n}\n"},
		},
		Config: Config{
			RuntimeExtractors: []RuntimeExtractor{
				{
					Kind:            store.RuntimeContractKind("rabbitmq"),
					Name:            "rabbitmq",
					ProducerMethods: []string{"Publish"},
					ImportsMatch:    []string{"amqp"},
					ArgIndex:        1,
					Normalize:       NormalizeRaw,
				},
			},
		},
	}

	// Built-ins only: no amqp import matches the nats gate → no jetstream.
	builtinOnly := *rabbitmq // shallow copy
	builtinOnly.Config = DefaultConfig()
	if c := ExtractRuntimeContracts(&builtinOnly, "2024-01-01T00:00:00Z"); len(c) != 0 {
		t.Errorf("rabbitmq file fired built-in extractors: %+v", c)
	}
	// With the rabbitmq extractor configured, only the rabbitmq kind fires.
	withExtractor := ExtractRuntimeContracts(rabbitmq, "2024-01-01T00:00:00Z")
	for _, c := range withExtractor {
		if c.Kind == store.RuntimeContractJetStream {
			t.Errorf("rabbitmq file fired jetstream: %+v", c)
		}
	}
	if !hasContract(withExtractor, store.RuntimeContractKind("rabbitmq"), "orders.created") {
		t.Errorf("rabbitmq extractor did not fire: %+v", withExtractor)
	}

	// paho-mqtt file: only an mqtt extractor fires.
	mqttCfg := Config{
		RuntimeExtractors: []RuntimeExtractor{
			{
				Kind:            store.RuntimeContractKind("mqtt"),
				Name:            "mqtt",
				ProducerMethods: []string{"Publish"},
				ImportsMatch:    []string{"mqtt"},
				Normalize:       NormalizeRaw,
			},
		},
	}
	mqtt := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/sensors.publish": {
					QualifiedName: "repo-a/sensors.publish",
					Name:          "publish",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "sensors.go",
					PosLine:       1,
					Repo:          "repo-a",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {"sensors.go": "import \"github.com/eclipse/paho.mqtt.golang\"\nfunc publish(c *mqtt.Client) {\n\tc.Publish(\"sensors/temp\", 0, false, \"\")\n}\n"},
		},
		Config: mqttCfg,
	}
	mqttContracts := ExtractRuntimeContracts(mqtt, "2024-01-01T00:00:00Z")
	for _, c := range mqttContracts {
		if c.Kind == store.RuntimeContractJetStream {
			t.Errorf("mqtt file fired jetstream: %+v", c)
		}
	}
	if !hasContract(mqttContracts, store.RuntimeContractKind("mqtt"), "sensors/temp") {
		t.Errorf("mqtt extractor did not fire: %+v", mqttContracts)
	}
}

func TestCustomExtractorWithConstPrefix(t *testing.T) {
	// Kafka-style topic constants declared by prefix, no call-site needed.
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/topics.TopicOrderCreated": {
					QualifiedName: "repo-a/topics.TopicOrderCreated",
					Name:          "TopicOrderCreated",
					Kind:          "const",
					Signature:     `= "orders.created"`,
					Repo:          "repo-a",
				},
			},
		},
		Config: Config{
			RuntimeExtractors: []RuntimeExtractor{
				{
					Kind:        store.RuntimeContractKind("kafka"),
					Name:        "kafka",
					ConstPrefix: "Topic",
					Normalize:   NormalizeRaw,
				},
			},
		},
	}

	contracts := ExtractRuntimeContracts(analysis, "2024-01-01T00:00:00Z")
	if len(contracts) != 1 {
		t.Fatalf("expected 1 contract, got %d", len(contracts))
	}
	rc := contracts[0]
	if rc.Kind != store.RuntimeContractKind("kafka") {
		t.Errorf("expected kafka kind, got %s", rc.Kind)
	}
	if rc.Pattern != "orders.created" {
		t.Errorf("expected pattern 'orders.created', got %q", rc.Pattern)
	}
	if rc.Direction != store.ContractDirectionShared {
		t.Errorf("expected shared direction, got %s", rc.Direction)
	}
}

func TestMergeRuntimeExtractorsCustomClient(t *testing.T) {
	// A workspace extends the redis extractor so its mycompany/redishlib client
	// fires the redis kind while the built-in protocol family is preserved.
	base := DefaultRuntimeExtractors()
	custom := []RuntimeExtractor{
		{
			Kind:            store.RuntimeContractRedis,
			Name:            "redis",
			ProducerMethods: []string{"Set"},
			ImportsMatch:    []string{"mycompany/redishlib"},
		},
	}
	merged := MergeRuntimeExtractors(base, custom)

	var redis RuntimeExtractor
	for _, e := range merged {
		if e.Kind == store.RuntimeContractRedis {
			redis = e
		}
	}
	if redis.Kind != store.RuntimeContractRedis {
		t.Fatal("merged extractors lost the redis kind")
	}
	want := []string{"redis", "valkey", "rueidis", "keydb", "mycompany/redishlib"}
	if !reflect.DeepEqual(redis.ImportsMatch, want) {
		t.Errorf("merged redis gate = %v, want %v", redis.ImportsMatch, want)
	}
	if redis.Normalize == nil {
		t.Error("merge dropped the built-in redis normalizer when config set no normalize")
	}

	// The merged gate lets a custom-client file fire.
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/app.run": {
					QualifiedName: "repo-a/app.run",
					Name:          "run",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "app.go",
					PosLine:       1,
					Repo:          "repo-a",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {"app.go": "import \"mycompany/redishlib\"\nfunc run(c *redishlib.Client) { c.Set(\"custom:key\", 1) }\n"},
		},
		Config: Config{RuntimeExtractors: merged},
	}
	contracts := ExtractRuntimeContracts(analysis, "2024-01-01T00:00:00Z")
	if !hasContract(contracts, store.RuntimeContractRedis, "custom:key") {
		t.Errorf("custom client did not fire the merged redis extractor: %+v", contracts)
	}
}

func TestWSScoping(t *testing.T) {
	// A file with a string switch but no MsgType constant must NOT yield ws_type.
	httpStatus := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/http.handle": {
					QualifiedName: "repo-a/http.handle",
					Name:          "handle",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "http.go",
					PosLine:       1,
					Repo:          "repo-a",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {"http.go": "package http\nfunc handle(msg string) {\n\tswitch msg {\n\tcase \"404\":\n\t}\n}\n"},
		},
		Config: DefaultConfig(),
	}
	for _, c := range ExtractRuntimeContracts(httpStatus, "2024-01-01T00:00:00Z") {
		if c.Kind == store.RuntimeContractWSType {
			t.Errorf("HTTP-status switch produced a ws_type contract: %+v", c)
		}
	}

	// A file declaring a MsgType constant and switching on it fires.
	wsFile := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/types.MsgTypeAgentAsk": {
					QualifiedName: "repo-a/types.MsgTypeAgentAsk",
					Name:          "MsgTypeAgentAsk",
					Kind:          "const",
					Signature:     `= "agent_ask"`,
					PosFile:       "types/ws.go",
					PosLine:       2,
					Repo:          "repo-a",
				},
				"repo-a/types.decode": {
					QualifiedName: "repo-a/types.decode",
					Name:          "decode",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "types/ws.go",
					PosLine:       4,
					Repo:          "repo-a",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {"types/ws.go": "package types\n\nconst MsgTypeAgentAsk = \"agent_ask\"\n\nfunc decode(m string) {\n\tswitch m {\n\tcase \"agent_ask\":\n\t}\n}\n"},
		},
		Config: DefaultConfig(),
	}
	var found bool
	for _, c := range ExtractRuntimeContracts(wsFile, "2024-01-01T00:00:00Z") {
		if c.Kind == store.RuntimeContractWSType && c.Pattern == "agent_ask" && c.Direction == store.ContractDirectionConsumer {
			found = true
		}
	}
	if !found {
		t.Errorf("decode switch in a MsgType file did not produce a ws_type consumer contract")
	}
}

func TestNormalizeRules(t *testing.T) {
	rules := []NormalizeRule{
		{Re: compileNormalizerRe(`user:\d+`), Replace: "user:{id}"},
		{Re: compileNormalizerRe(`:`), Replace: "/"},
	}
	normalize := NormalizeWithRules(NormalizeRaw, rules)
	if got := normalize("user:123"); got != "user/{id}" {
		t.Errorf("ordered rule application = %q, want %q", got, "user/{id}")
	}

	// Named built-in runs first, then rules (determinism).
	redisRules := []NormalizeRule{{Re: compileNormalizerRe(`:`), Replace: "/"}}
	withBase := NormalizeWithRules(NormalizeRedisKey, redisRules)
	if got := withBase("krucil_size:2024-01-01"); got != "krucil_size/{date}" {
		t.Errorf("base-then-rule chaining = %q, want %q", got, "krucil_size/{date}")
	}
}

func TestPostgresExtractorViaConfigRules(t *testing.T) {
	// A postgres extractor is fully describable in config: method list, import
	// gate, and inline normalize_rules. No codemap Go code is involved.
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/api.query": {
					QualifiedName: "repo-a/api.query",
					Name:          "query",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "api.go",
					PosLine:       1,
					Repo:          "repo-a",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {"api.go": "import \"database/sql\"\nfunc query(db *sql.DB) {\n\tdb.ExecContext(ctx, \"SELECT * FROM logs_2024-01-01\")\n}\n"},
		},
		Config: Config{
			RuntimeExtractors: []RuntimeExtractor{
				{
					Kind:            store.RuntimeContractKind("postgres"),
					Name:            "postgres",
					ConsumerMethods: []string{"ExecContext", "QueryContext"},
					ImportsMatch:    []string{"database/sql"},
					Normalize: NormalizeWithRules(NormalizeRaw, []NormalizeRule{
						{Re: compileNormalizerRe(`\d{4}-\d{2}-\d{2}`), Replace: "{date}"},
					}),
				},
			},
		},
	}
	contracts := ExtractRuntimeContracts(analysis, "2024-01-01T00:00:00Z")
	if !hasContract(contracts, store.RuntimeContractKind("postgres"), "SELECT * FROM logs_{date}") {
		t.Errorf("postgres extractor did not normalize via rules: %+v", contracts)
	}
}

func TestKafkaConstPrefixWithNormalizeRules(t *testing.T) {
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/topics.TopicOrderCreated": {
					QualifiedName: "repo-a/topics.TopicOrderCreated",
					Name:          "TopicOrderCreated",
					Kind:          "const",
					Signature:     `= "orders.2024"`,
					Repo:          "repo-a",
				},
			},
		},
		Config: Config{
			RuntimeExtractors: []RuntimeExtractor{
				{
					Kind:        store.RuntimeContractKind("kafka"),
					Name:        "kafka",
					ConstPrefix: "Topic",
					Normalize: NormalizeWithRules(NormalizeRaw, []NormalizeRule{
						{Re: compileNormalizerRe(`\.\d{4}`), Replace: ".{year}"},
					}),
				},
			},
		},
	}
	contracts := ExtractRuntimeContracts(analysis, "2024-01-01T00:00:00Z")
	if !hasContract(contracts, store.RuntimeContractKind("kafka"), "orders.{year}") {
		t.Errorf("kafka const + rules did not normalize: %+v", contracts)
	}
}

func TestTypedConstantRecognition(t *testing.T) {
	for sig, want := range map[string]string{
		`= "agent_ask"`:                         "agent_ask",
		`"agent_ask"`:                           "agent_ask",
		`MsgTypeAgentAsk MsgType = "agent_ask"`: "agent_ask",
		`const Name = "x"`:                      "x",
	} {
		if got := extractStringConst(sig); got != want {
			t.Errorf("extractStringConst(%q) = %q, want %q", sig, got, want)
		}
	}

	// The typed constant's value participates in shared-constant pairing.
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/types.MsgTypeAgentAsk": {
					QualifiedName: "repo-a/types.MsgTypeAgentAsk",
					Name:          "MsgTypeAgentAsk",
					Kind:          "const",
					Signature:     `MsgTypeAgentAsk MsgType = "agent_ask"`,
					Repo:          "repo-a",
				},
			},
		},
		Config: DefaultConfig(),
	}
	contracts := ExtractRuntimeContracts(analysis, "2024-01-01T00:00:00Z")
	if !hasContract(contracts, store.RuntimeContractWSType, "agent_ask") {
		t.Errorf("typed constant value did not produce a ws_type contract: %+v", contracts)
	}
}

func TestNormalizerNoRecompilation(t *testing.T) {
	before := normalizerCompileCount.Load()
	for range 5000 {
		NormalizeRedisKey("krucil_size:2024-01-01:10.0.0.1")
		NormalizeJetStreamSubject("agent.response.<request_id>")
	}
	if after := normalizerCompileCount.Load(); after != before {
		t.Errorf("normalizers compiled %d regexes during 5000 calls (before=%d after=%d)", after-before, before, after)
	}
}

func TestSiteIndexDirectionInference(t *testing.T) {
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{
			"repo-a": {
				"repo-a/typ.MsgTypeAgentAsk": {
					QualifiedName: "repo-a/typ.MsgTypeAgentAsk",
					Name:          "MsgTypeAgentAsk",
					Kind:          "const",
					Signature:     `= "agent_ask"`,
					PosFile:       "a.go",
					PosLine:       2,
					PackagePath:   "repo-a/typ",
					Repo:          "repo-a",
				},
				"repo-a/typ.EncodeAgentAsk": {
					QualifiedName: "repo-a/typ.EncodeAgentAsk",
					Name:          "EncodeAgentAsk",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "a.go",
					PosLine:       4,
					PackagePath:   "repo-a/typ",
					Repo:          "repo-a",
				},
			},
			"repo-b": {
				"repo-b/typ.MsgTypeAgentAsk": {
					QualifiedName: "repo-b/typ.MsgTypeAgentAsk",
					Name:          "MsgTypeAgentAsk",
					Kind:          "const",
					Signature:     `= "agent_ask"`,
					PosFile:       "b.go",
					PosLine:       2,
					PackagePath:   "repo-b/typ",
					Repo:          "repo-b",
				},
				"repo-b/typ.DecodeAgentAsk": {
					QualifiedName: "repo-b/typ.DecodeAgentAsk",
					Name:          "DecodeAgentAsk",
					Kind:          "function",
					Signature:     "func ()",
					PosFile:       "b.go",
					PosLine:       4,
					PackagePath:   "repo-b/typ",
					Repo:          "repo-b",
				},
			},
		},
		FileContents: map[string]map[string]string{
			"repo-a": {"a.go": "package typ\n\nconst MsgTypeAgentAsk = \"agent_ask\"\n\nfunc EncodeAgentAsk() { _ = MsgTypeAgentAsk }\n"},
			"repo-b": {"b.go": "package typ\n\nconst MsgTypeAgentAsk = \"agent_ask\"\n\nfunc DecodeAgentAsk() { _ = MsgTypeAgentAsk }\n"},
		},
	}
	idx := buildAnalysisIndex(analysis)
	dir := InferContractDirection(idx, analysis, "repo-a/typ.MsgTypeAgentAsk", "repo-b/typ.MsgTypeAgentAsk")
	if dir != store.ContractDirectionProducer {
		t.Errorf("direction = %q, want producer", dir)
	}
}

func TestSuggestedPairsPrefilter(t *testing.T) {
	analysis := &Analysis{
		SymbolsByRepo: map[string]map[string]store.Symbol{},
		StructsByRepo: map[string]map[string]StructInfo{
			"ra": {
				"ra/A": {QualifiedName: "ra/A", PackagePath: "ra", Repo: "ra", Fields: []FieldInfo{
					{GoName: "X", WireName: "x", GoType: "string"},
					{GoName: "Y", WireName: "y", GoType: "string"},
				}},
				"ra/B": {QualifiedName: "ra/B", PackagePath: "ra", Repo: "ra", Fields: []FieldInfo{
					{GoName: "P", WireName: "p", GoType: "string"},
					{GoName: "Q", WireName: "q", GoType: "string"},
				}},
			},
			"rb": {
				"rb/C": {QualifiedName: "rb/C", PackagePath: "rb", Repo: "rb", Fields: []FieldInfo{
					{GoName: "X", WireName: "x", GoType: "string"},
					{GoName: "Y", WireName: "y", GoType: "string"},
				}},
			},
			"rc": {
				"rc/D": {QualifiedName: "rc/D", PackagePath: "rc", Repo: "rc", Fields: []FieldInfo{
					{GoName: "P", WireName: "p", GoType: "string"},
					{GoName: "Q", WireName: "q", GoType: "string"},
				}},
			},
			"rd": {
				"rd/E": {QualifiedName: "rd/E", PackagePath: "rd", Repo: "rd", Fields: []FieldInfo{
					{GoName: "M", WireName: "m", GoType: "string"},
					{GoName: "N", WireName: "n", GoType: "string"},
				}},
			},
		},
	}
	idx := buildAnalysisIndex(analysis)
	before := shapeCompareCount.Load()
	contracts := suggestedShapePairs(analysis, idx, "2024-01-01T00:00:00Z")
	compared := shapeCompareCount.Load() - before

	// Only (ra/A, rb/C) and (ra/B, rc/D) share a wire field; rd/E is disjoint
	// from everything, so it must never be shape-compared.
	if len(contracts) != 2 {
		t.Errorf("expected 2 suggested pairs, got %d: %+v", len(contracts), contracts)
	}
	if compared > 2 {
		t.Errorf("pair loop ran %d shape comparisons; field-multiset filter should skip disjoint pairs", compared)
	}
}
