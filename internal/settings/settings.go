// Package settings keeps the machine settings changed from the apps (docs/mobd-v3.md,
// "Machine settings"): the default agent, and per agent its model list, default model,
// default effort and permissions. They are stored apart from config.json, which mobd never
// rewrites; an unset field falls through to config.json, then to the agent's own settings.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Agent is what the apps set for one agent; nil means not set here.
type Agent struct {
	Models        []string `json:"models,omitempty"`
	DefaultModel  string   `json:"defaultModel,omitempty"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
	Permissions   string   `json:"permissions,omitempty"`
}

// Settings is the whole stored document. Rev goes up on every change; a write must name the
// revision it was based on, so two devices cannot silently overwrite each other.
type Settings struct {
	Rev          int64            `json:"rev"`
	DefaultAgent string           `json:"defaultAgent,omitempty"`
	Agents       map[string]Agent `json:"agents,omitempty"`
}

// ErrConflict means the write was based on an older revision.
var ErrConflict = errors.New("settings changed on another device; reload them and try again")

// Store holds the settings in a JSON file, replaced atomically on each change.
type Store struct {
	mu   sync.Mutex
	path string
	data Settings
}

// Open reads path; a missing file is empty settings. An unreadable file is moved aside (so a
// later save does not lose it) and reported, and mobd starts without app settings.
func Open(path string) *Store {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Printf("settings: cannot read %s: %v; starting without app settings", path, err)
	default:
		if err := json.Unmarshal(data, &s.data); err != nil {
			aside := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
			_ = os.Rename(path, aside)
			log.Printf("settings: %s is not valid JSON (%v); moved to %s, starting without app settings", path, err, aside)
			s.data = Settings{}
		}
	}
	return s
}

// Get returns a copy of the settings.
func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.clone()
}

// Replace stores next if it is based on the current revision and validate accepts it. It
// returns the stored settings (with the new revision) and the previous ones.
func (s *Store) Replace(next Settings, validate func(Settings) error) (stored, previous Settings, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next.Rev != s.data.Rev {
		return Settings{}, Settings{}, ErrConflict
	}
	next = next.clone()
	for name, a := range next.Agents {
		if len(a.Models) == 0 {
			a.Models = nil // an empty list is "not set": the config's list applies
			next.Agents[name] = a
		}
		if a.empty() {
			delete(next.Agents, name)
		}
	}
	if err := validate(next); err != nil {
		return Settings{}, Settings{}, err
	}
	next.Rev = s.data.Rev + 1
	if err := save(s.path, next); err != nil {
		return Settings{}, Settings{}, fmt.Errorf("save settings: %w", err)
	}
	previous, s.data = s.data, next
	return next.clone(), previous.clone(), nil
}

func (a Agent) empty() bool {
	return a.Models == nil && a.DefaultModel == "" && a.DefaultEffort == "" && a.Permissions == ""
}

func (s Settings) clone() Settings {
	c := s
	if s.Agents != nil {
		c.Agents = make(map[string]Agent, len(s.Agents))
		for k, v := range s.Agents {
			v.Models = slices.Clone(v.Models)
			c.Agents[k] = v
		}
	}
	return c
}

func save(path string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
