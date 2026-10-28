package resolve

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"codemap/parse"
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

// TestStructEmbeddingCoversUnexportedTypes pins that an unexported wrapper gets
// its embeds edge. Skipping unexported types made the embed invisible, so a
// reader asking "what embeds X" got nothing and blast_radius reported zero
// embedders by construction.
func TestStructEmbeddingCoversUnexportedTypes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/embed\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package embed

type Target interface {
	Run()
}

// wrapper is unexported and embeds Target.
type wrapper struct {
	Target
}

// Outer is exported and embeds wrapper.
type Outer struct {
	wrapper
}
`
	if err := os.WriteFile(filepath.Join(dir, "embed.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := Run(pr)

	found := map[string]bool{}
	for _, e := range res.Edges {
		if e.Edge.EdgeType == "embeds" {
			found[e.Edge.FromRef+"->"+e.Edge.ToRef] = true
		}
	}
	for _, want := range []string{
		"example.com/embed.wrapper->example.com/embed.Target",
		"example.com/embed.Outer->example.com/embed.wrapper",
	} {
		if !found[want] {
			t.Errorf("missing embeds edge %s; got %v", want, found)
		}
	}
}
