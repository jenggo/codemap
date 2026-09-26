package vcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Hunk struct {
	Added    []int
	Removed  []int
	OldStart int
	OldCount int
	NewStart int
	NewCount int
}

func IsGitRepo(repoDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "not a git repository"
		}
		return fmt.Errorf("%s: %s", repoDir, msg)
	}
	return nil
}

func ensureGitAvailable() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git executable not found in PATH: %w", err)
	}
	return nil
}

func runGit(repoDir string, args ...string) (string, error) {
	if err := ensureGitAvailable(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

// headRef is the git ref used to identify the current commit, both in
// porcelain commands and as the detached-HEAD branch name.
const headRef = "HEAD"

// DefaultBranch returns the repository's integration branch name (from the
// remote's HEAD), falling back to the current branch, then to "HEAD" when no
// branch can be determined. This lets tools diff against a sensible base
// regardless of whether the default branch is called main or master.
func DefaultBranch(repoDir string) string {
	if err := IsGitRepo(repoDir); err != nil {
		return headRef
	}
	if out, err := runGit(repoDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		branch := strings.TrimSpace(out)
		if i := strings.Index(branch, "/"); i >= 0 {
			branch = branch[i+1:]
		}
		if branch != "" {
			return branch
		}
	}
	if out, err := runGit(repoDir, "rev-parse", "--abbrev-ref", headRef); err == nil {
		branch := strings.TrimSpace(out)
		if branch != "" && branch != headRef {
			return branch
		}
	}
	return headRef
}

// GitHead returns the current HEAD commit hash of the repo at repoDir.
func GitHead(repoDir string) (string, error) {
	out, err := runGit(repoDir, "rev-parse", headRef)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// runGitNUL runs git with a NUL-delimited -z output and returns the records.
// -z suppresses git's C-style path quoting, so names holding tabs, quotes or
// backslashes arrive verbatim instead of wrapped in quotes with escapes.
func runGitNUL(repoDir string, args ...string) ([]string, error) {
	out, err := runGit(repoDir, args...)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// splitNUL splits git -z output into records, dropping the empty record that
// separates entries.
func splitNUL(out string) []string {
	records := make([]string, 0, strings.Count(out, "\x00"))
	for rec := range strings.SplitSeq(out, "\x00") {
		if rec == "" {
			continue
		}
		records = append(records, rec)
	}
	return records
}

func GitChangedFiles(repoDir, ref string) ([]string, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	records, err := runGitNUL(repoDir, "diff", ref, "--name-only", "-z", "--", "*.go")
	if err != nil {
		return nil, err
	}
	return filterGoPaths(records), nil
}

// filterGoPaths keeps only .go paths. Records arrive unquoted from -z, so no
// trimming is applied: a leading space can be part of a real filename.
func filterGoPaths(records []string) []string {
	files := records[:0]
	for _, rec := range records {
		if strings.HasSuffix(rec, ".go") {
			files = append(files, rec)
		}
	}
	return files
}

type FileStatus struct {
	Path   string
	Status string
	// OldPath is the pre-rename path for an R/C status, empty otherwise.
	OldPath string
}

// GitFileStatuses returns each changed .go file with its diff status. -z is
// required rather than cosmetic: without it a rename is emitted as one
// "R100\told\tnew" line that no tab split can separate, and paths holding
// tabs, quotes or backslashes arrive quoted and escaped.
//
// With -z the records alternate status, path, status, path..., and a rename
// contributes status, old path, new path.
func GitFileStatuses(repoDir, ref string) ([]FileStatus, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	records, err := runGitNUL(repoDir, "diff", ref, "--name-status", "-z", "--", "*.go")
	if err != nil {
		return nil, err
	}
	var statuses []FileStatus
	for i := 0; i+1 < len(records); i += 2 {
		status := records[i]
		path := records[i+1]
		oldPath := ""
		if isRenameStatus(status) {
			if i+2 >= len(records) {
				break
			}
			oldPath = path
			path = records[i+2]
			i++
		}
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		statuses = append(statuses, FileStatus{Status: status, Path: path, OldPath: oldPath})
	}
	return statuses, nil
}

// isRenameStatus reports a git name-status code carrying two paths (R/C plus a
// similarity score, e.g. R100).
func isRenameStatus(status string) bool {
	return len(status) > 0 && (status[0] == 'R' || status[0] == 'C')
}

func GitHunks(repoDir, ref, file string) ([]Hunk, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	out, err := runGit(repoDir, "diff", ref, "--", file)
	if err != nil {
		return nil, err
	}
	return parseHunks(out), nil
}

func parseHunks(out string) []Hunk {
	var hunks []Hunk
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		h, ok := parseHunkHeader(line)
		if !ok {
			continue
		}
		h.Removed = computeRemovedRange(h.OldStart, h.OldCount)
		h.Added = computeAddedRange(h.NewStart, h.NewCount)
		hunks = append(hunks, h)
	}
	return hunks
}

func parseHunkHeader(line string) (Hunk, bool) {
	line = strings.TrimPrefix(line, "@@")
	body, cut, ok := strings.Cut(line, "@@")
	if !ok {
		return Hunk{}, false
	}
	_ = cut
	body = strings.TrimSpace(body)
	parts := strings.Fields(body)
	if len(parts) < 2 {
		return Hunk{}, false
	}
	oldStart, oldCount, ok1 := parseRange(parts[0])
	newStart, newCount, ok2 := parseRange(parts[1])
	if !ok1 || !ok2 {
		return Hunk{}, false
	}
	return Hunk{
		OldStart: oldStart,
		OldCount: oldCount,
		NewStart: newStart,
		NewCount: newCount,
	}, true
}

func parseRange(spec string) (int, int, bool) {
	comma := strings.Index(spec, ",")
	if comma < 0 {
		n, err := strconv.Atoi(spec[1:])
		if err != nil {
			return 0, 0, false
		}
		return n, 1, true
	}
	start, err := strconv.Atoi(spec[1:comma])
	if err != nil {
		return 0, 0, false
	}
	count, err := strconv.Atoi(spec[comma+1:])
	if err != nil {
		return 0, 0, false
	}
	return start, count, true
}

func computeAddedRange(start, count int) []int {
	if count <= 0 {
		return nil
	}
	out := make([]int, 0, count)
	for i := range count {
		out = append(out, start+i)
	}
	return out
}

func computeRemovedRange(start, count int) []int {
	if count <= 0 {
		return nil
	}
	out := make([]int, 0, count)
	for i := range count {
		out = append(out, start+i)
	}
	return out
}

func GitDeletedFiles(repoDir, ref string) ([]string, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	records, err := runGitNUL(repoDir, "diff", ref, "--name-only", "--diff-filter=D", "-z", "--", "*.go")
	if err != nil {
		return nil, err
	}
	return filterGoPaths(records), nil
}

func ResolveRepoDir(repoDir string) (string, error) {
	if repoDir == "" {
		repoDir = "."
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}
	return abs, nil
}

func GitShowFile(repoDir, ref, file string) ([]byte, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	out, err := runGit(repoDir, "show", ref+":"+file)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// GitDirtyDiff reports whether repoDir has uncommitted .go changes relative to
// ref and returns a fingerprint of that diff. The fingerprint lets callers
// check whether an index already reflects this exact working-tree state, so a
// repo that is legitimately dirty (in-progress edits) is not reindexed after
// every query. Returns an error when git is unavailable or repoDir is not a
// repository, so callers can fall back to mtime-only staleness checks.
func GitDirtyDiff(repoDir, ref string) (bool, string, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return false, "", err
	}
	out, err := runGit(repoDir, "diff", ref, "--", "*.go")
	if err != nil {
		return false, "", err
	}
	sum := sha256.Sum256([]byte(out))
	return strings.TrimSpace(out) != "", fmt.Sprintf("%x", sum[:]), nil
}

// ErrNoCommits reports a repository that has never had a commit. Churn is
// legitimately empty there; callers need not treat it as degraded.
var ErrNoCommits = errors.New("no commits in repository")

// GitFileChurn returns per-file commit counts for .go files in repoDir, or an
// error when git itself fails (missing binary, not a repository, corrupt repo).
// A repository with no commits yields ErrNoCommits, not an empty map with nil.
func GitFileChurn(repoDir, ref string) (map[string]int, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	args := []string{"log", "--format=format:", "--name-only", "-z"}
	if ref != "" {
		args = append(args, ref)
	}
	args = append(args, "--", "*.go")
	records, err := runGitNUL(repoDir, args...)
	if err != nil {
		if isNoCommitsErr(err) {
			return nil, ErrNoCommits
		}
		return nil, err
	}
	churn := make(map[string]int)
	// -z separates commits with a double NUL, which splitNUL collapses into the
	// same empty record it drops, so counting names needs no commit framing.
	for _, name := range records {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		churn[name]++
	}
	return churn, nil
}

func isNoCommitsErr(err error) bool {
	msg := err.Error()
	// Different git versions phrase an empty repository differently: some say
	// "does not have any commits yet", others fail to resolve HEAD.
	return strings.Contains(msg, "does not have any commits yet") ||
		strings.Contains(msg, "does not have any commits") ||
		strings.Contains(msg, "bad revision 'HEAD'") ||
		strings.Contains(msg, "unknown revision") ||
		strings.Contains(msg, "unknown revision or path not in the working tree")
}
