package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceChecksRevisionValidatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := Open(path)
	if got := s.Get(); got.Rev != 0 || got.Agents != nil {
		t.Fatalf("empty: %+v", got)
	}
	ok := func(Settings) error { return nil }
	next := Settings{DefaultAgent: "claude", Agents: map[string]Agent{
		"codex":  {Models: []string{"gpt-6-astra"}, DefaultEffort: "high"},
		"claude": {Models: []string{}}, // nothing set: dropped
	}}
	stored, previous, err := s.Replace(next, ok)
	if err != nil || stored.Rev != 1 || previous.Rev != 0 || len(stored.Agents) != 1 {
		t.Fatalf("%+v %+v %v", stored, previous, err)
	}
	// The caller's slice is not shared with the store.
	next.Agents["codex"].Models[0] = "changed"
	if s.Get().Agents["codex"].Models[0] != "gpt-6-astra" {
		t.Fatal("store shares the caller's slice")
	}
	if _, _, err := s.Replace(Settings{Rev: 0}, ok); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	bad := errors.New("bad effort")
	if _, _, err := s.Replace(Settings{Rev: 1, DefaultAgent: "x"}, func(Settings) error { return bad }); err != bad {
		t.Fatalf("validate: %v", err)
	}
	if got := Open(path).Get(); got.Rev != 1 || got.DefaultAgent != "claude" || got.Agents["codex"].DefaultEffort != "high" {
		t.Fatalf("reopened: %+v", got)
	}
}

func TestBrokenFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte("{oops"), 0600)
	if got := Open(path).Get(); got.Rev != 0 {
		t.Fatalf("%+v", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "settings.json.bad-") {
		t.Fatalf("entries %v", entries)
	}
}
