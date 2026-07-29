package vcs

import (
	"bytes"
	"context"
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

func GitChangedFiles(repoDir, ref string) ([]string, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	out, err := runGit(repoDir, "diff", ref, "--name-only", "--", "*.go")
	if err != nil {
		return nil, err
	}
	return parseChangedFiles(out, repoDir), nil
}

func parseChangedFiles(out, _ string) []string {
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".go") {
			continue
		}
		files = append(files, line)
	}
	return files
}

type FileStatus struct {
	Path   string
	Status string
}

func GitFileStatuses(repoDir, ref string) ([]FileStatus, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return nil, err
	}
	out, err := runGit(repoDir, "diff", ref, "--name-status", "--", "*.go")
	if err != nil {
		return nil, err
	}
	var statuses []FileStatus
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		statuses = append(statuses, FileStatus{Status: parts[0], Path: parts[1]})
	}
	return statuses, nil
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
	out, err := runGit(repoDir, "diff", ref, "--name-only", "--diff-filter=D", "--", "*.go")
	if err != nil {
		return nil, err
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, line)
	}
	return files, nil
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

func GitFileChurn(repoDir, ref string) (map[string]int, error) {
	if err := IsGitRepo(repoDir); err != nil {
		return map[string]int{}, nil
	}
	args := []string{"log", "--format=format:", "--name-only"}
	if ref != "" {
		args = append(args, ref)
	}
	args = append(args, "--", "*.go")
	out, err := runGit(repoDir, args...)
	if err != nil {
		return map[string]int{}, nil
	}
	churn := make(map[string]int)
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".go") {
			continue
		}
		churn[line]++
	}
	return churn, nil
}
