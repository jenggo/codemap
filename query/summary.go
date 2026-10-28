package query

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codemap/opt"
	"codemap/store"
	"codemap/workspace"
)

// SummaryResult orients an agent in a repository it has never seen: what the
// module is, how big it is, which packages matter, where execution starts, and
// what it depends on. Deliberately small — the full symbol inventory is
// Overview's job.
type SummaryResult struct {
	Module       string           `json:"module"`
	Description  string           `json:"description,omitempty"`
	IndexedVia   string           `json:"indexed_via"`
	GitHead      string           `json:"git_head"`
	ThirdParty   []string         `json:"third_party"`
	Stdlib       []string         `json:"stdlib"`
	Packages     []SummaryPackage `json:"packages"`
	EntryPoints  []EntryPoint     `json:"entry_points"`
	Hubs         []Hub            `json:"hubs"`
	TotalSymbols int              `json:"total_symbols"`
	TotalEdges   int              `json:"total_edges"`
}

type SummaryPackage struct {
	Path     string `json:"path"`
	Doc      string `json:"doc,omitempty"`
	SymCount int    `json:"sym_count"`
}

const (
	maxSummaryEntryPoints = 10
	maxSummaryHubs        = 5
	maxSummaryDeps        = 12
	maxSummaryDocLen      = 120
)

// Summary builds the tl;dr for the indexed repository. opts honour repo scoping
// and test inclusion; the module path is read from the repo's go.mod.
func Summary(s *store.Store, opts ...Option) (*SummaryResult, error) {
	options := &Options{}
	opt.Apply(options, opts)

	pkgs, err := s.ListPackages()
	if err != nil {
		return nil, err
	}
	health, err := s.Health()
	if err != nil {
		return nil, err
	}

	result := &SummaryResult{
		IndexedVia: health.IndexedVia,
		GitHead:    health.GitHead,
	}
	for _, p := range pkgs {
		result.TotalSymbols += p.SymCount
	}

	projectSet := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		if !options.IncludeTests && p.IsTest {
			continue
		}
		if repoMismatch(p.Repo, options.Repo) {
			continue
		}
		projectSet[p.Path] = true
		result.Packages = append(result.Packages, SummaryPackage{Path: p.Path, Doc: trimDoc(p.Doc), SymCount: p.SymCount})
	}
	sort.Slice(result.Packages, func(i, j int) bool {
		if result.Packages[i].SymCount != result.Packages[j].SymCount {
			return result.Packages[i].SymCount > result.Packages[j].SymCount
		}
		return result.Packages[i].Path < result.Packages[j].Path
	})
	result.Module = summaryModule(health.RepoPath)
	result.Description = RepoDescription(health.RepoPath)

	edges, err := s.CountEdges()
	if err != nil {
		return nil, err
	}
	result.TotalEdges = edges

	entryPoints, err := EntryPoints(s, []string{kindMain}, options.IncludeTests)
	if err != nil {
		return nil, err
	}
	result.EntryPoints = truncateEntryPoints(entryPoints, maxSummaryEntryPoints)

	result.ThirdParty, result.Stdlib = externalDeps(s, projectSet)
	result.Hubs = summaryHubs(s, projectSet)
	return result, nil
}

// repoDescription returns the repo's own one-paragraph self-description, taken
// from README.md. Package docs are not a substitute: the main package usually
// has none, so "what is this repo" was unanswerable from the index alone.
func RepoDescription(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	for _, name := range []string{"README.md", "readme.md", "README"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		if desc := firstParagraph(string(data)); desc != "" {
			return desc
		}
	}
	return ""
}

// firstParagraph skips the title, HTML, and badge lines that open a README and
// returns the first prose paragraph, capped to one summary line.
func firstParagraph(readme string) string {
	var para []string
	for line := range strings.SplitSeq(readme, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		if len(para) == 0 && isReadmeNoise(trimmed) {
			continue
		}
		para = append(para, trimmed)
	}
	return trimDoc(strings.Join(para, " "))
}

// isReadmeNoise reports lines that introduce a README rather than describe it:
// headings, HTML blocks, badge/shield markup, images, and bare relative links
// such as `krucil/README.md`.
func isReadmeNoise(line string) bool {
	switch {
	case strings.HasPrefix(line, "#"),
		strings.HasPrefix(line, "<!--"),
		strings.HasPrefix(line, "![["),
		strings.HasPrefix(line, "[!["),
		strings.HasPrefix(line, "<"),
		strings.HasPrefix(line, "---"):
		return true
	}
	return !strings.Contains(line, " ") && (strings.Contains(line, "/") || strings.HasSuffix(line, ".md"))
}

// summaryModule reads the module path from the repo's go.mod. The indexed
// package paths are not a reliable substitute: a module with a single package
// yields the package path, not the module.
func summaryModule(repoPath string) string {
	module, err := workspace.ReadModulePath(repoPath)
	if err != nil {
		return ""
	}
	return module
}

// trimDoc keeps a package doc short enough for a one-line summary row.
func trimDoc(doc string) string {
	if len(doc) <= maxSummaryDocLen {
		return doc
	}
	cut := doc[:maxSummaryDocLen]
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return cut + "..."
}

// externalDeps returns import targets outside the indexed project, split into
// third-party modules (a dot in the first path segment) and the standard
// library, each ranked by importer count. Test-file imports are skipped so the
// list reflects what the shipped code depends on.
func externalDeps(s *store.Store, projectSet map[string]bool) (thirdParty, stdlib []string) {
	imports, err := s.EdgesByType(edgeTypeImports)
	if err != nil {
		return nil, nil
	}
	counts := make(map[string]int)
	for _, e := range imports {
		if !projectSet[e.FromRef] || projectSet[e.ToRef] || testOnly(e) {
			continue
		}
		counts[e.ToRef]++
	}
	thirdParty, stdlib = splitDeps(counts)
	return thirdParty, stdlib
}

// testOnly reports whether every recorded site of an edge is a test file. The
// edge's PosFile is only one of its sites, so it cannot answer this alone.
func testOnly(e store.Edge) bool {
	if len(e.Sites) == 0 {
		return strings.HasSuffix(e.PosFile, "_test.go")
	}
	for _, site := range e.Sites {
		if !strings.HasSuffix(site.File, "_test.go") {
			return false
		}
	}
	return true
}

func splitDeps(counts map[string]int) (thirdParty, stdlib []string) {
	type dep struct {
		path  string
		count int
	}
	var mods, std []dep
	for path, count := range counts {
		first, _, _ := strings.Cut(path, "/")
		if strings.Contains(first, ".") {
			mods = append(mods, dep{path: path, count: count})
			continue
		}
		std = append(std, dep{path: path, count: count})
	}
	rank := func(deps []dep) []string {
		sort.Slice(deps, func(i, j int) bool {
			if deps[i].count != deps[j].count {
				return deps[i].count > deps[j].count
			}
			return deps[i].path < deps[j].path
		})
		limit := min(maxSummaryDeps, len(deps))
		out := make([]string, 0, limit)
		for _, d := range deps[:limit] {
			out = append(out, d.path)
		}
		return out
	}
	return rank(mods), rank(std)
}

func summaryHubs(s *store.Store, projectSet map[string]bool) []Hub {
	_, fanIn, fanOut, err := buildProjectGraph(s, projectSet)
	if err != nil {
		return nil
	}
	return topHubEntries(projectSet, fanIn, fanOut, maxSummaryHubs)
}

func truncateEntryPoints(entries []EntryPoint, limit int) []EntryPoint {
	if len(entries) <= limit {
		return entries
	}
	return entries[:limit]
}
