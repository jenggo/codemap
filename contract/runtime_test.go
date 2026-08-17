package contract

import (
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

func TestCustomExtractorArgIndex(t *testing.T) {
	// Same method, both ArgIndex=0 (default) and ArgIndex=1 paths.
	tests := []struct {
		name     string
		line     string
		argIndex int
		want     string
	}{
		{"default captures first literal", `rdb.Set("cache:key", v)`, 0, "cache:key"},
		{"argIndex 1 skips positional prefix", `ch.Publish("", "orders.created", false, false)`, 1, "orders.created"},
		{"argIndex 2 skips two literals", `q.F("a", "b", "orders.created", 3)`, 2, "orders.created"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce := RuntimeExtractor{
				Kind:            store.RuntimeContractKind("test"),
				Name:            "test",
				ProducerMethods: []string{"Set", "Publish", "F"},
				ArgIndex:        tt.argIndex,
				Normalize:       NormalizeRaw,
			}.compile()
			m := ce.producer.FindStringSubmatch(tt.line)
			if m == nil {
				t.Fatalf("no match for %q (argIndex=%d)", tt.line, tt.argIndex)
			}
			if m[1] != tt.want {
				t.Errorf("captured %q, want %q", m[1], tt.want)
			}
		})
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
