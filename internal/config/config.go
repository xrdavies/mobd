package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is shared by foreground and launchd-managed servers.
type Config struct {
	ControlSocket string `json:"controlSocket"`
	ListenAddr    string `json:"listen"`
	// StreamPort carries every live desktop session's WebRTC media on one UDP port, so a
	// firewall or tailnet rule can name it; 0 picks a random port per session.
	StreamPort int `json:"streamPort"`
	// StreamUDP is the socket bound to StreamPort by serve.
	StreamUDP     net.PacketConn `json:"-"`
	CodexBin      string         `json:"codexBin"`
	CodexWorkDir  string         `json:"workdir"`
	MobMCPCommand string         `json:"mcpCommand"`
	MobMCPArgs    []string       `json:"mcpArgs"`
	SharedSecret  string         `json:"sharedSecret,omitempty"`
	// Removed with the VNC relay (the live desktop is /stream now); accepted so older configs load.
	RemovedVNCViewURL    string        `json:"vncViewUrl,omitempty"`
	StatePath            string        `json:"statePath"`
	CORSOrigins          []string      `json:"corsOrigins"`
	MaxRunning           int           `json:"maxRunning"`
	DataDir              string        `json:"dataDir"`
	CodexModels          []string      `json:"codexModels"`
	CodexMode            string        `json:"codexMode"`
	CodexPermissions     string        `json:"codexPermissions"`
	Runners              []string      `json:"runners"`
	DefaultRunner        string        `json:"defaultRunner"`
	ClaudeBin            string        `json:"claudeBin"`
	ClaudeWorkDir        string        `json:"claudeWorkdir"`
	ClaudeModels         []string      `json:"claudeModels"`
	ClaudePermissions    string        `json:"claudePermissions"`
	ClaudeSettingSources string        `json:"claudeSettingSources"`
	PiBin                string        `json:"piBin"`
	PiModels             []string      `json:"piModels"`
	PiPermissions        string        `json:"piPermissions"`
	QuestionTimeout      time.Duration `json:"-"`
	RemovedVNCAddr       string        `json:"vncAddr,omitempty"`

	// Desktops (desktop.go). Linux only, except desktopMode; inert until the Linux desktop manager.
	DesktopMode           string        `json:"desktopMode"`
	DesktopSleepAfter     time.Duration `json:"-"`
	DesktopSleepCPU       float64       `json:"desktopSleepCPU"`
	DesktopSize           string        `json:"desktopSize"`
	DesktopScale          int           `json:"desktopScale"`
	DesktopKeyboardLayout string        `json:"desktopKeyboardLayout"`
	DesktopDisplayBase    int           `json:"desktopDisplayBase"`
	MaxDesktops           int           `json:"maxDesktops"`
	MemoryWarnPercent     int           `json:"memoryWarnPercent"`
	MemoryFloorPercent    int           `json:"memoryFloorPercent"`
	DiskWarnPercent       int           `json:"diskWarnPercent"`
	DiskWarnBytes         Bytes         `json:"diskWarnBytes"`
	DiskFloorBytes        Bytes         `json:"diskFloorBytes"`
	DesktopBrowserSeed    string        `json:"desktopBrowserSeed,omitempty"`
}

// JSON uses Go duration strings so user configs and service snapshots share a format.
func (c Config) MarshalJSON() ([]byte, error) {
	type plain Config
	return json.Marshal(struct {
		plain
		QuestionTimeout   string `json:"questionTimeout"`
		DesktopSleepAfter string `json:"desktopSleepAfter"`
	}{plain(c), c.QuestionTimeout.String(), c.DesktopSleepAfter.String()})
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	value := struct {
		*plain
		QuestionTimeout   *string `json:"questionTimeout"`
		DesktopSleepAfter *string `json:"desktopSleepAfter"`
	}{plain: (*plain)(c)}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return err
	}
	if value.QuestionTimeout != nil {
		duration, err := time.ParseDuration(*value.QuestionTimeout)
		if err != nil {
			return fmt.Errorf("questionTimeout: %w", err)
		}
		c.QuestionTimeout = duration
	}
	if value.DesktopSleepAfter != nil {
		duration, err := time.ParseDuration(*value.DesktopSleepAfter)
		if err != nil {
			return fmt.Errorf("desktopSleepAfter: %w", err)
		}
		c.DesktopSleepAfter = duration
	}
	return nil
}

