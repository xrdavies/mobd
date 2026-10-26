package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seed(name string) Agent {
	if name == "claude" {
		return Agent{Models: []string{"opus", "sonnet", "haiku"}}
	}
	return Agent{}
}

func TestEnsureCreatesCompleteSettingsAndAddsNewAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := Open(path)
	if err := s.Ensure([]string{"codex", "claude"}, seed); err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if got.Rev != 1 || got.DefaultAgent != "codex" || got.Agents["codex"].Permissions != "bypass" ||
		got.Agents["codex"].Models == nil || len(got.Agents["claude"].Models) != 3 {
		t.Fatalf("created: %+v", got)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"models": []`) || !strings.Contains(string(data), `"defaultEffort": ""`) {
		t.Fatalf("file is not explicit:\n%s", data)
	}
	// Unchanged: nothing is written.
	if err := s.Ensure([]string{"codex", "claude"}, seed); err != nil || s.Get().Rev != 1 {
		t.Fatalf("rev %d %v", s.Get().Rev, err)
	}
	// Pi enabled later gets an entry; codex disabled keeps its own; the default follows.
	if err := Open(path).Ensure([]string{"claude", "pi"}, seed); err != nil {
		t.Fatal(err)
	}
	got = Open(path).Get()
	if got.Rev != 2 || got.DefaultAgent != "claude" || len(got.Agents) != 3 {
		t.Fatalf("after enabling pi: %+v", got)
	}
}

func TestReplaceChecksRevisionAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := Open(path)
	s.Ensure([]string{"codex"}, seed)
	ok := func(Settings, Settings) error { return nil }
	next := s.Get()
	next.Agents["codex"] = Agent{Models: []string{"gpt-6-astra"}, DefaultEffort: "high"}
	stored, previous, err := s.Replace(next, ok)
	if err != nil || stored.Rev != 2 || previous.Rev != 1 || stored.Agents["codex"].Permissions != "bypass" {
		t.Fatalf("%+v %+v %v", stored, previous, err)
	}
	next.Agents["codex"].Models[0] = "changed"
	if s.Get().Agents["codex"].Models[0] != "gpt-6-astra" {
		t.Fatal("store shares the caller's slice")
	}
	if _, _, err := s.Replace(next, ok); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	bad := errors.New("bad")
	if _, _, err := s.Replace(s.Get(), func(Settings, Settings) error { return bad }); err != bad {
		t.Fatalf("validate: %v", err)
	}
	if got := Open(path).Get(); got.Rev != 2 || got.Agents["codex"].DefaultEffort != "high" {
		t.Fatalf("reopened: %+v", got)
	}
}

func TestBrokenFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte("{oops"), 0600)
	s := Open(path)
	if err := s.Ensure([]string{"codex"}, seed); err != nil || s.Get().Rev != 1 {
		t.Fatalf("%+v %v", s.Get(), err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !strings.HasPrefix(names[1], "settings.json.bad-") && !strings.HasPrefix(names[0], "settings.json.bad-") {
		t.Fatalf("entries %v", names)
	}
}
