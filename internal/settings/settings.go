// Package settings keeps the runtime settings of agents and conversations (docs/mobd-v3.md,
// "Settings"): the default agent, and per agent its model list, default model, default effort
// and permissions. The apps change them; config.json only says how mobd itself runs.
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

// Agent holds one agent's settings. An empty DefaultModel or DefaultEffort sends none, so the
// agent uses its own default.
type Agent struct {
	Models        []string `json:"models"`
	DefaultModel  string   `json:"defaultModel"`
	DefaultEffort string   `json:"defaultEffort"`
	Permissions   string   `json:"permissions"`
}

// Settings is the whole file. Rev goes up on every change; a write must name the revision it
// was based on, so two devices cannot silently overwrite each other.
type Settings struct {
	Rev          int64            `json:"rev"`
	DefaultAgent string           `json:"defaultAgent"`
	Agents       map[string]Agent `json:"agents"`
}

// ErrConflict means the write was based on an older revision.
var ErrConflict = errors.New("settings changed on another device; reload them and try again")

// Store holds the settings in a JSON file, replaced atomically on each change.
type Store struct {
	mu   sync.Mutex
	path string
	data Settings
}

// Open reads path; a missing file is created by Ensure. An unreadable one is moved aside (so it
// is not lost) and reported, and mobd starts from fresh settings.
func Open(path string) *Store {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Printf("settings: cannot read %s: %v; starting from fresh settings", path, err)
	default:
		if err := json.Unmarshal(data, &s.data); err != nil {
			aside := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
			_ = os.Rename(path, aside)
			log.Printf("settings: %s is not valid JSON (%v); moved to %s, starting from fresh settings", path, err, aside)
			s.data = Settings{}
		}
	}
	return s
}

// Ensure gives every enabled agent an entry (from seed) and picks the first enabled agent as
// the default if there is none or it is not enabled, then saves if anything changed. Entries of
// agents that are not enabled are kept.
func (s *Store) Ensure(enabled []string, seed func(name string) Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, changed := s.data.clone(), s.data.Rev == 0
	if next.Agents == nil {
		next.Agents = map[string]Agent{}
	}
	for _, name := range enabled {
		if _, ok := next.Agents[name]; !ok {
			next.Agents[name] = seed(name).normal()
			changed = true
		}
	}
	if len(enabled) > 0 && !slices.Contains(enabled, next.DefaultAgent) {
		next.DefaultAgent, changed = enabled[0], true
	}
	if !changed {
		return nil
	}
	next.Rev = s.data.Rev + 1
	if err := save(s.path, next); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	s.data = next
	return nil
}

// Get returns a copy of the settings.
func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.clone()
}

// Replace stores next if it is based on the current revision and validate accepts it; validate
// gets the current settings too (it runs under the store's lock, so it must not call Get). It
// returns the stored settings (with the new revision) and the previous ones.
func (s *Store) Replace(next Settings, validate func(next, current Settings) error) (stored, previous Settings, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next.Rev != s.data.Rev {
		return Settings{}, Settings{}, ErrConflict
	}
	next = next.clone()
	for name, a := range next.Agents {
		next.Agents[name] = a.normal()
	}
	if err := validate(next, s.data); err != nil {
		return Settings{}, Settings{}, err
	}
	next.Rev = s.data.Rev + 1
	if err := save(s.path, next); err != nil {
		return Settings{}, Settings{}, fmt.Errorf("save settings: %w", err)
	}
	previous, s.data = s.data, next
	return next.clone(), previous.clone(), nil
}

// normal writes an empty model list as [] and missing permissions as bypass.
func (a Agent) normal() Agent {
	if a.Models == nil {
		a.Models = []string{}
	}
	if a.Permissions == "" {
		a.Permissions = "bypass"
	}
	return a
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
