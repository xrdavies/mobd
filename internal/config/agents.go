package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// AgentConfig is one agent's section in config.json ("agents": {name: {...}}), docs/agent-adapters.md.
// Native agents (Claude Code, Codex, Pi) take only the keys in nativeKeys; agents spoken to over the
// Agent Client Protocol take protocol "acp" with a program, its arguments and environment.
type AgentConfig struct {
	Protocol string            `json:"protocol,omitempty"`
	Label    string            `json:"label,omitempty"`
	Bin      string            `json:"bin,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	// ApproveEnv is added to Env when the agent's permissions are "approve" (settings.json): ACP has
	// no approval switch, so this is what makes an agent ask before acting.
	ApproveEnv map[string]string `json:"approveEnv,omitempty"`
	Install    string            `json:"install,omitempty"`
	// Claude Code only: which of its settings it loads (claudeSettingSources).
	SettingSources string `json:"settingSources,omitempty"`
	// Codex only: app-server or exec (codexMode).
	Mode string `json:"mode,omitempty"`
}

// ProtocolACP marks an agent run by the generic Agent Client Protocol adapter.
const ProtocolACP = "acp"

// NativeAgents have mobd's own adapters.
var NativeAgents = []string{"codex", "claude", "pi"}

// Legacy maps a native agent's section key to the flat config.json key it replaces; both may not
// be set at once. The flat keys keep working (D3).
var Legacy = map[string]string{
	"claude.bin": "claudeBin", "claude.workdir": "claudeWorkdir", "claude.settingSources": "claudeSettingSources",
	"codex.bin": "codexBin", "codex.mode": "codexMode",
	"pi.bin": "piBin",
}

// ACPPresets are ACP agents mobd knows: turning one on needs no section. A section overrides fields.
var ACPPresets = map[string]AgentConfig{
	"opencode": {Protocol: ProtocolACP, Label: "OpenCode", Bin: "opencode", Args: []string{"acp"},
		Install:    "npm install -g opencode-ai",
		ApproveEnv: map[string]string{"OPENCODE_PERMISSION": `{"bash":"ask","edit":"ask","webfetch":"ask","external_directory":"ask"}`}},
	"dsh": {Protocol: ProtocolACP, Label: "DeepSeek Harness", Bin: "dsh", Args: []string{"--profile", "acp"},
		Install: "npm install -g @deepseek-ai/dsh"},
}

var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// nativeSections moves native agents' sections onto their flat fields (the adapters read those) and
// removes them from Agents, so a service snapshot written from the config loads the same way.
// raw is config.json's top-level keys, to refuse a value set both ways.
func nativeSections(cfg *Config, raw map[string]json.RawMessage) error {
	for name, a := range cfg.Agents {
		if !slices.Contains(NativeAgents, name) {
			continue
		}
		if a.Protocol != "" && a.Protocol != "native" {
			return fmt.Errorf("agents.%s: %s has mobd's own adapter; remove protocol", name, name)
		}
		fields := map[string]*string{}
		var bad []string
		set := func(key, value string, dest *string) {
			if value == "" {
				return
			}
			if dest == nil {
				bad = append(bad, key)
				return
			}
			fields[key] = dest
			*dest = value
		}
		switch name {
		case "claude":
			set("bin", a.Bin, &cfg.ClaudeBin)
			set("workdir", a.Workdir, &cfg.ClaudeWorkDir)
			set("settingSources", a.SettingSources, &cfg.ClaudeSettingSources)
			set("mode", a.Mode, nil)
		case "codex":
			set("bin", a.Bin, &cfg.CodexBin)
			set("mode", a.Mode, &cfg.CodexMode)
			set("workdir", a.Workdir, nil)
			set("settingSources", a.SettingSources, nil)
		case "pi":
			set("bin", a.Bin, &cfg.PiBin)
			set("workdir", a.Workdir, nil)
			set("mode", a.Mode, nil)
			set("settingSources", a.SettingSources, nil)
		}
		if a.Label != "" || len(a.Args) > 0 || len(a.Env) > 0 || len(a.ApproveEnv) > 0 || a.Install != "" {
			bad = append(bad, "label/args/env/approveEnv/install")
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			return fmt.Errorf("agents.%s: %s does not take %s (codex and pi use the shared workdir)", name, name, strings.Join(bad, ", "))
		}
		for key := range fields {
			if flat := Legacy[name+"."+key]; raw[flat] != nil {
				return fmt.Errorf("agents.%s.%s and %s are both set; keep one", name, key, flat)
			}
		}
		delete(cfg.Agents, name)
	}
	return nil
}

// acpAgents completes the ACP agents: presets for enabled ones without a section, defaults, checks.
func acpAgents(cfg *Config) error {
	if cfg.Agents == nil {
		cfg.Agents = map[string]AgentConfig{}
	}
	for _, name := range cfg.Runners {
		if _, ok := cfg.Agents[name]; !ok {
			if p, ok := ACPPresets[name]; ok {
				cfg.Agents[name] = p
			}
		}
	}
	for name, a := range cfg.Agents {
		if !agentName.MatchString(name) {
			return fmt.Errorf("agents.%s: names use lowercase letters, digits and -", name)
		}
		preset, known := ACPPresets[name]
		if a.Protocol == "" && known {
			a.Protocol = ProtocolACP
		}
		if a.Protocol != ProtocolACP {
			return fmt.Errorf("agents.%s: unknown agent; set \"protocol\": \"acp\" for an Agent Client Protocol agent", name)
		}
		if known {
			if a.Label == "" {
				a.Label = preset.Label
			}
			if a.Bin == "" {
				a.Bin = preset.Bin
			}
			if a.Args == nil {
				a.Args = preset.Args
			}
			if a.Install == "" {
				a.Install = preset.Install
			}
			if a.ApproveEnv == nil {
				a.ApproveEnv = preset.ApproveEnv
			}
		}
		if a.Label == "" {
			a.Label = name
		}
		if a.SettingSources != "" || a.Mode != "" {
			return fmt.Errorf("agents.%s: settingSources and mode are for claude and codex", name)
		}
		if strings.TrimSpace(a.Bin) == "" {
			return fmt.Errorf("agents.%s.bin must name the agent's program", name)
		}
		if a.Workdir == "" {
			a.Workdir = cfg.CodexWorkDir
		}
		wd, err := filepath.Abs(a.Workdir)
		if err != nil {
			return err
		}
		a.Workdir = wd
		cfg.Agents[name] = a
	}
	return nil
}

// ACP returns an enabled-or-configured ACP agent's section.
func (c Config) ACP(name string) (AgentConfig, bool) {
	a, ok := c.Agents[name]
	return a, ok && a.Protocol == ProtocolACP
}

// AllACP are the configured ACP agents plus the presets not configured, completed with defaults, so
// the apps can list agents that are not turned on yet.
func (c Config) AllACP() map[string]AgentConfig {
	out := map[string]AgentConfig{}
	for n, a := range c.Agents {
		if a.Protocol == ProtocolACP {
			out[n] = a
		}
	}
	for n, p := range ACPPresets {
		if _, ok := out[n]; !ok {
			p.Workdir = c.CodexWorkDir
			out[n] = p
		}
	}
	return out
}

// KnownAgents are the agents runners may name: the native ones, the ACP presets and configured ACP agents.
func (c Config) KnownAgents() []string {
	names := append([]string{}, NativeAgents...)
	for n := range ACPPresets {
		names = append(names, n)
	}
	for n, a := range c.Agents {
		if a.Protocol == ProtocolACP && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}
