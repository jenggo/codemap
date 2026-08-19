package resolve

import (
	"codemap/parse"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInterfaceMethodSymbols(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "iface-embed")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	methods := make(map[string]bool)
	for _, sym := range result.Symbols {
		if sym.Symbol.Kind == "method" && sym.Symbol.Receiver != "" {
			methods[sym.Symbol.QualifiedName] = true
		}
	}

	for _, want := range []string{
		"example.com/iface-embed.Logger.Log",
		"example.com/iface-embed.Closer.Close",
		"example.com/iface-embed.ReadWriter.Read",
		"example.com/iface-embed.ReadWriter.Write",
	} {
		if !methods[want] {
			t.Errorf("missing interface method symbol %q", want)
		}
	}
}

func TestInterfaceEmbedsEdge(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixtures", "iface-embed")

	pr, err := parse.Run(fixtureDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Run(pr)

	found := false
	for _, e := range result.Edges {
		if e.Edge.EdgeType == "embeds" && e.Edge.FromRef == "example.com/iface-embed.ReadWriter" {
			if e.Edge.ToRef == "example.com/iface-embed.Logger" || e.Edge.ToRef == "example.com/iface-embed.Closer" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("missing embeds edge from ReadWriter to Logger or Closer")
	}

	count := 0
	for _, e := range result.Edges {
		if e.Edge.EdgeType == "embeds" && e.Edge.FromRef == "example.com/iface-embed.ReadWriter" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 embeds edges from ReadWriter, got %d", count)
	}
}
