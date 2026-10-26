package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
)

func TestDefaultsComeFromCodexConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	write := func(s string) { os.WriteFile(filepath.Join(home, "config.toml"), []byte(s), 0600) }
	if m, e := defaults(); m != "" || e != "" {
		t.Fatalf("no config: %q %q", m, e)
	}
	write(`model_provider = "OpenAI"
model = "gpt-6-astra"   # the usual one
model_reasoning_effort = "xhigh"

[model_providers.OpenAI]
name = "OpenAI"
model = "not-a-default"
`)
	info := New(config.Config{}).Info()
	if info.DefaultModel != "gpt-6-astra" || info.DefaultEffort != "xhigh" {
		t.Fatalf("top level: %+v", info)
	}
	write(`model = "gpt-6-astra"
model_reasoning_effort = "xhigh"
profile = "quick"

[profiles.quick]
model_reasoning_effort = "low"
`)
	if m, e := defaults(); m != "gpt-6-astra" || e != "low" {
		t.Fatalf("profile: %q %q", m, e)
	}
}
