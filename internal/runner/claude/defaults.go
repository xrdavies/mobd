package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// defaults reads the model and effort Claude Code uses when a turn names none, from the
// settings files mobd lets it load (claudeSettingSources): user ~/.claude/settings.json, then
// project and local ones in the work directory, later ones winning; ANTHROPIC_MODEL wins over all.
func (r *Runner) defaults() (model, effort string) {
	sources := r.cfg.ClaudeSettingSources
	if sources == "" || sources == "all" {
		sources = "user,project,local"
	}
	var files []string
	for _, src := range strings.Split(sources, ",") {
		switch strings.TrimSpace(src) {
		case "user":
			if f := userSettings(); f != "" {
				files = append(files, f)
			}
		case "project":
			files = append(files, filepath.Join(r.cfg.ClaudeWorkDir, ".claude", "settings.json"))
		case "local":
			files = append(files, filepath.Join(r.cfg.ClaudeWorkDir, ".claude", "settings.local.json"))
		}
	}
	// Claude applies user, then project, then local, whatever order the flag lists them in.
	rank := func(f string) int {
		switch {
		case strings.HasSuffix(f, "settings.local.json"):
			return 2
		case strings.HasPrefix(f, r.cfg.ClaudeWorkDir+string(filepath.Separator)):
			return 1
		}
		return 0
	}
	for want := 0; want <= 2; want++ {
		for _, f := range files {
			if rank(f) != want {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var s struct {
				Model       string `json:"model"`
				EffortLevel string `json:"effortLevel"`
			}
			if json.Unmarshal(data, &s) != nil {
				continue
			}
			if s.Model != "" {
				model = s.Model
			}
			if s.EffortLevel != "" {
				effort = s.EffortLevel
			}
		}
	}
	// The personal env reaches Claude also without "user" (connectionSettings), and env beats model.
	if m := personalModel(); m != "" {
		model = m
	}
	if m := os.Getenv("ANTHROPIC_MODEL"); m != "" {
		model = m
	}
	return model, effort
}

// personalModel is env.ANTHROPIC_MODEL in the personal settings.
func personalModel() string {
	f := userSettings()
	if f == "" {
		return ""
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	var s struct {
		Env map[string]any `json:"env"`
	}
	if json.Unmarshal(data, &s) != nil {
		return ""
	}
	m, _ := s.Env["ANTHROPIC_MODEL"].(string)
	return m
}
