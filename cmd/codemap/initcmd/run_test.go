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
	if !strings.Contains(string(nav), "Version: 1.3") {
		t.Error("navigation.md missing version marker")
	}

	// Check codemap-guard.ts
	pluginPath := filepath.Join(tmpHome, ".config", "opencode", "plugins", "codemap-guard.ts")
	plugin, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("codemap-guard.ts not written: %v", err)
	}
	content := string(plugin)
	for _, want := range []string{
		"Version: 1.3",
		"CodemapGuard",
		"tool.execute.after",
		"baseToolName",
		"warnKey",
		"codemap_search",
		"codemap_show",
		"codemap_symbols_in_file",
		"codemap_changed_symbols",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("codemap-guard.ts missing %q", want)
		}
	}
	if strings.Contains(content, "codemap_list_packages") {
		t.Error("codemap-guard.ts still references removed tool codemap_list_packages")
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

func TestWriteIfChanged_ReportsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")

	changed, err := writeIfChanged(path, "v1")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !changed {
		t.Error("first write should report changed")
	}

	changed, err = writeIfChanged(path, "v1")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if changed {
		t.Error("identical content should report unchanged")
	}

	changed, err = writeIfChanged(path, "v2")
	if err != nil {
		t.Fatalf("third write: %v", err)
	}
	if !changed {
		t.Error("different content should report changed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v2" {
		t.Errorf("content = %q, want v2", data)
	}
}

func TestInjectCommandCode_WritesMod(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".commandcode"), 0755); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		if err := injectCommandCode(); err != nil {
			t.Fatalf("injectCommandCode run %d failed: %v", i+1, err)
		}
	}

	modPath := filepath.Join(tmpHome, ".commandcode", "mods", "codemap-guard.ts")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatalf("codemap-guard.ts not written: %v", err)
	}
	content := string(mod)
	for _, want := range []string{
		"Version: 1.0",
		"afterToolCall",
		"additionalContext",
		"baseToolName",
		"codemap_search",
		"codemap_show",
		"codemap_symbols_in_file",
		"codemap_changed_symbols",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("commandcode mod missing %q", want)
		}
	}
	if strings.Contains(content, "codemap_list_packages") {
		t.Error("commandcode mod still references removed tool codemap_list_packages")
	}
}

func TestInjectCommandCode_SkipsWithoutCommandCode(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := injectCommandCode(); err != nil {
		t.Fatalf("injectCommandCode should skip without error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpHome, ".commandcode")); !os.IsNotExist(err) {
		t.Error("~/.commandcode must not be created when absent")
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
