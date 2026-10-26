package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/settings"
	"github.com/xrdavies/mobd/internal/task"
)

const maxModels = 50

// seedAgent is a new agent's entry in settings.json: Claude's aliases, which always work, or the
// default model of the agent's own config, so the model menu is not empty.
func seedAgent(info runner.Info) settings.Agent {
	a := settings.Agent{Models: []string{}, Permissions: "bypass"}
	switch {
	case info.Name == "claude":
		a.Models = []string{"opus", "sonnet", "haiku"}
	case info.DefaultModel != "":
		a.Models = []string{info.DefaultModel}
	}
	return a
}

// agentInfo is what an app needs besides the settings: the efforts an agent takes and the
// defaults of its own config (used when settings.json names none).
type agentInfo struct {
	Label              string   `json:"label"`
	Efforts            []string `json:"efforts"`
	AgentDefaultModel  string   `json:"agentDefaultModel,omitempty"`
	AgentDefaultEffort string   `json:"agentDefaultEffort,omitempty"`
}

type settingsView struct {
	// Settings is settings.json: edit it and PUT it back whole, with the same rev.
	Settings settings.Settings    `json:"settings"`
	Agents   map[string]agentInfo `json:"agents"`
	// Installed is every supported agent's installation state, enabled or not.
	Installed []agentStatus `json:"installed"`
}

func (s *Server) settingsView() settingsView {
	v := settingsView{Settings: s.settings.Get(), Agents: map[string]agentInfo{}, Installed: s.agentStatuses(context.Background())}
	for _, rn := range s.runners.All() {
		info := rn.Info()
		v.Agents[info.Name] = agentInfo{Label: info.Label, Efforts: info.Efforts, AgentDefaultModel: info.DefaultModel, AgentDefaultEffort: info.DefaultEffort}
	}
	return v
}

// agent is an agent's settings; ok is false when it has none (not enabled).
func (s *Server) agent(name string) (settings.Agent, bool) {
	a, ok := s.settings.Get().Agents[name]
	return a, ok
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var next settings.Settings
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid settings: "+err.Error())
		return
	}
	stored, previous, err := s.settings.Replace(next, s.validateSettings)
	switch {
	case errors.Is(err, settings.ErrConflict):
		writeErrCode(w, http.StatusConflict, "settings_changed", err.Error())
		return
	case errors.Is(err, errInvalidSetting):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		log.Printf("settings: %v", err)
		writeErr(w, http.StatusInternalServerError, "could not save the settings")
		return
	}
	s.applyDefaultAgent(stored)
	for _, change := range settingChanges(previous, stored) {
		log.Printf("settings: %q set %s", s.deviceName(r), change)
	}
	view := s.settingsView()
	s.hub.publish("settings", view)
	writeJSON(w, http.StatusOK, view)
}

var errInvalidSetting = errors.New("invalid setting")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalidSetting, fmt.Sprintf(format, args...))
}

