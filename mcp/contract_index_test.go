package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codemap/contract"
	"codemap/store"
)

// TestAutoIndexPopulatesRuntimeContracts pins that the MCP stale-rebuild path
// runs contract analysis, so a single-repo index answers runtime_contracts
// without a workspace, and that an unchanged index is not reanalysed.
func TestAutoIndexPopulatesRuntimeContracts(t *testing.T) {
	dir := t.TempDir()
	writeRuntimeFixture(t, dir)

	s := &Server{dbPath: filepath.Join(t.TempDir(), "rt.db")}
	if err := s.autoIndex(dir); err != nil {
		t.Fatalf("autoIndex: %v", err)
	}

	contracts, err := s.store.QueryRuntimeContracts(store.RuntimeContractWSType, "")
	if err != nil {
		t.Fatalf("QueryRuntimeContracts: %v", err)
	}
	if len(contracts) == 0 {
		t.Fatal("expected a ws_type runtime contract from the switch on MsgTypePing")
	}

	first, ok, err := s.store.ContractAnalysisTime()
	if err != nil {
		t.Fatalf("ContractAnalysisTime: %v", err)
	}
	if !ok {
		t.Fatal("contract_indexed_at was not recorded")
	}

	// The guard is reachable once the analysis is current for the index: the
	// staleness check a query performs then costs one meta read and leaves the
	// recorded time untouched.
	if _, err := contract.ReanalyzeStaleContracts(s.store, contract.DefaultConfig()); err != nil {
		t.Fatalf("ReanalyzeStaleContracts: %v", err)
	}
	second, ok, err := s.store.ContractAnalysisTime()
	if err != nil {
		t.Fatalf("ContractAnalysisTime: %v", err)
	}
	if !ok {
		t.Fatal("contract_indexed_at disappeared after a current reanalysis check")
	}
	if !second.Equal(first) {
		t.Errorf("analysis reran while current: %v -> %v", first, second)
	}
}

// TestAutoIndexKeepsIndexWhenContractAnalysisFails pins the non-fatal handling:
// the index is already on disk, so a failed analysis is a warning rather than a
// failed rebuild, and the store still answers.
func TestAutoIndexKeepsIndexWhenContractAnalysisFails(t *testing.T) {
	dir := t.TempDir()
	writeRuntimeFixture(t, dir)

	s := &Server{dbPath: filepath.Join(t.TempDir(), "rt.db")}
	s.contractFn = func(*store.Store, contract.Config) ([]string, error) {
		return nil, errors.New("injected analysis failure")
	}
	if err := s.autoIndex(dir); err != nil {
		t.Fatalf("autoIndex must not fail the index: %v", err)
	}

	if _, syms, _, err := s.store.Counts(); err != nil {
		t.Fatalf("index unusable after a failed analysis: %v", err)
	} else if syms == 0 {
		t.Fatal("index is empty after a failed analysis")
	}
	if _, err := s.store.SymbolByName("example.com/rt.handle"); err != nil {
		t.Errorf("index does not answer after a failed analysis: %v", err)
	}
	var warned bool
	for _, w := range s.warnings {
		if strings.Contains(w, "contract analysis failed") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("failure was not appended to warnings: %v", s.warnings)
	}
}

func writeRuntimeFixture(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module example.com/rt\n\ngo 1.26\n",
		"rt.go": `package rt

const MsgTypePing = "ping"

func handle(t string) string {
	switch t {
	case MsgTypePing:
		return "pong"
	}
	return ""
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}
