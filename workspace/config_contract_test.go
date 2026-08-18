package workspace

import (
	"reflect"
	"strings"
	"testing"

	"codemap/contract"
	"codemap/store"
)

func TestContractConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name:    "negative arg_index names extractor",
			cfg:     &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{{Name: "rabbit", Kind: "rabbitmq", ArgIndex: -1}}}},
			wantErr: "rabbit",
		},
		{
			name:    "negative arg_index mentions arg_index",
			cfg:     &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{{Name: "rabbit", Kind: "rabbitmq", ArgIndex: -1}}}},
			wantErr: "arg_index",
		},
		{
			name:    "empty kind rejected",
			cfg:     &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{{Name: "x", Kind: ""}}}},
			wantErr: "empty kind",
		},
		{
			name:    "malformed suppress separator errors with the arrow",
			cfg:     &Config{Contracts: ContractsConfig{Suppress: []string{"a.b.C -> d.e.F"}}},
			wantErr: "→",
		},
		{
			name:    "empty suppress side errors",
			cfg:     &Config{Contracts: ContractsConfig{Suppress: []string{"a.b.C →"}}},
			wantErr: "empty side",
		},
		{
			name:    "invalid normalize_rules regex names the rule index",
			cfg:     &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{{Name: "s3", Kind: "s3", NormalizeRules: []NormalizeRuleConfig{{Regex: "["}}}}}},
			wantErr: "normalize_rules[0]",
		},
		{
			name:    "empty normalize_rules regex names the rule index",
			cfg:     &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{{Name: "s3", Kind: "s3", NormalizeRules: []NormalizeRuleConfig{{Regex: ""}}}}}},
			wantErr: "normalize_rules[0]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.cfg.ContractConfig()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestContractConfigUnknownNormalizeWarns(t *testing.T) {
	cfg := &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{{Name: "redis", Kind: "redis", Normalize: "no-such-normalizer"}}}}
	c, err := cfg.ContractConfig()
	if err != nil {
		t.Fatalf("unknown normalize must warn, not error: %v", err)
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), "no-such-normalizer") {
		t.Errorf("warning did not name the unknown normalize: %v", c.Warnings)
	}
	var redis contract.RuntimeExtractor
	for _, e := range c.RuntimeExtractors {
		if e.Kind == store.RuntimeContractRedis {
			redis = e
		}
	}
	if redis.Normalize == nil {
		t.Fatal("unknown normalize must fall back to a working normalizer")
	}
	if got := redis.Normalize(`"x"`); got != "x" {
		t.Errorf("fallback normalizer = %q, want quote-strip %q", got, "x")
	}
}

func TestContractConfigValidSuppress(t *testing.T) {
	cfg := &Config{Contracts: ContractsConfig{Suppress: []string{"repo-a/x.A → repo-b/y.B"}}}
	c, err := cfg.ContractConfig()
	if err != nil {
		t.Fatalf("valid suppress entry must build: %v", err)
	}
	if !c.SuppressPairs["repo-a/x.A → repo-b/y.B"] {
		t.Errorf("valid suppress pair missing from config: %v", c.SuppressPairs)
	}
}

func TestContractConfigExtendsBuiltinGate(t *testing.T) {
	// A config entry with kind redis extends the built-in redis extractor's
	// import gate so a bespoke client library fires the redis kind.
	cfg := &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{
		{Name: "redis", Kind: "redis", ImportsMatch: []string{"mycompany/redishlib"}},
	}}}
	c, err := cfg.ContractConfig()
	if err != nil {
		t.Fatalf("config build: %v", err)
	}
	var redis contract.RuntimeExtractor
	for _, e := range c.RuntimeExtractors {
		if e.Kind == store.RuntimeContractRedis {
			redis = e
		}
	}
	want := []string{"redis", "valkey", "rueidis", "keydb", "mycompany/redishlib"}
	if !reflect.DeepEqual(redis.ImportsMatch, want) {
		t.Errorf("merged redis gate = %v, want %v", redis.ImportsMatch, want)
	}
}

func TestContractConfigPostgresViaYAMLOnly(t *testing.T) {
	// A postgres extractor is a pure YAML entry: methods, import gate, and
	// inline normalization rules. No codemap Go code changes.
	cfg := &Config{Contracts: ContractsConfig{Extractors: []ExtractorConfig{
		{
			Name:            "postgres",
			Kind:            "postgres",
			ConsumerMethods: []string{"ExecContext", "QueryContext"},
			ImportsMatch:    []string{"database/sql"},
			NormalizeRules: []NormalizeRuleConfig{
				{Regex: `\d{4}-\d{2}-\d{2}`, Replace: "{date}"},
			},
		},
	}}}
	c, err := cfg.ContractConfig()
	if err != nil {
		t.Fatalf("config build: %v", err)
	}
	var pg contract.RuntimeExtractor
	for _, e := range c.RuntimeExtractors {
		if e.Kind == store.RuntimeContractKind("postgres") {
			pg = e
		}
	}
	if pg.Normalize == nil || len(pg.NormalizeRules) != 1 {
		t.Fatal("postgres extractor missing configured normalization")
	}
	if got := pg.Normalize("SELECT * FROM logs_2024-01-01"); got != "SELECT * FROM logs_{date}" {
		t.Errorf("configured rules did not normalize: %q", got)
	}
}
