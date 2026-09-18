package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// markIndexed simulates what a real index write records: the generated-code
// provenance version plus the index timestamp. Staleness tests that build a
// database by hand use it so the provenance trigger does not mask the signal
// under test.
func markIndexed(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	if err := s.SetIndexedAt(at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		generatedProvenanceKey, generatedProvenanceVersion); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateGeneratedProvenanceUpgrade verifies that opening an index built
// before generated-code detection adds the is_generated columns in place,
// preserves every existing row, and leaves the provenance marker absent so the
// one-time reindex runs.
func TestMigrateGeneratedProvenanceUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := writeV2DB(t, dir, nil)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, table := range []string{"packages", "symbols"} {
		has, err := columnExists(s.db, table, generatedColumn)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", table, err)
		}
		if !has {
			t.Fatalf("%s.%s missing after migration", table, generatedColumn)
		}
	}

	// Pre-existing rows survive and read as non-generated. The legacy fixture
	// omits receiver/signature, so the row is inspected directly rather than
	// through scanSymbol (which requires non-NULL columns).
	var legacyGenerated bool
	if err := s.db.QueryRowContext(context.Background(), "SELECT is_generated FROM symbols WHERE qualified_name = 'legacy.example/repo.A'").Scan(&legacyGenerated); err != nil {
		t.Fatalf("legacy symbol lost after migration: %v", err)
	}
	if legacyGenerated {
		t.Error("legacy symbol must default to non-generated")
	}

	pkgs, err := s.ListPackages()
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(pkgs) != 1 || pkgs[0].SymCount != 2 {
		t.Fatalf("legacy package rows lost after migration: %+v", pkgs)
	}
	if pkgs[0].IsGenerated {
		t.Error("legacy package must default to non-generated")
	}

	// No provenance marker yet: the staleness checks must treat this index as
	// stale exactly once so real values replace the column defaults.
	if _, ok := readMetaDB(s.db, generatedProvenanceKey); ok {
		t.Fatal("migration must not record generated provenance")
	}
}

// TestGeneratedFlagRoundTrip verifies that generated provenance survives the
// index write and reads back per symbol and per package.
func TestGeneratedFlagRoundTrip(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"mixed/hand.go":          "package mixed\n\nfunc HandMade() {}\n",
		"mixed/thing.pb.go":      "package mixed\n\nfunc GeneratedThing() {}\n",
		"allgen/zz_generated.go": "package allgen\n\nfunc AutoGen() {}\n",
	})
	res, files := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, tc := range []struct {
		name      string
		generated bool
	}{
		{"atomic.example/mixed.HandMade", false},
		{"atomic.example/mixed.GeneratedThing", true},
		{"atomic.example/allgen.AutoGen", true},
	} {
		sym, err := s.SymbolByName(tc.name)
		if err != nil {
			t.Fatalf("SymbolByName(%s): %v", tc.name, err)
		}
		if sym.IsGenerated != tc.generated {
			t.Errorf("%s: IsGenerated = %t, want %t", tc.name, sym.IsGenerated, tc.generated)
		}
	}

	pkgs, err := s.ListPackages()
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]Package, len(pkgs))
	for _, p := range pkgs {
		byPath[p.Path] = p
	}
	if byPath["atomic.example/mixed"].IsGenerated {
		t.Error("mixed package must not be IsGenerated")
	}
	if !byPath["atomic.example/allgen"].IsGenerated {
		t.Error("allgen package must be IsGenerated")
	}
}

// TestWriteRecordsGeneratedProvenance verifies that a successful index write
// records the provenance version the staleness checks look for.
func TestWriteRecordsGeneratedProvenance(t *testing.T) {
	dir := writeModule(t, map[string]string{"a.go": "package prov\nfunc Kept() {}\n"})
	res, files := parseModule(t, dir)

	s, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, ok := readMetaDB(s.db, generatedProvenanceKey)
	if !ok {
		t.Fatal("index write must record the generated provenance version")
	}
	if got != generatedProvenanceVersion {
		t.Fatalf("provenance version = %q, want %q", got, generatedProvenanceVersion)
	}
}

// TestStaleUntilGeneratedProvenanceRecorded verifies that an index without the
// provenance marker is reported stale for the reason callers surface, and that
// writing an index clears it.
func TestStaleUntilGeneratedProvenanceRecorded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	stale, reason, err := StaleReason(dbPath, repo)
	if err != nil {
		t.Fatalf("StaleReason: %v", err)
	}
	if !stale || !strings.Contains(reason, "generated-code provenance") {
		t.Fatalf("expected provenance staleness, got stale=%t reason=%q", stale, reason)
	}

	dir := writeModule(t, map[string]string{"a.go": "package prov\nfunc Kept() {}\n"})
	res, files := parseModule(t, dir)
	if err := s.Write(res, files, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.SetIndexedAt(time.Now()); err != nil {
		t.Fatal(err)
	}

	stale, reason, err = StaleReason(dbPath, repo)
	if err != nil {
		t.Fatalf("StaleReason after write: %v", err)
	}
	if stale {
		t.Fatalf("index fresh after write, got stale=%t reason=%q", stale, reason)
	}
}
