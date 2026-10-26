package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
)

func TestDefaultsFollowTheSettingSourcesMobdLoads(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ANTHROPIC_MODEL", "")
	os.MkdirAll(filepath.Join(home, ".claude"), 0700)
	os.MkdirAll(filepath.Join(work, ".claude"), 0700)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"model":"opus","effortLevel":"xhigh"}`), 0600)
	info := func(sources string) (string, string) {
		i := New(config.Config{ClaudeWorkDir: work, ClaudeSettingSources: sources}).Info()
		return i.DefaultModel, i.DefaultEffort
	}
	// Only project settings are loaded: the personal ones do not apply.
	if m, e := info("project"); m != "" || e != "" {
		t.Fatalf("project only: %q %q", m, e)
	}
	if m, e := info("all"); m != "opus" || e != "xhigh" {
		t.Fatalf("all: %q %q", m, e)
	}
	os.WriteFile(filepath.Join(work, ".claude", "settings.json"), []byte(`{"effortLevel":"medium"}`), 0600)
	os.WriteFile(filepath.Join(work, ".claude", "settings.local.json"), []byte(`{"model":"sonnet"}`), 0600)
	if m, e := info("local,project,user"); m != "sonnet" || e != "medium" {
		t.Fatalf("later sources win: %q %q", m, e)
	}
	t.Setenv("ANTHROPIC_MODEL", "haiku")
	if m, _ := info("project"); m != "haiku" {
		t.Fatalf("ANTHROPIC_MODEL: %q", m)
	}
}
