package store

import (
	"encoding/json"
	"testing"
)

// An empty contract query must marshal to [] rather than null: MCP clients
// iterate the result, and a JSON null breaks them silently.
func TestEmptyContractQueriesMarshalAsArray(t *testing.T) {
	s, _ := newCountingStore(t)
	defer func() { _ = s.Close() }()

	contracts, err := s.QueryContracts(ContractFilter{})
	if err != nil {
		t.Fatalf("QueryContracts: %v", err)
	}
	assertJSONArray(t, "contracts", contracts)

	drift, err := s.QueryDrift("", "")
	if err != nil {
		t.Fatalf("QueryDrift: %v", err)
	}
	assertJSONArray(t, "drift", drift)

	runtimeContracts, err := s.QueryRuntimeContracts("", "")
	if err != nil {
		t.Fatalf("QueryRuntimeContracts: %v", err)
	}
	assertJSONArray(t, "runtime contracts", runtimeContracts)
}

func assertJSONArray(t *testing.T, label string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s marshal: %v", label, err)
	}
	if string(data) != "[]" {
		t.Errorf("%s empty should marshal to [], got %s", label, data)
	}
}
