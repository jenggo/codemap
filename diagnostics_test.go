package codemap_test

import (
	"path/filepath"
	"strings"
	"testing"

	"codemap/parse"
	"codemap/query"
	"codemap/resolve"
	"codemap/store"
)

func TestDiagnosticsCaptureTypeErrors(t *testing.T) {
	absPath, err := filepath.Abs("testdata/fixtures/diagnostics")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}

	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	resolveResult := resolve.Run(parseResult)

	var typeErrs []resolve.Diagnostic
	for _, d := range resolveResult.Diagnostics {
		if d.Line > 0 {
			typeErrs = append(typeErrs, d)
		}
	}
	if len(typeErrs) == 0 {
		t.Fatalf("expected positioned diagnostics, got %v", resolveResult.Diagnostics)
	}
	foundSpeel := false
	for _, d := range typeErrs {
		if strings.Contains(d.Message, "speel") {
			foundSpeel = true
		}
		if !strings.HasSuffix(d.File, "broken.go") {
			t.Errorf("diagnostic attributed to wrong file: %s", d.File)
		}
	}
	if !foundSpeel {
		t.Errorf("expected a diagnostic naming the undefined symbol; got %v", typeErrs)
	}

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(resolveResult, nil, nil); err != nil {
		t.Fatalf("store write error: %v", err)
	}

	diags, err := query.Diagnostics(s, "")
	if err != nil {
		t.Fatalf("diagnostics query error: %v", err)
	}
	if len(diags) == 0 {
		t.Fatal("expected diagnostics persisted in the index")
	}
	if !strings.HasSuffix(diags[0].File, "broken.go") {
		t.Errorf("diagnostic file mismatch: %s", diags[0].File)
	}

	narrow, err := query.Diagnostics(s, "nonexistent_file.go")
	if err != nil {
		t.Fatalf("diagnostics query error: %v", err)
	}
	if len(narrow) != 0 {
		t.Errorf("expected empty result for unmatched file suffix, got %v", narrow)
	}
}

func TestDiagnosticsReplaceRepoOnReindex(t *testing.T) {
	absPath, err := filepath.Abs("testdata/fixtures/diagnostics")
	if err != nil {
		t.Fatalf("failed to get absolute path: %v", err)
	}
	parseResult, err := parse.Run(absPath)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	resolveResult := resolve.Run(parseResult)

	s, err := store.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store create error: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.WriteWorkspace(resolveResult, nil, nil, []store.RepoSpec{{ModulePath: "broken", Dir: absPath}}); err != nil {
		t.Fatalf("workspace write error: %v", err)
	}
	if err := s.ReplaceRepo(resolveResult, nil, nil, "broken"); err != nil {
		t.Fatalf("replace repo error: %v", err)
	}
	diags, err := query.Diagnostics(s, "")
	if err != nil {
		t.Fatalf("diagnostics query error: %v", err)
	}
	if len(diags) == 0 {
		t.Fatal("expected diagnostics to survive per-repo reindex path")
	}
	if diags[0].Repo != "broken" {
		t.Errorf("expected repo attribution 'broken', got %q", diags[0].Repo)
	}
}
