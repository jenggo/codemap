package workspace

import (
	"fmt"
	"os"
	"path/filepath"
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
	// ProducerMethods are method names whose entity string-literal argument is
	// a produced entity (write/publish side).
	ProducerMethods []string `yaml:"producer_methods"`
	// ConsumerMethods are method names whose entity string-literal argument is a
	// consumed entity (read/subscribe side).
	ConsumerMethods []string `yaml:"consumer_methods"`
	// ConfigPatterns are regexes matching config literals that name a produced
	// entity; capture group 1 is the literal.
	ConfigPatterns []string `yaml:"config_patterns"`
	// ArgIndex selects which string-literal argument holds the entity, 0-based.
	// Default 0 captures the first string literal; RabbitMQ-style APIs that put
	// a positional string (e.g. the exchange) before the routing key need 1.
	ArgIndex int `yaml:"arg_index"`
	// DecodeSwitch, when true, captures `case "..."` literals as consumers.
	DecodeSwitch bool `yaml:"decode_switch"`
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
func (c *Config) SuppressPairs() map[string]bool {
	pairs := c.ContractSuppressionPairs()
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		out[p[0]+" → "+p[1]] = true
	}
	return out
}

// ContractSuppressionPairs returns the parsed from/to pairs from the
// contracts.suppress list.
func (c *Config) ContractSuppressionPairs() [][2]string {
	if len(c.Contracts.Suppress) == 0 {
		return nil
	}
	var out [][2]string
	for _, entry := range c.Contracts.Suppress {
		from, to, ok := strings.Cut(entry, "→")
		if !ok {
			continue
		}
		from = strings.TrimSpace(from)
		to = strings.TrimSpace(to)
		if from == "" || to == "" {
			continue
		}
		out = append(out, [2]string{from, to})
	}
	return out
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
// carrying over contracts.suppress pairs and any custom runtime extractors
// declared under contracts.extractors.
func (c *Config) ContractConfig() contract.Config {
	cfg := contract.DefaultConfig()
	if pairs := c.SuppressPairs(); len(pairs) > 0 {
		cfg.SuppressPairs = pairs
	}
	if exts := runtimeExtractors(c.Contracts.Extractors); len(exts) > 0 {
		cfg.RuntimeExtractors = append(cfg.RuntimeExtractors, exts...)
	}
	return cfg
}

// runtimeExtractors converts codemap.yaml extractor declarations into
// contract.RuntimeExtractor values. Unknown normalizer names fall back to the
// raw (quote-strip) normalizer.
func runtimeExtractors(ecs []ExtractorConfig) []contract.RuntimeExtractor {
	if len(ecs) == 0 {
		return nil
	}
	out := make([]contract.RuntimeExtractor, 0, len(ecs))
	for _, ec := range ecs {
		normalize := contract.NamedNormalizers[ec.Normalize]
		if normalize == nil {
			normalize = contract.NormalizeRaw
		}
		out = append(out, contract.RuntimeExtractor{
			Kind:            store.RuntimeContractKind(ec.Kind),
			Name:            ec.Name,
			ProducerMethods: ec.ProducerMethods,
			ConsumerMethods: ec.ConsumerMethods,
			ArgIndex:        ec.ArgIndex,
			ConfigPatterns:  ec.ConfigPatterns,
			ConstPrefix:     ec.ConstPrefix,
			DecodeSwitch:    ec.DecodeSwitch,
			Normalize:       normalize,
		})
	}
	return out
}