func DefaultPath() (string, error) {
	dir, err := Dir()
	return filepath.Join(dir, "config.json"), err
}

// Load applies defaults, JSON, exported environment, then command-line flags.
// An absent default config is fine; an explicitly selected file must exist.
func Load(args []string) (Config, string, error) {
	var cli Config
	var path string
	fs := flag.NewFlagSet("mobd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&path, "config", "", "JSON config path")
	fs.StringVar(&cli.ControlSocket, "control-socket", "", "Local management socket")
	fs.StringVar(&cli.ListenAddr, "listen", "", "HTTP listen address")
	fs.StringVar(&cli.CodexBin, "codex-bin", "", "Codex executable")
	fs.StringVar(&cli.CodexWorkDir, "workdir", "", "Codex working directory")
	fs.StringVar(&cli.StatePath, "state-path", "", "Conversation snapshot")
	fs.IntVar(&cli.MaxRunning, "max-running", 0, "Concurrent tasks")
	if err := fs.Parse(args); err != nil {
		return Config{}, "", err
	}
	if fs.NArg() != 0 {
		return Config{}, "", fmt.Errorf("unexpected argument: %s", fs.Arg(0))
	}
	explicit := path != ""
	var err error
	if !explicit {
		path, err = DefaultPath()
	}
	if err != nil {
		return Config{}, "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Config{}, "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return Config{}, path, err
	}
	stateDir, err := dataHome()
	if err != nil {
		return Config{}, path, err
	}
	cfg := Config{
		ListenAddr: ":8080", StreamPort: 9527, CodexBin: "codex", CodexWorkDir: cwd,
		// Keep existing history visible when upgrading from environment-only releases.
		StatePath:   filepath.Join(stateDir, "state.json"),
		CORSOrigins: []string{"http://localhost:1420", "http://127.0.0.1:1420", "tauri://localhost", "http://tauri.localhost"},
		MaxRunning:  maxRunningUnset,
		Runners:     []string{"codex", "claude"}, DefaultRunner: "codex",
		CodexMode: "app-server", CodexPermissions: "bypass",
		ClaudeBin: "claude", ClaudeModels: []string{"opus", "sonnet", "haiku"},
		ClaudePermissions: "bypass", ClaudeSettingSources: "project",
		PiBin: "pi", PiPermissions: "bypass",
		QuestionTimeout: 30 * time.Minute,
	}
	defaultDesktop(&cfg)
	data, err := os.ReadFile(path)
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return cfg, path, errors.New("config must be a JSON object")
		}
		if err = dec.Decode(&cfg); err != nil {
			return cfg, path, fmt.Errorf("config %s: %w", path, err)
		}
		if err = dec.Decode(new(any)); err != io.EOF {
			return cfg, path, fmt.Errorf("config %s: unexpected trailing data", path)
		}
	} else if explicit || !os.IsNotExist(err) {
		return cfg, path, fmt.Errorf("config %s: %w", path, err)
	}
	for key, dest := range map[string]*string{
		"MOBD_CONTROL_SOCKET": &cfg.ControlSocket,
		"MOBD_LISTEN":         &cfg.ListenAddr, "MOBD_CODEX_BIN": &cfg.CodexBin,
		"MOBD_CODEX_WORKDIR": &cfg.CodexWorkDir, "MOBD_MOB_MCP_COMMAND": &cfg.MobMCPCommand,
		"MOBD_SHARED_SECRET": &cfg.SharedSecret,
		"MOBD_STATE_PATH":    &cfg.StatePath, "MOBD_DATA_DIR": &cfg.DataDir,
		"MOBD_CODEX_MODE": &cfg.CodexMode, "MOBD_CODEX_PERMISSIONS": &cfg.CodexPermissions,
		"MOBD_DEFAULT_RUNNER": &cfg.DefaultRunner, "MOBD_CLAUDE_BIN": &cfg.ClaudeBin,
		"MOBD_CLAUDE_WORKDIR": &cfg.ClaudeWorkDir, "MOBD_CLAUDE_PERMISSIONS": &cfg.ClaudePermissions,
		"MOBD_CLAUDE_SETTING_SOURCES": &cfg.ClaudeSettingSources,
		"MOBD_PI_BIN":                 &cfg.PiBin, "MOBD_PI_PERMISSIONS": &cfg.PiPermissions,
	} {
		if value, ok := os.LookupEnv(key); ok {
			*dest = value
		}
	}
	// The legacy name remains an environment-only alias; the new name wins.
	if value, ok := os.LookupEnv("MOBD_MODELS"); ok {
		cfg.CodexModels = splitList(value)
	}
	for key, dest := range map[string]*[]string{
		"MOBD_CORS_ORIGINS": &cfg.CORSOrigins, "MOBD_CODEX_MODELS": &cfg.CodexModels,
		"MOBD_RUNNERS": &cfg.Runners, "MOBD_CLAUDE_MODELS": &cfg.ClaudeModels,
		"MOBD_PI_MODELS": &cfg.PiModels,
	} {
		if value, ok := os.LookupEnv(key); ok {
			*dest = splitList(value)
		}
	}
	if value, ok := os.LookupEnv("MOBD_MOB_MCP_ARGS"); ok {
		cfg.MobMCPArgs = nil
		if strings.TrimSpace(value) != "" {
			if err := json.Unmarshal([]byte(value), &cfg.MobMCPArgs); err != nil {
				return cfg, path, fmt.Errorf("MOBD_MOB_MCP_ARGS must be a JSON string array: %w", err)
			}
		}
	}
	if err := desktopEnv(&cfg); err != nil {
		return cfg, path, err
	}
	if value, ok := os.LookupEnv("MOBD_STREAM_PORT"); ok {
		cfg.StreamPort, err = strconv.Atoi(value)
		if err != nil {
			return cfg, path, fmt.Errorf("MOBD_STREAM_PORT: %w", err)
		}
	}
	if value, ok := os.LookupEnv("MOBD_MAX_RUNNING"); ok {
		cfg.MaxRunning, err = strconv.Atoi(value)
		if err != nil {
			return cfg, path, fmt.Errorf("MOBD_MAX_RUNNING: %w", err)
		}
	}
	if value, ok := os.LookupEnv("MOBD_QUESTION_TIMEOUT"); ok {
		cfg.QuestionTimeout, err = time.ParseDuration(value)
		if err != nil {
			return cfg, path, fmt.Errorf("MOBD_QUESTION_TIMEOUT: %w", err)
		}
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "control-socket":
			cfg.ControlSocket = cli.ControlSocket
		case "listen":
			cfg.ListenAddr = cli.ListenAddr
		case "codex-bin":
			cfg.CodexBin = cli.CodexBin
		case "workdir":
			cfg.CodexWorkDir = cli.CodexWorkDir
		case "state-path":
			cfg.StatePath = cli.StatePath
		case "max-running":
			cfg.MaxRunning = cli.MaxRunning
		}
	})
	if cfg.SharedSecret != "" {
		return cfg, path, errors.New("sharedSecret/MOBD_SHARED_SECRET is no longer supported; remove it and pair clients with mobd link (history is preserved)")
	}
	if cfg.ControlSocket == "" {
		cfg.ControlSocket, err = ControlSocketDefault()
		if err != nil {
			return cfg, path, err
		}
	}
	cfg.ControlSocket, err = filepath.Abs(cfg.ControlSocket)
	if err != nil {
		return cfg, path, err
	}
	if cfg.MaxRunning == maxRunningUnset {
		cfg.MaxRunning = defaultMaxRunning(cfg)
	}
	if cfg.MaxRunning < 1 {
		return cfg, path, errors.New("maxRunning must be positive")
	}
	if cfg.QuestionTimeout <= 0 {
		return cfg, path, errors.New("questionTimeout must be positive")
	}
	if err := validateDesktop(&cfg); err != nil {
		return cfg, path, err
	}
	if cfg.CodexMode != "app-server" && cfg.CodexMode != "exec" {
		return cfg, path, errors.New("codexMode must be app-server or exec")
	}
	for name, value := range map[string]string{"codexPermissions": cfg.CodexPermissions, "claudePermissions": cfg.ClaudePermissions, "piPermissions": cfg.PiPermissions} {
		if value != "bypass" && value != "approve" {
			return cfg, path, fmt.Errorf("%s must be bypass or approve", name)
		}
	}
	// codex exec never asks before a command or edit; only app-server can enforce approve.
	if cfg.CodexMode == "exec" && cfg.CodexPermissions == "approve" {
		return cfg, path, errors.New("codexPermissions approve needs codexMode app-server (codex exec cannot ask before running commands)")
	}
	enabled := make(map[string]bool)
	for _, name := range cfg.Runners {
		if name != "codex" && name != "claude" && name != "pi" {
			return cfg, path, fmt.Errorf("unknown runner %q (known: codex, claude, pi)", name)
		}
		if enabled[name] {
			return cfg, path, fmt.Errorf("duplicate runner %q", name)
		}
		enabled[name] = true
	}
	if !enabled[cfg.DefaultRunner] {
		return cfg, path, errors.New("defaultRunner must be an enabled runner")
	}
	if enabled["pi"] && cfg.PiBin == "" {
		return cfg, path, errors.New("piBin must not be empty when pi is enabled")
	}
	for name, addr := range map[string]string{"listen": cfg.ListenAddr} {
		_, port, err := net.SplitHostPort(addr)
		n, parseErr := strconv.Atoi(port)
		if err != nil || parseErr != nil || n < 1 || n > 65535 {
			return cfg, path, fmt.Errorf("%s must be host:port with port 1–65535", name)
		}
	}
	if cfg.StreamPort < 0 || cfg.StreamPort > 65535 {
		return cfg, path, errors.New("streamPort must be 0–65535 (0: a random port per session)")
	}
	if cfg.CodexBin == "" || cfg.CodexWorkDir == "" || cfg.StatePath == "" || cfg.ClaudeBin == "" {
		return cfg, path, errors.New("codexBin, claudeBin, workdir and statePath must not be empty")
	}
	if cfg.ClaudeWorkDir == "" {
		cfg.ClaudeWorkDir = cfg.CodexWorkDir
	}
	for _, p := range []*string{&cfg.CodexWorkDir, &cfg.ClaudeWorkDir, &cfg.StatePath} {
		*p, err = filepath.Abs(*p)
		if err != nil {
			return cfg, path, err
		}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Dir(cfg.StatePath)
	}
	cfg.DataDir, err = filepath.Abs(cfg.DataDir)
	if err != nil {
		return cfg, path, err
	}
	if strings.TrimSpace(cfg.MobMCPCommand) == "" {
		cfg.MobMCPCommand, cfg.MobMCPArgs = BuiltinMCP()
	}
	return cfg, path, nil
}

// Init creates a private config without overwriting an existing file.
func Init(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	// maxRunning is left out so the per-OS default applies.
	_, writeErr := file.WriteString("{\n  \"listen\": \"0.0.0.0:8080\"\n}\n")
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func BuiltinMCP() (string, []string) {
	exe, err := os.Executable()
	if err != nil {
		exe = "mobd"
	} else if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, []string{"mcp"}
}

func (c Config) UsesBuiltinMCP() bool {
	exe, args := BuiltinMCP()
	return c.MobMCPCommand == exe && len(c.MobMCPArgs) == 1 && c.MobMCPArgs[0] == args[0]
}

func splitList(raw string) []string {
	var items []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// SelfURL is how local child processes reach the configured listener.
func (c Config) SelfURL() string {
	host, port, _ := net.SplitHostPort(c.ListenAddr)
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (c Config) PortHint() int {
	_, port, _ := net.SplitHostPort(c.ListenAddr)
	n, _ := strconv.Atoi(port)
	return n
}
