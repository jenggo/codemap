package resolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
)

// TestNoPhantomRefsForScopeBoundObjects verifies that a callee or reference
// naming something with no package-scope symbol (a local func variable, a
// func-valued struct field, a local const, a goto label, or a method on an
// anonymous interface) emits no edge. Each would render as "<pkg>.<name>" and
// resolve to nothing in show/callers_of.
func TestNoPhantomRefsForScopeBoundObjects(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/phantom\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package phantom

type queue struct{ now func() int }

func (q *queue) stamp() int { return q.now() }

func worker(goFn func()) { goFn() }

func loop() int {
	const limit = 3
	total := 0
	for i := 0; i < limit; i++ {
		goto done
	}
done:
	return total
}

func assertFd(w any) uintptr {
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		return f.Fd()
	}
	return 0
}

func Use() int {
	q := &queue{now: func() int { return 1 }}
	worker(func() {})
	return q.stamp() + loop() + int(assertFd(nil))
}
`
	if err := os.WriteFile(filepath.Join(dir, "phantom.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	pr, err := parse.Run(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for _, e := range Run(pr).Edges {
		for _, bad := range []string{".now", ".goFn", ".Fd", ".done", ".limit"} {
			if strings.HasSuffix(e.Edge.ToRef, bad) && e.Edge.ToRef != "example.com/phantom.queue.stamp" {
				t.Errorf("edge to scope-bound %q should not be emitted: %s -> %s (%s)",
					bad, e.Edge.FromRef, e.Edge.ToRef, e.Edge.EdgeType)
			}
		}
	}
}
