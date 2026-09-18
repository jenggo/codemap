package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/render"
	"codemap/resolve"
	"codemap/store"
)

func TestCmdPatternReportsMatch(t *testing.T) {
	dbPath := patternFixtureDB(t)
	out := captureStdout(t, func() {
		if err := cmdPattern("defer $CALL", dbPath, parsedFlags{filePattern: "main.go"}, []render.Option{render.WithFormat(render.FormatJSON)}); err != nil {
			t.Errorf("cmdPattern: %v", err)
		}
	})
	if !strings.Contains(out, "example.com/pattern.Handle") {
		t.Errorf("output must name the enclosing symbol, got:\n%s", out)
	}
}

func TestCmdPatternInvalidPatternReturnsError(t *testing.T) {
	dbPath := patternFixtureDB(t)
	if err := cmdPattern("if err != nil {", dbPath, parsedFlags{}, []render.Option{render.WithFormat(render.FormatJSON)}); err == nil {
		t.Fatal("an unparseable pattern must return an error")
	}
}

func patternFixtureDB(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "pattern"))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := parse.Run(abs)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Create(dbPath)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := s.Write(resolve.Run(pr), parse.FileContents(pr), nil); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return dbPath
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()

	fn()

	_ = w.Close()
	os.Stdout = orig
	return <-done
}
