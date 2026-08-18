package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"codemap/contract"
	"codemap/store"
)

// Member declares a single repo in a workspace. The stable identity is the
// module path, not the directory: the same module reached via a symlink or a
// moved tree resolves to one repo.
type Member struct {
	// Module is the module path (e.g. "krucil" or "github.com/jenggo/server").
	Module string `yaml:"module"`
	// Dir is the checkout directory, relative to the workspace root or absolute.
	Dir string `yaml:"dir"`
	// DiscoveryRoot is an optional directory scanned by --discover to find
	// sibling Go modules (module-path-prefix match).
	DiscoveryRoot string `yaml:"discovery_root,omitempty"`
	// Missing is true when Dir does not exist. Missing members are kept in the
	// workspace, marked missing, and reported as "repo not indexed" in queries.
	Missing bool
}

// ExtractorConfig declares a custom runtime-contract extractor in codemap.yaml.
// Adding an infrastructure (Postgres, RabbitMQ, Kafka, ...) is a config entry,
// not new Go code.
type ExtractorConfig struct {
	// Name identifies the extractor in evidence and diagnostics.
	Name string `yaml:"name"`
	// Kind is the runtime contract kind (e.g. "redis", "postgres", "rabbitmq").
	Kind string `yaml:"kind"`
	// ConstPrefix, when set, marks string constants whose symbol name starts
	// with it as shared entities.
	ConstPrefix string `yaml:"const_prefix"`
	// Normalize selects a built-in normalizer by name: "raw", "redis", or
	// "subject". Empty means the raw (quote-strip) normalizer.
	Normalize string `yaml:"normalize"`
	// NormalizeRules applies ordered regex-replace rules on top of Normalize,
	// so any pattern shape (DSNs, topic ARNs, bucket names) normalizes without
	// Go changes.
	NormalizeRules []NormalizeRuleConfig `yaml:"normalize_rules"`
	// ImportsMatch gates call-site extraction to files whose import block
	// contains at least one of these substrings. Empty fires in any file. When
	// merging into a built-in extractor of the same kind, the configured list
	// is added to the built-in's default gate.
	ImportsMatch []string `yaml:"imports_match"`
	// ProducerMethods are method names whose entity string-literal arguments
	// are produced entities (write/publish side).
	ProducerMethods []string `yaml:"producer_methods"`
	// ConsumerMethods are method names whose entity string-literal arguments are
	// consumed entities (read/subscribe side).
	ConsumerMethods []string `yaml:"consumer_methods"`
	// ConfigPatterns are regexes matching config literals that name a produced
	// entity; capture group 1 is the literal.
	ConfigPatterns []string `yaml:"config_patterns"`
	// ArgIndex selects the first string-literal argument holding the entity,
	// 0-based; every string-literal argument from it to the call end is
	// captured. RabbitMQ-style APIs that put a positional string (e.g. the
	// exchange) before the routing key need 1.
	ArgIndex int `yaml:"arg_index"`
	// DecodeSwitch, when true, captures `case "..."` literals as consumers.
	DecodeSwitch bool `yaml:"decode_switch"`
}

// NormalizeRuleConfig is one ordered regex-replace normalization rule.
type NormalizeRuleConfig struct {
	Regex   string `yaml:"regex"`
	Replace string `yaml:"replace"`
}

// ContractsConfig holds contract-intelligence settings.
type ContractsConfig struct {
	// Suppress lists contract pairs to drop from every result. Each entry is a
	// "from → to" pair of qualified symbol names, e.g.
	//   suppress:
	//     - "repo-a/pkg.SymA → repo-b/pkg.SymB"
	Suppress []string `yaml:"suppress"`
	// Extractors adds custom runtime-contract extractors on top of the built-in
	// redis/jetstream/ws defaults.
	Extractors []ExtractorConfig `yaml:"extractors"`
}

// Config is a parsed codemap.yaml.
type Config struct {
	// Root is the absolute directory containing the config file.
	Root string
	// Path is the config file path that was loaded.
	Path      string
	Members   []Member        `yaml:"members"`
	Contracts ContractsConfig `yaml:"contracts"`
}

// SuppressPairs converts the suppress list into a lookup map keyed by the
// "from → to" direction written in the config.
func (c *Config) SuppressPairs() (map[string]bool, error) {
	pairs, err := c.ContractSuppressionPairs()
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		out[p[0]+" → "+p[1]] = true
	}
	return out, nil
}

