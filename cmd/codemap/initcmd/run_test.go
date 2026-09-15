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

func TestInjectMaki_WritesGuardAndMCP(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := os.MkdirAll(filepath.Join(tmpHome, ".config", "maki"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := injectMaki(); err != nil {
		t.Fatalf("injectMaki failed: %v", err)
	}

	lua, err := os.ReadFile(filepath.Join(tmpHome, ".config", "maki", "lua", "codemap-guard.lua"))
	if err != nil {
		t.Fatalf("codemap-guard.lua not written: %v", err)
	}
	content := string(lua)
	for _, want := range []string{
		"codemap-guard",
		"set_slot(\"tool.\" .. name .. \".input\"",
		"set_slot(\"tool.\" .. name .. \".output\"",
		"codemap__search",
		"codemap__search_text",
		"codemap__methods_of",
		"codemap__index",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("codemap-guard.lua missing %q", want)
		}
	}

	init, err := os.ReadFile(filepath.Join(tmpHome, ".config", "maki", "init.lua"))
	if err != nil {
		t.Fatalf("init.lua not written: %v", err)
	}
	if !strings.Contains(string(init), `require("codemap-guard")`) {
		t.Error("init.lua missing require(\"codemap-guard\")")
	}

	perms, err := os.ReadFile(filepath.Join(tmpHome, ".config", "maki", "plugin.toml"))
	if err != nil {
		t.Fatalf("plugin.toml not written: %v", err)
	}
	for _, want := range []string{"fs_read", "fs_write", "net", "run", "env"} {
		if !strings.Contains(string(perms), want) {
			t.Errorf("plugin.toml missing %q", want)
		}
	}

	mcp, err := os.ReadFile(filepath.Join(tmpHome, ".config", "maki", "mcp.toml"))
	if err != nil {
		t.Fatalf("mcp.toml not written: %v", err)
	}
	mcpContent := string(mcp)
	if !strings.Contains(mcpContent, "[mcp.codemap]") {
		t.Error("mcp.toml missing [mcp.codemap]")
	}
	if !strings.Contains(mcpContent, "\"serve\"") {
		t.Error("mcp.toml missing serve arg")
	}
	if !strings.Contains(mcpContent, "always_load = true") {
		t.Error("mcp.toml missing always_load = true")
	}
}

func TestInjectMaki_SkipsWithoutConfigDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := injectMaki(); err != nil {
		t.Fatalf("injectMaki should not fail without maki: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpHome, ".config", "maki")); !os.IsNotExist(err) {
		t.Error("~/.config/maki must not be created when absent")
	}
	if _, err := os.Stat(filepath.Join(tmpHome, ".maki")); !os.IsNotExist(err) {
		t.Error("~/.maki must not be created when absent")
	}
}

func TestInjectMaki_PrefersDotMaki(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := os.MkdirAll(filepath.Join(tmpHome, ".maki"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpHome, ".config", "maki"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := injectMaki(); err != nil {
		t.Fatalf("injectMaki failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmpHome, ".maki", "lua", "codemap-guard.lua")); err != nil {
		t.Errorf("guard not written to ~/.maki: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpHome, ".config", "maki", "lua")); !os.IsNotExist(err) {
		t.Error("~/.config/maki must not be touched when ~/.maki exists")
	}
}

func TestInjectMaki_Idempotent(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := os.MkdirAll(filepath.Join(tmpHome, ".config", "maki"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := injectMaki(); err != nil {
		t.Fatalf("first injectMaki failed: %v", err)
	}

	luaPath := filepath.Join(tmpHome, ".config", "maki", "lua", "codemap-guard.lua")
	mcpPath := filepath.Join(tmpHome, ".config", "maki", "mcp.toml")
	initPath := filepath.Join(tmpHome, ".config", "maki", "init.lua")

	lua1, _ := os.ReadFile(luaPath)
	mcp1, _ := os.ReadFile(mcpPath)
	init1, _ := os.ReadFile(initPath)

	if err := injectMaki(); err != nil {
		t.Fatalf("second injectMaki failed: %v", err)
	}

	lua2, _ := os.ReadFile(luaPath)
	mcp2, _ := os.ReadFile(mcpPath)
	init2, _ := os.ReadFile(initPath)

	if string(lua1) != string(lua2) {
		t.Error("codemap-guard.lua changed on second run")
	}
	if string(mcp1) != string(mcp2) {
		t.Error("mcp.toml changed on second run")
	}
	if string(init1) != string(init2) {
		t.Error("init.lua changed on second run")
	}
}

func TestInjectMaki_RespectsExistingPluginTomlAndInitLua(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	dir := filepath.Join(tmpHome, ".config", "maki")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	existingPerms := "[permissions]\nfs_read = true\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.toml"), []byte(existingPerms), 0644); err != nil {
		t.Fatal(err)
	}
	existingInit := "-- my init\n"
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte(existingInit), 0644); err != nil {
		t.Fatal(err)
	}

	if err := injectMaki(); err != nil {
		t.Fatalf("injectMaki failed: %v", err)
	}

	perms, err := os.ReadFile(filepath.Join(dir, "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(perms) != existingPerms {
		t.Error("existing plugin.toml must not be modified")
	}

	init, err := os.ReadFile(filepath.Join(dir, "init.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(init), existingInit) {
		t.Error("existing init.lua content must be preserved")
	}
	if strings.Count(string(init), `require("codemap-guard")`) != 1 {
		t.Error("init.lua should gain require(\"codemap-guard\") exactly once")
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
