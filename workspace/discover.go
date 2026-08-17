package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Candidate is a sibling Go module found by auto-discovery.
type Candidate struct {
	Module string
	Dir    string
}

// Discover finds sibling Go modules that could join the workspace. It scans
// the sibling root (the parent of dir) one level deep for directories
// containing a go.mod. When the current module path contains a "/", only
// siblings whose module path shares that directory prefix are returned
// (module-path-prefix match); bare module names include all siblings.
func Discover(modulePath, dir string) ([]Candidate, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	siblingRoot := filepath.Dir(abs)

	entries, err := os.ReadDir(siblingRoot)
	if err != nil {
		return nil, fmt.Errorf("discovery: reading %s: %w", siblingRoot, err)
	}

	prefix := ""
	if strings.Contains(modulePath, "/") {
		// "github.com/jenggo/emak" -> "github.com/jenggo/" so sibling
		// modules under the same org/group are discovered.
		prefix = modulePath[:strings.LastIndex(modulePath, "/")+1]
	}

	var candidates []Candidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") || e.Name() == "vendor" || e.Name() == "node_modules" {
			continue
		}
		candDir := filepath.Join(siblingRoot, e.Name())
		if candDir == abs {
			continue
		}
		candModule, err := ReadModulePath(candDir)
		if err != nil {
			continue
		}
		if prefix != "" && !strings.HasPrefix(candModule, prefix) {
			continue
		}
		candidates = append(candidates, Candidate{Module: candModule, Dir: candDir})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Module < candidates[j].Module
	})
	return candidates, nil
}
