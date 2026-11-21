package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func agentsLoadErr(t *testing.T, data string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(data), 0600)
	_, _, err := Load([]string{"--config", path})
	return err
}

func TestNativeSectionsAndFlatKeysLoadAlike(t *testing.T) {
	flat := loadTest(t, `{"claudeBin":"/x/claude","claudeSettingSources":"all","codexMode":"exec","piBin":"/x/pi","runners":["claude","codex","pi"]}`)
	sections := loadTest(t, `{"runners":["claude","codex","pi"],"agents":{"claude":{"bin":"/x/claude","settingSources":"all"},"codex":{"mode":"exec"},"pi":{"bin":"/x/pi"}}}`)
	for _, c := range []Config{flat, sections} {
		if c.ClaudeBin != "/x/claude" || c.ClaudeSettingSources != "all" || c.CodexMode != "exec" || c.PiBin != "/x/pi" {
			t.Fatalf("%+v", c)
		}
		if _, ok := c.Agents["claude"]; ok {
			t.Fatal("native sections stay in Agents")
		}
	}
	// A service snapshot written from the loaded config loads the same way.
	snap, _ := json.Marshal(sections)
	again := loadTest(t, string(snap))
	if again.ClaudeBin != "/x/claude" || again.CodexMode != "exec" {
		t.Fatalf("snapshot %+v", again)
	}
}

func TestAgentSectionErrors(t *testing.T) {
	for data, want := range map[string]string{
		`{"claudeBin":"/a","agents":{"claude":{"bin":"/b"}}}`:                 "both set",
		`{"agents":{"codex":{"workdir":"/w"}}}`:                               "does not take workdir",
		`{"agents":{"claude":{"protocol":"acp"}}}`:                            "own adapter",
		`{"runners":["foo"]}`:                                                 "unknown runner",
		`{"runners":["codex","foo"],"agents":{"foo":{"bin":"foo"}}}`:          "protocol",
		`{"runners":["codex"],"agents":{"x":{"protocol":"acp"}}}`:             "must name the agent's program",
		`{"runners":["codex"],"agents":{"Bad":{"protocol":"acp","bin":"b"}}}`: "lowercase",
	} {
		if err := agentsLoadErr(t, data); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", data, want, err)
		}
	}
}

func TestACPAgentsFromPresetsAndSections(t *testing.T) {
	c := loadTest(t, `{"runners":["codex","opencode","gemini"],"agents":{"gemini":{"protocol":"acp","label":"Gemini CLI","bin":"gemini","args":["--acp"]},"dsh":{"bin":"/opt/dsh"}}}`)
	oc, ok := c.ACP("opencode")
	if !ok || oc.Bin != "opencode" || strings.Join(oc.Args, " ") != "acp" || oc.Label != "OpenCode" || oc.Workdir != c.CodexWorkDir {
		t.Fatalf("preset %+v", oc)
	}
	g, ok := c.ACP("gemini")
	if !ok || g.Label != "Gemini CLI" || g.Args[0] != "--acp" {
		t.Fatalf("section %+v", g)
	}
	// A preset's section overrides only what it sets.
	d, ok := c.ACP("dsh")
	if !ok || d.Bin != "/opt/dsh" || d.Label != "DeepSeek Harness" || d.Args[1] != "acp" {
		t.Fatalf("preset override %+v", d)
	}
	if _, ok := c.ACP("codex"); ok {
		t.Fatal("codex is native")
	}
}
