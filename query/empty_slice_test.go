package query_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"codemap/query"
	"codemap/store"
)

// Empty-slice results must marshal to [] rather than null so MCP clients see
// a JSON array instead of a JSON null.
func TestEmptyResultsMarshalAsArray(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cycles, err := query.DetectCycles(s, "calls")
	if err != nil {
		t.Fatalf("DetectCycles: %v", err)
	}
	if data, err := json.Marshal(cycles); err != nil || string(data) != "[]" {
		t.Errorf("DetectCycles empty should marshal to [], got %s (err %v)", data, err)
	}

	// An unresolvable name is an error, not an empty list: "no implementers"
	// and "no such type" are different answers.
	if _, err := query.InterfaceImplementations(s, "missing.Interface"); err == nil {
		t.Error("InterfaceImplementations on an unknown type should error")
	}

	unused, err := query.UnusedSymbols(s)
	if err != nil {
		t.Fatalf("UnusedSymbols: %v", err)
	}
	if data, err := json.Marshal(unused); err != nil || string(data) != "[]" {
		t.Errorf("UnusedSymbols empty should marshal to [], got %s (err %v)", data, err)
	}
}
