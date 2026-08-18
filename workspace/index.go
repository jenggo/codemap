package workspace

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"time"

	"codemap/parse"
	"codemap/resolve"
	"codemap/store"
	"codemap/vcs"
)

// IndexSummary reports one member's indexing result.
type IndexSummary struct {
	Repo     string
	Dir      string
	State    string
	Packages int
	Symbols  int
	Edges    int
	Missing  bool
}

// IndexAll indexes every member of the workspace into dbPath, attributing
// packages/symbols/edges/files to their repo. Missing members are registered
// but not parsed; queries for them will report "repo not indexed".
func IndexAll(cfg *Config, dbPath string) ([]IndexSummary, error) {
	specs := make([]store.RepoSpec, 0, len(cfg.Members))
	roots := make([]parse.WorkspaceRoot, 0, len(cfg.Members))
	for _, m := range cfg.Members {
		specs = append(specs, store.RepoSpec{ModulePath: m.Module, Dir: m.Dir, Missing: m.Missing})
		if !m.Missing {
			roots = append(roots, parse.WorkspaceRoot{Dir: m.Dir, ModulePath: m.Module})
		}
	}

	parseResult, err := parse.RunWorkspace(roots)
	if err != nil {
		return nil, fmt.Errorf("workspace parse: %w", err)
	}

	resolveResult := resolve.Run(parseResult)
	churn, churnErr := mergedChurn(cfg.Existing())

	s, err := store.Create(dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.WriteWorkspace(resolveResult, parse.FileContents(parseResult), churn, specs); err != nil {
		return nil, fmt.Errorf("workspace write: %w", err)
	}

	now := time.Now()
	for _, m := range cfg.Existing() {
		if err := s.SetRepoIndexedAt(m.Module, now); err != nil {
			return nil, fmt.Errorf("set indexed_at for %s: %w", m.Module, err)
		}
		s.RecordDirtyFingerprint(m.Dir, m.Module)
	}
	if err := s.SetChurnDegraded(churnReason(churnErr)); err != nil {
		return nil, fmt.Errorf("set churn degradation: %w", err)
	}
	for _, m := range cfg.Members {
		if m.Missing {
			if err := s.SetRepoMissing(m.Module, true); err != nil {
				return nil, err
			}
		}
	}

	return buildSummaries(cfg, resolveResult), nil
}

// ReindexRepo re-indexes a single member (parse + write) into an existing
// workspace database, replacing only that repo's rows.
func ReindexRepo(dbPath, modulePath, dir string) (*IndexSummary, error) {
	parseResult, err := parse.RunWorkspace([]parse.WorkspaceRoot{{Dir: dir, ModulePath: modulePath}})
	if err != nil {
		return nil, fmt.Errorf("workspace parse: %w", err)
	}
	resolveResult := resolve.Run(parseResult)

	s, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()

	churn, churnErr := vcs.GitFileChurn(dir, "HEAD")
	if err := s.ReplaceRepo(resolveResult, parse.FileContents(parseResult), churn, modulePath); err != nil {
		return nil, fmt.Errorf("replace repo %s: %w", modulePath, err)
	}
	if err := s.SetRepoIndexedAt(modulePath, time.Now()); err != nil {
		return nil, err
	}
	s.RecordDirtyFingerprint(dir, modulePath)
	if err := s.SetChurnDegraded(churnReason(churnErr)); err != nil {
		return nil, fmt.Errorf("set churn degradation: %w", err)
	}
	return &IndexSummary{
		Repo:     modulePath,
		Dir:      dir,
		Packages: len(resolveResult.Packages),
		Symbols:  len(resolveResult.Symbols),
		Edges:    len(resolveResult.Edges),
	}, nil
}

// ReindexStale re-indexes every member whose checkout has newer .go files than
// its last index. Returns the list of repos that were reindexed and any errors
// (a member that cannot be reindexed is reported, not fatal).
func ReindexStale(dbPath string) ([]string, error) {
	s, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()

	repos, err := s.ListRepos()
	if err != nil {
		return nil, err
	}

	var reindexed []string
	var errs []error
	for _, r := range repos {
		if r.Missing {
			continue
		}
		if _, err := os.Stat(r.Dir); os.IsNotExist(err) {
			if merr := s.SetRepoMissing(r.ModulePath, true); merr != nil {
				errs = append(errs, fmt.Errorf("mark %s missing: %w", r.ModulePath, merr))
			}
			continue
		}
		stale, err := s.IsRepoStale(r.ModulePath)
		if err != nil {
			errs = append(errs, fmt.Errorf("staleness check for %s: %w", r.ModulePath, err))
			continue
		}
		if stale {
			if _, err := ReindexRepo(dbPath, r.ModulePath, r.Dir); err != nil {
				errs = append(errs, fmt.Errorf("reindexing %s: %w", r.ModulePath, err))
				continue
			}
			reindexed = append(reindexed, r.ModulePath)
		}
	}
	return reindexed, errors.Join(errs...)
}

func buildSummaries(cfg *Config, result *resolve.Result) []IndexSummary {
	summaries := make([]IndexSummary, 0, len(cfg.Members))
	existing := make(map[string]bool)
	for _, m := range cfg.Existing() {
		existing[m.Module] = true
	}
	// Package counts per module.
	pkgCount := make(map[string]int)
	symCount := make(map[string]int)
	edgeCount := make(map[string]int)
	for _, p := range result.Packages {
		pkgCount[p.ModulePath]++
	}
	for _, sym := range result.Symbols {
		for module := range existing {
			if sym.Symbol.QualifiedName == module || hasPrefix(sym.Symbol.QualifiedName, module) {
				symCount[module]++
				break
			}
		}
	}
	for _, e := range result.Edges {
		for module := range existing {
			if e.Edge.FromRef == module || hasPrefix(e.Edge.FromRef, module) {
				edgeCount[module]++
				break
			}
		}
	}
	for _, m := range cfg.Members {
		summaries = append(summaries, IndexSummary{
			Repo:     m.Module,
			Dir:      m.Dir,
			Missing:  m.Missing,
			Packages: pkgCount[m.Module],
			Symbols:  symCount[m.Module],
			Edges:    edgeCount[m.Module],
			State:    "indexed",
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Repo < summaries[j].Repo })
	return summaries
}

func hasPrefix(s, module string) bool {
	return len(s) > len(module) && s[:len(module)] == module && (s[len(module)] == '/' || s[len(module)] == '.')
}

// mergedChurn combines git churn (per-file commit counts) across all member
// checkouts. It returns the first non-benign failure (anything but an empty
// repo) so callers can record degraded churn instead of pretending the data
// exists.
func mergedChurn(members []Member) (map[string]int, error) {
	out := make(map[string]int)
	var firstErr error
	for _, m := range members {
		churn, err := vcs.GitFileChurn(m.Dir, "HEAD")
		if err != nil {
			if !errors.Is(err, vcs.ErrNoCommits) && firstErr == nil {
				firstErr = fmt.Errorf("git churn for %s: %w", m.Module, err)
			}
			continue
		}
		maps.Copy(out, churn)
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, firstErr
}

// churnReason converts a churn-gathering failure into the reason stored for
// degraded-churn reporting; benign empty repos produce "" (no degradation).
func churnReason(err error) string {
	if err == nil {
		return ""
	}
	return "git churn unavailable: " + err.Error()
}
