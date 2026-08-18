package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"codemap/extract"
	"codemap/parse"
	"codemap/resolve"
)

// churnFixture builds a resolved result whose symbols live one per file, so
// churn can target each symbol individually.
func churnFixture(t *testing.T, fileCount int) (*resolve.Result, map[string]int) {
	t.Helper()
	res := &resolve.Result{
		Packages: []parse.PackageInfo{{ImportPath: "churn.test", Name: "test"}},
		Symbols:  make([]resolve.ResolvedSymbol, 0, fileCount),
	}
	churn := make(map[string]int, fileCount)
	for i := range fileCount {
		file := fmt.Sprintf("file%d.go", i)
		res.Symbols = append(res.Symbols, resolve.ResolvedSymbol{
			Symbol: extract.Symbol{
				QualifiedName: fmt.Sprintf("churn.test.Sym%d", i),
				Name:          fmt.Sprintf("Sym%d", i),
				Kind:          "function",
				Pos:           extract.Position{File: file, Line: 1},
			},
		})
		churn[file] = i + 1
	}
	return res, churn
}

// TestChurnValuesIdenticalPrePost verifies the batched implementation produces
// exactly the churn values the previous per-file UPDATE implementation produced.
func TestChurnValuesIdenticalPrePost(t *testing.T) {
	res, churn := churnFixture(t, 40)

	// New implementation: batched single UPDATE inside Write.
	anew, err := Create(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = anew.Close() }()
	if err := anew.Write(res, nil, churn); err != nil {
		t.Fatalf("batched write: %v", err)
	}

	// Pre-change implementation: one UPDATE per file after a plain write.
	aold, err := Create(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = aold.Close() }()
	if err := aold.Write(res, nil, nil); err != nil {
		t.Fatalf("plain write: %v", err)
	}
	ctx := context.Background()
	for file, count := range churn {
		if _, err := aold.db.ExecContext(ctx, `UPDATE symbols SET churn_count = ? WHERE pos_file = ?`, count, file); err != nil {
			t.Fatal(err)
		}
	}

	rowsNew := churnReport(t, anew)
	rowsOld := churnReport(t, aold)
	if len(rowsNew) != len(rowsOld) {
		t.Fatalf("row count mismatch: new %d, old %d", len(rowsNew), len(rowsOld))
	}
	for i := range rowsNew {
		if rowsNew[i] != rowsOld[i] {
			t.Fatalf("churn row %d mismatch: new %q, old %q", i, rowsNew[i], rowsOld[i])
		}
	}
}

func churnReport(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT qualified_name || ':' || CAST(churn_count AS TEXT) FROM symbols ORDER BY qualified_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestChurnUpdateStatementCountBounded verifies the number of UPDATE statements
// is independent of the changed-file count: 400 files collapse to a single
// batched UPDATE.
func TestChurnUpdateStatementCountBounded(t *testing.T) {
	res, churn := churnFixture(t, 400)

	s, c := newCountingStore(t)
	defer func() { _ = s.Close() }()

	if err := s.Write(res, nil, churn); err != nil {
		t.Fatalf("write with churn: %v", err)
	}
	if got := c.updates.Load(); got != 1 {
		t.Fatalf("expected exactly 1 UPDATE for 400 churn files, got %d", got)
	}

	// Spot-check that staged values landed on the right files.
	for _, probe := range []struct {
		file  string
		count int
	}{
		{"file0.go", 1},
		{"file7.go", 8},
		{"file399.go", 400},
	} {
		var got int
		if err := s.db.QueryRowContext(context.Background(),
			`SELECT churn_count FROM symbols WHERE pos_file = ?`, probe.file).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != probe.count {
			t.Errorf("churn for %s = %d, want %d", probe.file, got, probe.count)
		}
	}
}
