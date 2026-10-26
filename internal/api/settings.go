package api

import (
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

// Where a setting's value comes from: set in an app, in mobd's config, in the agent's own
// settings, or nothing set (the agent's built-in default).
const (
	fromApp     = "app"
	fromConfig  = "config"
	fromAgent   = "agent"
	fromBuiltin = "builtin"
)

const maxModels = 50

// effectiveAgent is what an agent uses on this machine now, and where each value comes from.
type effectiveAgent struct {
	Models            []string `json:"models"`
	ModelsFrom        string   `json:"modelsFrom"`
	DefaultModel      string   `json:"defaultModel,omitempty"`
	DefaultModelFrom  string   `json:"defaultModelFrom"`
	DefaultEffort     string   `json:"defaultEffort,omitempty"`
	DefaultEffortFrom string   `json:"defaultEffortFrom"`
	Permissions       string   `json:"permissions"`
	PermissionsFrom   string   `json:"permissionsFrom"`
	Efforts           []string `json:"efforts"`
	// Fallback is what applies without the app settings (models, defaults, permissions), so an
	// app can say what clearing a value returns to.
	Fallback *effectiveAgent `json:"fallback,omitempty"`
}

// configuredPermissions is an agent's permissions from config.json and the environment.
func (s *Server) configuredPermissions(name string) string {
	p := map[string]string{"codex": s.cfg.CodexPermissions, "claude": s.cfg.ClaudePermissions, "pi": s.cfg.PiPermissions}[name]
	if p == "" {
		return "bypass"
	}
	return p
}

// effective resolves an agent's settings: app settings, then config, then the agent's own.
func (s *Server) effective(info runner.Info, set settings.Settings) effectiveAgent {
	app := set.Agents[info.Name]
	e := effectiveAgent{Models: info.Models, ModelsFrom: fromConfig, Efforts: info.Efforts,
		Permissions: s.configuredPermissions(info.Name), PermissionsFrom: fromConfig}
	if e.Models == nil {
		e.Models = []string{}
	}
	if app.Models != nil {
		e.Models, e.ModelsFrom = slices.Clone(app.Models), fromApp
	}
	pick := func(appValue, agentValue string) (string, string) {
		switch {
		case appValue != "":
			return appValue, fromApp
		case agentValue != "":
			return agentValue, fromAgent
		}
		return "", fromBuiltin
	}
	e.DefaultModel, e.DefaultModelFrom = pick(app.DefaultModel, info.DefaultModel)
	e.DefaultEffort, e.DefaultEffortFrom = pick(app.DefaultEffort, info.DefaultEffort)
	if app.Permissions != "" {
		e.Permissions, e.PermissionsFrom = app.Permissions, fromApp
	}
	return e
}

// agentSettings is effective for one enabled agent; ok is false if it is not enabled.
func (s *Server) agentSettings(name string) (effectiveAgent, bool) {
	rn, ok := s.runners.Get(name)
	if !ok {
		return effectiveAgent{}, false
	}
	return s.effective(rn.Info(), s.settings.Get()), true
}

type settingsView struct {
	// Settings are what the apps set (edit these and PUT them back with the same rev).
	Settings         settings.Settings         `json:"settings"`
	DefaultAgent     string                    `json:"defaultAgent"`
	DefaultAgentFrom string                    `json:"defaultAgentFrom"`
	// FallbackDefaultAgent is the default agent without the app setting (mobd's config).
	FallbackDefaultAgent string `json:"fallbackDefaultAgent"`
	Agents           map[string]effectiveAgent `json:"agents"`
}

func (s *Server) settingsView() settingsView {
	set := s.settings.Get()
	v := settingsView{Settings: set, DefaultAgent: s.runners.Default(), DefaultAgentFrom: fromConfig,
		FallbackDefaultAgent: s.configuredDefault, Agents: map[string]effectiveAgent{}}
	if set.DefaultAgent != "" {
		v.DefaultAgentFrom = fromApp
	}
	for _, rn := range s.runners.All() {
		e := s.effective(rn.Info(), set)
		fallback := s.effective(rn.Info(), settings.Settings{})
		e.Fallback = &fallback
		v.Agents[rn.Info().Name] = e
	}
	return v
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
func (s *Server) validateSettings(next settings.Settings) error {
	if next.DefaultAgent != "" {
		if _, ok := s.runners.Get(next.DefaultAgent); !ok {
			return invalid("default agent %q is not enabled on this mobd", next.DefaultAgent)
		}
	}
	for name, a := range next.Agents {
		rn, ok := s.runners.Get(name)
		if !ok {
			return invalid("agent %q is not enabled on this mobd", name)
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
		case "", "bypass":
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

// applyDefaultAgent makes new conversations use the settings' default agent, or the configured one.
func (s *Server) applyDefaultAgent(set settings.Settings) {
	name := set.DefaultAgent
	if name == "" {
		name = s.configuredDefault
	}
	if err := s.runners.SetDefault(name); err != nil {
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
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for k := range names {
			if !yield(k) {
				return
			}
		}
	}) {
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