// ContractSuppressionPairs returns the parsed from/to pairs from the
// contracts.suppress list. A malformed entry (missing the U+2192 '→'
// separator, or an empty side) is an error naming the offending entry.
func (c *Config) ContractSuppressionPairs() ([][2]string, error) {
	if len(c.Contracts.Suppress) == 0 {
		return nil, nil
	}
	var out [][2]string
	for i, entry := range c.Contracts.Suppress {
		from, to, ok := strings.Cut(entry, "→")
		if !ok {
			return nil, fmt.Errorf("workspace config %s: contracts.suppress[%d] %q is missing the separator '→' (U+2192); expected e.g. \"a.b.C → d.e.F\"", c.Path, i, entry)
		}
		from = strings.TrimSpace(from)
		to = strings.TrimSpace(to)
		if from == "" || to == "" {
			return nil, fmt.Errorf("workspace config %s: contracts.suppress[%d] %q has an empty side", c.Path, i, entry)
		}
		out = append(out, [2]string{from, to})
	}
	return out, nil
}

const configFileName = "codemap.yaml"

// DefaultPath returns the path of the codemap.yaml nearest to startDir,
// searching startDir and each ancestor. It returns "" when none is found.
func DefaultPath(startDir string) string {
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return ""
	}
	dir := abs
	for {
		candidate := filepath.Join(dir, configFileName)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Load reads and resolves a workspace config. path may point at the
// codemap.yaml file itself or at a directory containing one.
func Load(path string) (*Config, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("workspace config: %w", err)
	}
	filePath := path
	if fi.IsDir() {
		filePath = filepath.Join(path, configFileName)
	}
	if _, err := os.Stat(filePath); err != nil {
		return nil, fmt.Errorf("workspace config: %w", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("workspace config: reading %s: %w", filePath, err)
	}

	cfg := &Config{Root: filepath.Dir(filePath), Path: filePath}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("workspace config: parsing %s: %w", filePath, err)
	}

	if len(cfg.Members) == 0 {
		return nil, fmt.Errorf("workspace config %s declares no members", filePath)
	}
	return cfg.resolve()
}

// resolve normalizes member dirs to absolute paths, deduplicates by module
// path, marks missing members, and validates non-missing entries.
func (c *Config) resolve() (*Config, error) {
	seen := make(map[string]bool)
	var members []Member
	for _, m := range c.Members {
		if strings.TrimSpace(m.Module) == "" {
			return nil, fmt.Errorf("workspace config %s: member %q has no module path", c.Path, m.Dir)
		}
		if seen[m.Module] {
			continue
		}
		seen[m.Module] = true

		dir := m.Dir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(c.Root, dir)
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		dir, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("workspace config %s: member %q: %w", c.Path, m.Module, err)
		}
		m.Dir = dir

		if _, err := os.Stat(m.Dir); os.IsNotExist(err) {
			m.Missing = true
			members = append(members, m)
			continue
		}

		module, err := ReadModulePath(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("workspace config %s: member %q: %w", c.Path, m.Module, err)
		}
		if module != m.Module {
			return nil, fmt.Errorf("workspace config %s: member %q: go.mod declares module %q, expected %q", c.Path, m.Module, module, m.Module)
		}
		members = append(members, m)
	}
	c.Members = members
	return c, nil
}

// ReadModulePath returns the module path declared by go.mod in dir, or an
// error when the directory is not a Go module.
func ReadModulePath(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("not a Go module (no go.mod): %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "module "); ok {
			module := strings.TrimSpace(after)
			module = strings.Trim(module, `"`)
			if i := strings.Index(module, " "); i >= 0 {
				module = module[:i]
			}
			if module == "" {
				return "", fmt.Errorf("go.mod declares an empty module path")
			}
			return module, nil
		}
	}
	return "", fmt.Errorf("go.mod has no module declaration")
}

// Existing returns the members whose directories exist (non-missing).
func (c *Config) Existing() []Member {
	var out []Member
	for _, m := range c.Members {
		if !m.Missing {
			out = append(out, m)
		}
	}
	return out
}

