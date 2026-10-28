package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
)

// runtimeFixture builds a single-module tree that declares one WS message type
// and switches on it, which is enough for a runtime contract.
func runtimeFixture(t *testing.T) (string, *parse.Result, *resolve.Result) {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, "go.mod", "module example.com/clirt\n\ngo 1.26\n")
	writeFixture(t, dir, "rt.go", `package rt

const MsgTypePing = "ping"

func handle(t string) string {
	switch t {
	case MsgTypePing:
		return "pong"
	}
	return ""
}
`)

	parseResult, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return dir, parseResult, resolve.Run(parseResult)
}

// TestWriteIndexStorePopulatesRuntimeContracts pins the CLI single-repo path:
// `codemap index` records runtime contracts and their analysis time without a
// workspace, so runtime_contracts is not empty after one command.
func TestWriteIndexStorePopulatesRuntimeContracts(t *testing.T) {
	dir, parseResult, resolveResult := runtimeFixture(t)
	s := newIndexStore(t)

	writeIndexStore(s, dir, parseResult, resolveResult)

	contracts, err := s.QueryRuntimeContracts(store.RuntimeContractWSType, "")
	if err != nil {
		t.Fatalf("QueryRuntimeContracts: %v", err)
	}
	if len(contracts) == 0 {
		t.Fatal("expected a ws_type runtime contract from the switch on MsgTypePing")
	}
	if _, ok, err := s.ContractAnalysisTime(); err != nil {
		t.Fatalf("ContractAnalysisTime: %v", err)
	} else if !ok {
		t.Fatal("contract_indexed_at was not recorded")
	}
}

// TestWriteIndexStoreKeepsIndexWhenContractAnalysisFails pins the non-fatal
// handling: the index is already written, so a failed analysis must leave it
// usable and must not record a completed analysis.
func TestWriteIndexStoreKeepsIndexWhenContractAnalysisFails(t *testing.T) {
	dir, parseResult, resolveResult := runtimeFixture(t)
	s := newIndexStore(t)
	s.SetTestHook(func(stage, _ string) error {
		if stage == "contract.meta" {
			return errors.New("injected analysis failure")
		}
		return nil
	})
	t.Cleanup(func() { s.SetTestHook(nil) })

	capturedStderr := captureStderr(t, func() {
		writeIndexStore(s, dir, parseResult, resolveResult)
	})

	if _, syms, _, err := s.Counts(); err != nil {
		t.Fatalf("index unusable after a failed analysis: %v", err)
	} else if syms == 0 {
		t.Fatal("index is empty after a failed analysis")
	}
	if _, err := s.SymbolByName("example.com/clirt.handle"); err != nil {
		t.Errorf("index does not answer after a failed analysis: %v", err)
	}
	if _, ok, err := s.ContractAnalysisTime(); err != nil {
		t.Fatalf("ContractAnalysisTime: %v", err)
	} else if ok {
		t.Error("analysis time recorded despite the failure")
	}
	if !strings.Contains(capturedStderr, "Contract analysis error: injected analysis failure") {
		t.Errorf("failure was not reported on stderr, got %q", capturedStderr)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	return string(data)
}

func newIndexStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Create(filepath.Join(t.TempDir(), "cli.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
