package contract

import (
	"testing"

	"codemap/store"
)

func TestDetectDrift(t *testing.T) {
	a := StructInfo{
		QualifiedName: "repo-a/types.AgentAsk",
		Fields: []FieldInfo{
			{GoName: "ID", WireName: "id", GoType: "string"},
			{GoName: "Content", WireName: "content", GoType: "string"},
			{GoName: "Timeout", WireName: "timeout", GoType: "int"},
		},
		Repo: "repo-a",
	}
	b := StructInfo{
		QualifiedName: "repo-b/types.AgentAsk",
		Fields: []FieldInfo{
			{GoName: "ID", WireName: "id", GoType: "string"},
			{GoName: "Content", WireName: "content", GoType: "string"},
		},
		Repo: "repo-b",
	}

	report := DetectDrift("repo-a/types.AgentAsk", "repo-b/types.AgentAsk", a, b, "2024-01-01T00:00:00Z")
	if report == nil {
		t.Fatal("expected drift report")
	}
	if report.Severity != store.DriftSeverityCompatible {
		t.Errorf("expected compatible severity, got %s", report.Severity)
	}
	if len(report.Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(report.Fields))
	}
	if report.Fields[0].Status != "removed" {
		t.Errorf("expected removed status, got %s", report.Fields[0].Status)
	}
}

func TestDetectConstantDriftValueChanged(t *testing.T) {
	a := ConstantRef{Repo: "repo-a", Name: "MsgType", Value: "agent_ask"}
	b := ConstantRef{Repo: "repo-b", Name: "MsgType", Value: "agent_ans"}

	report := DetectConstantDrift(a, b, "2024-01-01T00:00:00Z")
	if report == nil {
		t.Fatal("expected drift report")
	}
	if report.Severity != store.DriftSeverityBreaking {
		t.Errorf("expected breaking severity, got %s", report.Severity)
	}
}

func TestDetectConstantDriftDifferentNames(t *testing.T) {
	a := ConstantRef{Repo: "repo-a", Name: "MsgTypeA", Value: "agent_ask"}
	b := ConstantRef{Repo: "repo-b", Name: "MsgTypeB", Value: "agent_ask"}

	report := DetectConstantDrift(a, b, "2024-01-01T00:00:00Z")
	if report == nil {
		t.Fatal("expected drift report")
	}
	if report.Severity != store.DriftSeverityUnknown {
		t.Errorf("expected unknown severity, got %s", report.Severity)
	}
}

func TestDetectConstantDriftSame(t *testing.T) {
	a := ConstantRef{Repo: "repo-a", Name: "MsgType", Value: "agent_ask"}
	b := ConstantRef{Repo: "repo-b", Name: "MsgType", Value: "agent_ask"}

	report := DetectConstantDrift(a, b, "2024-01-01T00:00:00Z")
	if report != nil {
		t.Error("expected no drift for identical constants")
	}
}