// ContractConfig builds a contract analysis config from the workspace,
// carrying over contracts.suppress pairs (validated) and any custom runtime
// extractors declared under contracts.extractors, merged over the built-ins.
// Configuration errors (negative arg_index, unknown kind, malformed suppress
// separator, invalid normalize_rules) are returned rather than silently
// degrading; non-fatal issues (unknown normalize name) become warnings on the
// returned config.
func (c *Config) ContractConfig() (contract.Config, error) {
	cfg := contract.DefaultConfig()
	pairs, err := c.ContractSuppressionPairs()
	if err != nil {
		return cfg, err
	}
	if len(pairs) > 0 {
		cfg.SuppressPairs = make(map[string]bool, len(pairs))
		for _, p := range pairs {
			cfg.SuppressPairs[p[0]+" → "+p[1]] = true
		}
	}
	if len(c.Contracts.Extractors) > 0 {
		exts, warnings, err := runtimeExtractors(c.Contracts.Extractors)
		if err != nil {
			return cfg, err
		}
		cfg.Warnings = append(cfg.Warnings, warnings...)
		cfg.RuntimeExtractors = contract.MergeRuntimeExtractors(cfg.RuntimeExtractors, exts)
	}
	return cfg, nil
}

// runtimeExtractors converts codemap.yaml extractor declarations into
// contract.RuntimeExtractor values. Validation is loud: a negative arg_index,
// an empty kind, or an invalid normalize_rules regex is an error naming the
// extractor (or rule index); an unknown normalize name produces a warning while
// falling back to the raw (quote-strip) normalizer.
func runtimeExtractors(ecs []ExtractorConfig) ([]contract.RuntimeExtractor, []string, error) {
	var out []contract.RuntimeExtractor
	var warnings []string
	for _, ec := range ecs {
		name := ec.Name
		if name == "" {
			name = ec.Kind
		}
		if strings.TrimSpace(ec.Kind) == "" {
			return nil, warnings, fmt.Errorf("contracts.extractors: extractor %q has an empty kind", name)
		}
		if ec.ArgIndex < 0 {
			return nil, warnings, fmt.Errorf("contracts.extractors: extractor %q has a negative arg_index (%d); arg_index must be >= 0", name, ec.ArgIndex)
		}

		var normalize func(string) string
		var rules []contract.NormalizeRule
		if ec.Normalize != "" || len(ec.NormalizeRules) > 0 {
			base := contract.NamedNormalizers[ec.Normalize]
			if base == nil && ec.Normalize != "" && ec.Normalize != "raw" {
				warnings = append(warnings, fmt.Sprintf("extractor %q: unknown normalize %q, falling back to raw", name, ec.Normalize))
			}
			if base == nil {
				base = contract.NormalizeRaw
			}
			compiled, err := compileNormalizeRules(ec.NormalizeRules, name)
			if err != nil {
				return nil, warnings, err
			}
			rules = compiled
			normalize = contract.NormalizeWithRules(base, rules)
		}

		out = append(out, contract.RuntimeExtractor{
			Kind:            store.RuntimeContractKind(ec.Kind),
			Name:            name,
			ProducerMethods: ec.ProducerMethods,
			ConsumerMethods: ec.ConsumerMethods,
			ArgIndex:        ec.ArgIndex,
			ConfigPatterns:  ec.ConfigPatterns,
			ConstPrefix:     ec.ConstPrefix,
			DecodeSwitch:    ec.DecodeSwitch,
			ImportsMatch:    ec.ImportsMatch,
			Normalize:       normalize,
			NormalizeRules:  rules,
		})
	}
	return out, warnings, nil
}

// compileNormalizeRules eagerly compiles an extractor's normalize_rules,
// erroring on an invalid regex with the rule index and extractor named.
func compileNormalizeRules(rules []NormalizeRuleConfig, extractorName string) ([]contract.NormalizeRule, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]contract.NormalizeRule, 0, len(rules))
	for i, r := range rules {
		if strings.TrimSpace(r.Regex) == "" {
			return nil, fmt.Errorf("contracts.extractors: extractor %q normalize_rules[%d] has an empty regex", extractorName, i)
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("contracts.extractors: extractor %q normalize_rules[%d]: invalid regex %q: %w", extractorName, i, r.Regex, err)
		}
		out = append(out, contract.NormalizeRule{Re: re, Replace: r.Replace})
	}
	return out, nil
}
