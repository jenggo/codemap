package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"codemap/chunk"
	"codemap/extract"
	"codemap/parse"
	"codemap/resolve"
)

func newChunkTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestResponseChunksSaveManifestReadRoundTrip(t *testing.T) {
	st := newChunkTestStore(t)
	sections := []chunk.Section{
		{Title: "packages", Content: strings.Repeat("packages[2]:\n  - symbol line\n", 40)},
		{Title: "summary", Content: strings.Repeat("summary:\n  edges: 4\n", 40)},
	}

	id, err := st.SaveResponseChunks("overview", sections)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasPrefix(id, "resp-") {
		t.Fatalf("response id %q lacks the resp- prefix", id)
	}

	manifest, err := st.ResponseManifest(id)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.ResponseID != id || manifest.Tool != "overview" {
		t.Errorf("manifest = %+v, want id %s tool overview", manifest, id)
	}
	if manifest.Sections != 2 {
		t.Errorf("manifest sections = %d, want 2", manifest.Sections)
	}
	wantBytes := 0
	for _, sec := range sections {
		wantBytes += len(sec.Content)
	}
	if manifest.TotalBytes != wantBytes {
		t.Errorf("manifest total bytes = %d, want %d", manifest.TotalBytes, wantBytes)
	}

	all, err := st.ResponseSections(id, nil)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("read %d sections, want 2", len(all))
	}
	var b strings.Builder
	for _, sec := range all {
		b.WriteString(sec.Content)
	}
	for i, sec := range sections {
		if all[i].Title != sec.Title {
			t.Errorf("section %d title = %q, want %q", i, all[i].Title, sec.Title)
		}
		if all[i].ByteCount != len(sec.Content) {
			t.Errorf("section %d byte_count = %d, want %d", i, all[i].ByteCount, len(sec.Content))
		}
	}
	if b.String() != sections[0].Content+sections[1].Content {
		t.Errorf("sections read back are not lossless")
	}

	idx := 1
	one, err := st.ResponseSections(id, &idx)
	if err != nil {
		t.Fatalf("read one: %v", err)
	}
	if len(one) != 1 || one[0].Title != "summary" {
		t.Fatalf("indexed read = %+v, want the summary section", one)
	}
}

func TestResponseChunksKeywordSearchHitsDeepSection(t *testing.T) {
	st := newChunkTestStore(t)
	deep := strings.Repeat("filler text to push the target past the preview window\n", 300) +
		"grepForMeInDeepContent\n"
	sections := []chunk.Section{
		{Title: "head", Content: strings.Repeat("harmless header lines\n", 300)},
		{Title: "tail", Content: deep},
	}
	id, err := st.SaveResponseChunks("overview", sections)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	hits, err := st.SearchResponseSections(id, "grepForMeInDeepContent", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("search returned %d hits, want 1", len(hits))
	}
	if hits[0].Title != "tail" || hits[0].SectionIdx != 1 {
		t.Errorf("hit = %+v, want section 1 (tail)", hits[0])
	}

	// A short pattern falls back to the verbatim scan and still matches.
	shortHits, err := st.SearchResponseSections(id, "filler", 5)
	if err != nil {
		t.Fatalf("short search: %v", err)
	}
	if len(shortHits) != 1 || shortHits[0].Title != "tail" {
		t.Errorf("short-pattern hits = %+v, want the tail section", shortHits)
	}
}

func TestResponseChunksPrunedOnReindex(t *testing.T) {
	st := newChunkTestStore(t)
	sections := []chunk.Section{
		{Title: "packages", Content: strings.Repeat("data\n", 200)},
	}
	id, err := st.SaveResponseChunks("overview", sections)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	res := &resolve.Result{
		Packages: []parse.PackageInfo{{ImportPath: "prune.test", Name: "test"}},
		Symbols: []resolve.ResolvedSymbol{{
			Symbol: extract.Symbol{
				QualifiedName: "prune.test.Sym",
				Name:          "Sym",
				Kind:          "function",
				Pos:           extract.Position{File: "a.go", Line: 1},
			},
		}},
	}
	if err := st.Write(res, nil, nil); err != nil {
		t.Fatalf("reindex write: %v", err)
	}

	if _, err := st.ResponseManifest(id); !errors.Is(err, ErrUnknownResponse) {
		t.Fatalf("manifest after re-index err = %v, want ErrUnknownResponse", err)
	}
}

func TestResponseChunksLRUCapEviction(t *testing.T) {
	st := newChunkTestStore(t)
	origCap := responseChunkCapBytes
	responseChunkCapBytes = 600
	t.Cleanup(func() { responseChunkCapBytes = origCap })

	big := strings.Repeat("x", 400)
	firstID, err := st.SaveResponseChunks("overview", []chunk.Section{{Title: "a", Content: big}})
	if err != nil {
		t.Fatalf("save first: %v", err)
	}
	secondID, err := st.SaveResponseChunks("overview", []chunk.Section{{Title: "b", Content: big}})
	if err != nil {
		t.Fatalf("save second: %v", err)
	}

	if _, err := st.ResponseManifest(firstID); !errors.Is(err, ErrUnknownResponse) {
		t.Errorf("first response err = %v, want ErrUnknownResponse (evicted)", err)
	}
	if _, err := st.ResponseManifest(secondID); err != nil {
		t.Errorf("second response err = %v, want nil (kept)", err)
	}

	// A single response larger than the cap is kept whole, never evicted on
	// its own insert.
	soloID, err := st.SaveResponseChunks("overview", []chunk.Section{{Title: "c", Content: strings.Repeat("y", 2000)}})
	if err != nil {
		t.Fatalf("save oversized: %v", err)
	}
	if _, err := st.ResponseManifest(soloID); err != nil {
		t.Errorf("oversized response err = %v, want nil (kept whole)", err)
	}
}

func TestResponseChunksUnknownResponseID(t *testing.T) {
	st := newChunkTestStore(t)

	if _, err := st.ResponseManifest("resp-does-not-exist"); !errors.Is(err, ErrUnknownResponse) {
		t.Errorf("manifest err = %v, want ErrUnknownResponse", err)
	}
	if _, err := st.ResponseSections("resp-does-not-exist", nil); !errors.Is(err, ErrUnknownResponse) {
		t.Errorf("sections err = %v, want ErrUnknownResponse", err)
	}
	if _, err := st.SearchResponseSections("resp-does-not-exist", "query", 5); !errors.Is(err, ErrUnknownResponse) {
		t.Errorf("search err = %v, want ErrUnknownResponse", err)
	}
}

func TestResponseIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := range 50 {
		id, err := newChunkTestStore(t).SaveResponseChunks(fmt.Sprintf("tool%d", i), []chunk.Section{{Title: "s", Content: "c"}})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate response id %q", id)
		}
		seen[id] = true
	}
}
