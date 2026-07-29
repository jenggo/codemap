package initcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectOpencode_WritesNavigationAndPlugin(t *testing.T) {
	// Use a temp HOME so we don't touch real config
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := injectOpencode(); err != nil {
		t.Fatalf("injectOpencode failed: %v", err)
	}

	// Check navigation.md
	navPath := filepath.Join(tmpHome, ".config", "opencode", "context", "codemap", "navigation.md")
	nav, err := os.ReadFile(navPath)
	if err != nil {
		t.Fatalf("navigation.md not written: %v", err)
	}
	if !strings.Contains(string(nav), "Codemap MCP Tools") {
		t.Error("navigation.md missing expected content")
	}

	// Check codemap-guard.ts
	pluginPath := filepath.Join(tmpHome, ".config", "opencode", "plugins", "codemap-guard.ts")
	plugin, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("codemap-guard.ts not written: %v", err)
	}
	content := string(plugin)
	for _, want := range []string{"CodemapGuard", "tool.execute.before", "codemap_search", "codemap_show"} {
		if !strings.Contains(content, want) {
			t.Errorf("codemap-guard.ts missing %q", want)
		}
	}
}

func TestInjectAgents_SkipsExisting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Create existing AGENTS.md
	agentsPath := filepath.Join(tmpDir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := injectAgents(); err != nil {
		t.Fatalf("injectAgents failed: %v", err)
	}

	data, _ := os.ReadFile(agentsPath)
	if string(data) != "existing" {
		t.Error("injectAgents overwrote existing AGENTS.md")
	}
}

func TestInjectAgents_CreatesNew(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	if err := injectAgents(); err != nil {
		t.Fatalf("injectAgents failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md not created: %v", err)
	}
	if !strings.Contains(string(data), "codemap") {
		t.Error("AGENTS.md missing codemap content")
	}
}

func TestInjectOpencode_Idempotent(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Run twice
	for i := range 2 {
		if err := injectOpencode(); err != nil {
			t.Fatalf("injectOpencode run %d failed: %v", i+1, err)
		}
	}

	// Files should still exist and be valid
	navPath := filepath.Join(tmpHome, ".config", "opencode", "context", "codemap", "navigation.md")
	if _, err := os.Stat(navPath); err != nil {
		t.Fatal("navigation.md missing after second run")
	}
	pluginPath := filepath.Join(tmpHome, ".config", "opencode", "plugins", "codemap-guard.ts")
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatal("codemap-guard.ts missing after second run")
	}
}