// validateSettings accepts only values the agents can use; the store runs it before saving.
// Every enabled agent must have an entry; entries kept for agents not enabled are left alone.
func (s *Server) validateSettings(next, current settings.Settings) error {
	if _, ok := s.runners.Get(next.DefaultAgent); !ok {
		return invalid("default agent %q is not enabled on this mobd", next.DefaultAgent)
	}
	stored := current.Agents
	for _, rn := range s.runners.All() {
		if _, ok := next.Agents[rn.Info().Name]; !ok {
			return invalid("settings for %s are missing", rn.Info().Label)
		}
	}
	for name, a := range next.Agents {
		rn, enabled := s.runners.Get(name)
		if !enabled {
			if _, kept := stored[name]; !kept {
				return invalid("agent %q is not enabled on this mobd", name)
			}
			continue
		}
		info := rn.Info()
		if len(a.Models) > maxModels {
			return invalid("%s: at most %d models", info.Label, maxModels)
		}
		seen := map[string]bool{}
		for _, m := range a.Models {
			if !validModel.MatchString(m) {
				return invalid("%s: %q is not a model name", info.Label, m)
			}
			if seen[m] {
				return invalid("%s: %q is listed twice", info.Label, m)
			}
			seen[m] = true
		}
		if a.DefaultModel != "" && !validModel.MatchString(a.DefaultModel) {
			return invalid("%s: %q is not a model name", info.Label, a.DefaultModel)
		}
		if a.DefaultEffort != "" && !slices.Contains(info.Efforts, a.DefaultEffort) {
			return invalid("%s: effort must be one of %s", info.Label, strings.Join(info.Efforts, ", "))
		}
		switch a.Permissions {
		case "bypass":
		case "approve":
			if name == "codex" && s.cfg.CodexMode == "exec" {
				return invalid("Codex can only ask before commands in app-server mode (codexMode is exec)")
			}
		default:
			return invalid("%s: permissions must be bypass or approve", info.Label)
		}
	}
	return nil
}

// applyDefaultAgent makes new conversations use the settings' default agent.
func (s *Server) applyDefaultAgent(set settings.Settings) {
	if err := s.runners.SetDefault(set.DefaultAgent); err != nil {
		log.Printf("settings: default agent: %v", err)
	}
}

// settingChanges describes each changed field for the log, e.g. `claude defaultEffort "" → "high"`.
func settingChanges(old, next settings.Settings) []string {
	var out []string
	if old.DefaultAgent != next.DefaultAgent {
		out = append(out, fmt.Sprintf("defaultAgent %q → %q", old.DefaultAgent, next.DefaultAgent))
	}
	names := map[string]bool{}
	for n := range old.Agents {
		names[n] = true
	}
	for n := range next.Agents {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	slices.Sort(sorted)
	for _, n := range sorted {
		a, b := old.Agents[n], next.Agents[n]
		if !slices.Equal(a.Models, b.Models) {
			out = append(out, fmt.Sprintf("%s models %q → %q", n, a.Models, b.Models))
		}
		for _, f := range [][3]string{{"defaultModel", a.DefaultModel, b.DefaultModel}, {"defaultEffort", a.DefaultEffort, b.DefaultEffort}, {"permissions", a.Permissions, b.Permissions}} {
			if f[1] != f[2] {
				out = append(out, fmt.Sprintf("%s %s %q → %q", n, f[0], f[1], f[2]))
			}
		}
	}
	return out
}

// deviceName names the paired device making a request, for logs.
func (s *Server) deviceName(r *http.Request) string {
	id := pairing.ClientID(r.Context())
	for _, c := range s.pairing.Clients() {
		if c.ID == id {
			return c.Name
		}
	}
	return "unknown device"
}

// checkSessionSettings validates a conversation's new model or effort against its agent.
func (s *Server) checkSessionSettings(id string, patch task.SessionPatch) (string, int) {
	name, _, _, err := s.store.SessionSettings(id)
	if err != nil {
		return "session not found", http.StatusNotFound
	}
	rn, ok := s.runners.Get(name)
	if !ok {
		return "runner " + name + " is not enabled on this mobd", http.StatusConflict
	}
	info := rn.Info()
	if patch.Model != nil && *patch.Model != "" && !validModel.MatchString(*patch.Model) {
		return "invalid model", http.StatusBadRequest
	}
	if patch.Effort != nil && *patch.Effort != "" && !slices.Contains(info.Efforts, *patch.Effort) {
		return "effort must be one of " + strings.Join(info.Efforts, ", "), http.StatusBadRequest
	}
	return "", 0
}

// DefaultAgent is the agent new conversations use (settings.json).
func (s *Server) DefaultAgent() string { return s.runners.Default() }

// SetSupported names every agent mobd supports (enabled or not), for the installation check.
func (s *Server) SetSupported(all ...runner.Runner) { s.supported = all }
