package contract

import (
	"testing"

	"codemap/store"
)

func TestFindSharedTypeConstants(t *testing.T) {
	analysis := &Analysis{
		ConstantsByRepo: map[string]map[string]string{
			"repo-a": {
				"repo-a/types.MsgTypeAgentAsk": "agent_ask",
			},
			"repo-b": {
				"repo-b/types.MsgTypeAgentAsk": "agent_ask",
			},
			"repo-c": {
				"repo-c/types.MsgTypeAgentAsk": "agent_ans",
			},
		},
	}

	pairs := FindSharedTypeConstants(analysis)

	// repo-a and repo-b share the same value "agent_ask"
	// repo-c has a different value "agent_ans"
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair, got %d", len(pairs))
	}

	pair := pairs[0]
	if pair.A.Value != "agent_ask" || pair.B.Value != "agent_ask" {
		t.Errorf("expected both values to be 'agent_ask', got %q and %q", pair.A.Value, pair.B.Value)
	}
}

func TestConfidenceScore(t *testing.T) {
	tests := []struct {
		name     string
		signalA  bool
		signalB  bool
		expected float64
	}{
		{"both signals", true, true, 1.0},
		{"constant only", true, false, 0.6},
		{"shape only", false, true, 0.4},
		{"no signals", false, false, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := ConfidenceScore(tt.signalA, tt.signalB)
			if score != tt.expected {
				t.Errorf("expected %f, got %f", tt.expected, score)
			}
		})
	}
}

func TestIsSuggested(t *testing.T) {
	if !IsSuggested(0.3, 0.5) {
		t.Error("expected 0.3 to be suggested with threshold 0.5")
	}
	if IsSuggested(0.6, 0.5) {
		t.Error("expected 0.6 not to be suggested with threshold 0.5")
	}
}

func TestCompareStructShapes(t *testing.T) {
	a := StructInfo{
		Fields: []FieldInfo{
			{GoName: "ID", WireName: "id", GoType: "string"},
			{GoName: "Content", WireName: "content", GoType: "string"},
			{GoName: "Timeout", WireName: "timeout", GoType: "int"},
		},
	}
	b := StructInfo{
		Fields: []FieldInfo{
			{GoName: "ID", WireName: "id", GoType: "string"},
			{GoName: "Content", WireName: "content", GoType: "string"},
			{GoName: "Timeout", WireName: "timeout", GoType: "int"},
		},
	}

	comp := CompareStructShapes(a, b)
	if !comp.Identical {
		t.Error("expected identical structs")
	}
	if comp.Matches != 3 {
		t.Errorf("expected 3 matches, got %d", comp.Matches)
	}
}

func TestCompareStructShapesDivergent(t *testing.T) {
	a := StructInfo{
		Fields: []FieldInfo{
			{GoName: "ID", WireName: "id", GoType: "string"},
			{GoName: "Content", WireName: "content", GoType: "string"},
			{GoName: "Timeout", WireName: "timeout", GoType: "int"},
		},
	}
	b := StructInfo{
		Fields: []FieldInfo{
			{GoName: "ID", WireName: "id", GoType: "string"},
			{GoName: "Content", WireName: "content", GoType: "string"},
		},
	}

	comp := CompareStructShapes(a, b)
	if comp.Identical {
		t.Error("expected non-identical structs")
	}
	if len(comp.OnlyInA) != 1 {
		t.Errorf("expected 1 field only in A, got %d", len(comp.OnlyInA))
	}
}

func TestClassifyDriftSeverity(t *testing.T) {
	// Identical structs → compatible
	identical := ShapeComparison{Identical: true}
	if ClassifyDriftSeverity(identical) != store.DriftSeverityCompatible {
		t.Error("expected compatible for identical structs")
	}

	// Type change → breaking
	withDivergence := ShapeComparison{
		Divergences: []FieldDivergence{
			{WireName: "id", TypeA: "string", TypeB: "int", Status: "type_changed"},
		},
	}
	if ClassifyDriftSeverity(withDivergence) != store.DriftSeverityBreaking {
		t.Error("expected breaking for type change")
	}
}

func TestInferDirection(t *testing.T) {
	analysis := &Analysis{}

	// Producer → Consumer
	dir := InferDirection("pkg.Encode", "pkg.Decode", analysis)
	if dir != store.ContractDirectionProducer {
		t.Errorf("expected producer, got %s", dir)
	}

	// Consumer → Producer
	dir = InferDirection("pkg.Decode", "pkg.Encode", analysis)
	if dir != store.ContractDirectionConsumer {
		t.Errorf("expected consumer, got %s", dir)
	}

	// Unknown → shared
	dir = InferDirection("pkg.Handler", "pkg.Handler2", analysis)
	if dir != store.ContractDirectionShared {
		t.Errorf("expected shared, got %s", dir)
	}
}

func TestFilterSuppressed(t *testing.T) {
	pairs := []ConstantPair{
		{A: ConstantRef{Repo: "repo-a", Name: "a.X"}, B: ConstantRef{Repo: "repo-b", Name: "b.Y"}},
		{A: ConstantRef{Repo: "repo-a", Name: "a.Z"}, B: ConstantRef{Repo: "repo-b", Name: "b.W"}},
	}
	suppress := map[string]bool{
		"repo-a:a.X → repo-b:b.Y": true,
	}

	filtered := FilterSuppressed(pairs, suppress)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 pair, got %d", len(filtered))
	}
	if filtered[0].A.Name != "a.Z" {
		t.Errorf("expected remaining pair to be a.Z → b.W, got %s → %s", filtered[0].A.Name, filtered[0].B.Name)
	}
}
