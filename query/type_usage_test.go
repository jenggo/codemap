package query_test

import (
	"os"
	"path/filepath"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

// A type used only through embedding must be reported. The signature scan
// cannot see an embedded field (it ends in ";" or a newline), so type_usage
// returned nothing for a base type while blast_radius counted its embedders.
func TestTypeUsageIncludesEmbedders(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/embedding")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	usages, err := query.TypeUsage(s, "Base")
	if err != nil {
		t.Fatalf("TypeUsage: %v", err)
	}

	got := make(map[string]bool, len(usages))
	for _, u := range usages {
		got[u.QualifiedName] = true
	}
	for _, want := range []string{
		"codemap/testdata/embedding.Extended",
		"codemap/testdata/embedding.MultiEmbed",
	} {
		if !got[want] {
			t.Errorf("%s embeds Base but is absent from type_usage (got %v)", want, got)
		}
	}

	blast, err := query.GetBlastRadius(s, "codemap/testdata/embedding.Base", 1)
	if err != nil {
		t.Fatalf("GetBlastRadius: %v", err)
	}
	if blast.Embedders != 2 {
		t.Fatalf("expected 2 embedders from blast_radius, got %d", blast.Embedders)
	}
}

// Embedders and signature users are merged without duplicates: a type that both
// embeds Base and mentions it in a signature is one usage, not two.
func TestTypeUsageDeduplicatesEmbedders(t *testing.T) {
	absPath, err := filepath.Abs("../testdata/embedding")
	if err != nil {
		t.Fatal(err)
	}
	s := setupFixtureStore(t, absPath)

	usages, err := query.TypeUsage(s, "Base")
	if err != nil {
		t.Fatalf("TypeUsage: %v", err)
	}

	seen := make(map[string]bool, len(usages))
	for _, u := range usages {
		if seen[u.QualifiedName] {
			t.Errorf("duplicate usage row for %s", u.QualifiedName)
		}
		seen[u.QualifiedName] = true
	}
}

// TestTypeUsageIncludesReferenceOnlyUses pins that a type named outside any
// signature is reported. A method receiver and a composite literal reach the
// type only through a reference edge, so signature matching alone answered
// "nowhere" for live types that show reported incoming references for.
func TestTypeUsageIncludesReferenceOnlyUses(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/refuse\n\ngo 1.21\n")
	write("p/p.go", `package p

// Buffer is only ever a receiver and a composite-literal type; no signature
// names it.
type Buffer struct {
	n int
}

func (b *Buffer) Add() { b.n++ }

func (b *Buffer) Len() int { return b.n }

func New() *Buffer {
	return &Buffer{n: 0}
}
`)
	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	usages, err := query.TypeUsage(s, "Buffer")
	if err != nil {
		t.Fatalf("TypeUsage: %v", err)
	}
	got := map[string]bool{}
	for _, u := range usages {
		got[u.QualifiedName] = true
	}
	// New names Buffer in its return type; the two methods reach it only as
	// receivers.
	for _, want := range []string{
		"example.com/refuse/p.New",
		"example.com/refuse/p.Buffer.Add",
		"example.com/refuse/p.Buffer.Len",
	} {
		if !got[want] {
			t.Errorf("%s uses Buffer but is absent from type_usage (got %v)", want, got)
		}
	}
}

// TestShowReportsSharedQualifiedName pins that show says how many declarations
// share the queried name. A package may declare several funcs with one name
// (init, in one file per migration), and show used to present one of them with
// every declaration's edges merged, which reads as a single function calling
// all of them.
func TestShowReportsSharedQualifiedName(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/dup\n\ngo 1.21\n")
	write("p/a.go", "package p\n\nfunc init() { helperA() }\n\nfunc helperA() {}\n")
	write("p/b.go", "package p\n\nfunc init() { helperB() }\n\nfunc helperB() {}\n")
	write("p/c.go", "package p\n\nfunc Once() {}\n")

	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(resolve.Run(pr), nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	initShow, err := query.Show(s, "example.com/dup/p.init")
	if err != nil {
		t.Fatalf("Show init: %v", err)
	}
	if initShow.Declarations != 2 {
		t.Errorf("init declarations = %d, want 2", initShow.Declarations)
	}

	onceShow, err := query.Show(s, "example.com/dup/p.Once")
	if err != nil {
		t.Fatalf("Show Once: %v", err)
	}
	if onceShow.Declarations != 1 {
		t.Errorf("Once declarations = %d, want 1", onceShow.Declarations)
	}
}
