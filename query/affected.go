package query

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"codemap/store"
	"codemap/vcs"
)

const (
	// affectedDefaultDepth matches blast_radius's default, so "how far out do
	// I look" has one answer across both tools.
	affectedDefaultDepth = 3
	// affectedViaCap bounds the evidence list per test file: five names are
	// enough to recognize the connection, and a changed hub symbol would
	// otherwise make one entry unreadable.
	affectedViaCap = 5
	// affectedLowerBoundNote states what static edges cannot see.
	affectedLowerBoundNote = "reachability comes from static call and reference edges, so a test reached only through dynamic dispatch, a callback invoked by name, or reflection may be missing"
	// reasonCallers marks a test found through the call graph.
	reasonCallers = "callers"
	// reasonConvention marks a test found by its _test.go sibling name.
	reasonConvention = "convention"
)

// AffectedTest is one test file impacted by a change.
type AffectedTest struct {
	File         string   `json:"file"`
	Reason       string   `json:"reason"`
	Via          []string `json:"via"`
	ViaTruncated bool     `json:"via_truncated"`
}

// AffectedTestsResult maps changed files to the test files that exercise them.
type AffectedTestsResult struct {
	Note  string         `json:"note"`
	Files []AffectedTest `json:"files"`
}

// AffectedTests reports the test files that reach symbols defined in the
// changed files, either through incoming call/reference edges within depth
// levels or through the _test.go naming convention. With no explicit files,
// the changed set is the working tree delta against ref.
func AffectedTests(s *store.Store, repoDir, ref string, files []string, depth int) (*AffectedTestsResult, error) {
	if depth <= 0 {
		depth = affectedDefaultDepth
	}
	repoDir, err := resolveRepoDir(repoDir)
	if err != nil {
		return nil, err
	}
	changed, err := affectedChangedFiles(repoDir, ref, files)
	if err != nil {
		return nil, err
	}

	namesByFile := make(map[string][]string, len(changed))
	var roots []string
	for _, path := range changed {
		names, err := symbolNamesInFile(s, path)
		if err != nil {
			return nil, err
		}
		namesByFile[path] = names
		roots = append(roots, names...)
	}

	collected := make(map[string]*AffectedTest)
	if err := collectCallerTests(s, roots, depth, collected); err != nil {
		return nil, err
	}
	if err := collectConventionTests(s, changed, namesByFile, collected); err != nil {
		return nil, err
	}

	result := &AffectedTestsResult{Files: []AffectedTest{}, Note: affectedLowerBoundNote}
	for _, entry := range collected {
		entry.Via, entry.ViaTruncated = cappedVia(entry.Via)
		result.Files = append(result.Files, *entry)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].File < result.Files[j].File })
	if len(result.Files) == 0 {
		result.Note = "no affected tests found; " + affectedLowerBoundNote
	}
	return result, nil
}

// affectedChangedFiles resolves the changed set: the explicit list when given,
// otherwise the working tree delta against ref. Deleted files are skipped —
// they define no symbol a test could reach.
func affectedChangedFiles(repoDir, ref string, files []string) ([]string, error) {
	if len(files) > 0 {
		out := make([]string, 0, len(files))
		for _, f := range files {
			if !filepath.IsAbs(f) {
				f = filepath.Join(repoDir, f)
			}
			out = append(out, f)
		}
		return out, nil
	}

	if ref == "" {
		ref = vcs.DefaultBranch(repoDir)
	}
	statuses, err := vcs.GitFileStatuses(repoDir, ref)
	if err != nil {
		return nil, fmt.Errorf("affected_tests: %w", err)
	}
	untracked, err := vcs.GitUntrackedFiles(repoDir)
	if err != nil {
		return nil, fmt.Errorf("affected_tests: %w", err)
	}

	out := make([]string, 0, len(statuses)+len(untracked))
	for _, st := range statuses {
		if st.Status == "D" {
			continue
		}
		out = append(out, filepath.Join(repoDir, st.Path))
	}
	for _, path := range untracked {
		out = append(out, filepath.Join(repoDir, path))
	}
	return out, nil
}

// symbolNamesInFile returns the qualified names declared in one indexed file.
func symbolNamesInFile(s *store.Store, path string) ([]string, error) {
	syms, err := s.SymbolsInFile(path, true)
	if err != nil {
		return nil, fmt.Errorf("affected_tests: %s: %w", path, err)
	}
	names := make([]string, len(syms))
	for i, sym := range syms {
		names[i] = sym.QualifiedName
	}
	return names, nil
}

// collectCallerTests runs the incoming BFS one query per depth level, so a
// change costs at most depth queries regardless of how many symbols it
// defines. Each visited node carries the changed roots that reached it, which
// is what a test file's evidence list reports.
func collectCallerTests(s *store.Store, roots []string, depth int, collected map[string]*AffectedTest) error {
	if len(roots) == 0 {
		return nil
	}
	origins := make(map[string][]string, len(roots))
	frontier := make([]string, 0, len(roots))
	for _, root := range roots {
		if _, ok := origins[root]; ok {
			continue
		}
		origins[root] = []string{root}
		frontier = append(frontier, root)
	}

	for level := 0; level < depth && len(frontier) > 0; level++ {
		edges, err := s.EdgesForNodes(frontier, []string{"calls", "references"}, false)
		if err != nil {
			return fmt.Errorf("affected_tests: edges: %w", err)
		}
		var next []string
		for _, e := range edges {
			if strings.HasSuffix(e.PosFile, "_test.go") {
				addCallerTest(collected, e.PosFile, origins[e.ToRef])
			}
			if _, ok := origins[e.FromRef]; ok {
				continue
			}
			origins[e.FromRef] = append([]string(nil), origins[e.ToRef]...)
			next = append(next, e.FromRef)
		}
		frontier = next
	}
	return nil
}

// addCallerTest records a test file found through the call graph.
func addCallerTest(collected map[string]*AffectedTest, file string, via []string) {
	entry := collected[file]
	if entry == nil {
		entry = &AffectedTest{File: file, Reason: reasonCallers}
		collected[file] = entry
	}
	entry.Via = append(entry.Via, via...)
}

// collectConventionTests adds the _test.go sibling of each changed file that
// exists in the index. A file already found through the call graph keeps that
// reason: a real edge is stronger evidence than a filename.
func collectConventionTests(s *store.Store, changed []string, namesByFile map[string][]string, collected map[string]*AffectedTest) error {
	for _, path := range changed {
		if strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
			continue
		}
		sibling := strings.TrimSuffix(path, ".go") + "_test.go"
		exists, err := indexedFile(s, sibling)
		if err != nil {
			return fmt.Errorf("affected_tests: %s: %w", sibling, err)
		}
		if !exists {
			continue
		}
		if _, ok := collected[sibling]; ok {
			continue
		}
		collected[sibling] = &AffectedTest{File: sibling, Reason: reasonConvention, Via: namesByFile[path]}
	}
	return nil
}

// indexedFile reports whether the path is present in the index.
func indexedFile(s *store.Store, path string) (bool, error) {
	_, err := s.FileContent(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// cappedVia deduplicates the evidence names, sorts them for determinism, and
// caps the list at affectedViaCap, reporting whether it was truncated.
func cappedVia(names []string) ([]string, bool) {
	seen := make(map[string]bool, len(names))
	unique := make([]string, 0, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, name)
	}
	sort.Strings(unique)
	if len(unique) > affectedViaCap {
		return unique[:affectedViaCap], true
	}
	return unique, false
}
