package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func loadTest(t *testing.T, data string, args ...string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(append([]string{"--config", path}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestPrecedence(t *testing.T) {
	data := `{"listen":"127.0.0.1:8001","codexModels":["file-model"]}`
	if got := loadTest(t, data).ListenAddr; got != "127.0.0.1:8001" {
		t.Fatal(got)
	}
	t.Setenv("MOBD_LISTEN", "127.0.0.1:8002")
	t.Setenv("MOBD_MODELS", " env-model, second ")
	t.Setenv("MOBD_SHARED_SECRET", "")
	cfg := loadTest(t, data)
	if cfg.ListenAddr != "127.0.0.1:8002" || cfg.SharedSecret != "" || !reflect.DeepEqual(cfg.CodexModels, []string{"env-model", "second"}) {
		t.Fatalf("precedence: %+v", cfg)
	}
	if got := loadTest(t, data, "--listen", "127.0.0.1:8003").ListenAddr; got != "127.0.0.1:8003" {
		t.Fatal(got)
	}
	if got := loadTest(t, `{"streamPort":0}`).StreamPort; got != 0 {
		t.Fatal(got)
	}
	t.Setenv("MOBD_STREAM_PORT", "9600")
	if got := loadTest(t, `{"streamPort":0}`).StreamPort; got != 9600 {
		t.Fatal(got)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, data := range []string{`{`, `null`, `{} {}`, `{"typo":true}`, `{"sharedSecret":"legacy"}`, `{"maxRunning":0}`, `{"listen":":0"}`, `{"listen":"bad"}`, `{"streamPort":-1}`, `{"streamPort":65536}`, `{"mcpArgs":"bad"}`, `{"workdir":""}`, `{"questionTimeout":"0s"}`, `{"questionTimeout":123}`, `{"codexMode":"invalid"}`, `{"codexPermissions":"typo"}`, `{"claudePermissions":"typo"}`, `{"runners":[]}`, `{"runners":["unknown"]}`, `{"runners":["codex","codex"]}`, `{"runners":["claude"],"defaultRunner":"codex"}`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load([]string{"--config", path}); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if _, _, err := Load([]string{"--config", filepath.Join(t.TempDir(), "missing.json")}); err == nil {
		t.Fatal("missing explicit config accepted")
	}
	for key, value := range map[string]string{"MOBD_MAX_RUNNING": "oops", "MOBD_STREAM_PORT": "udp", "MOBD_MOB_MCP_ARGS": "not-json", "MOBD_QUESTION_TIMEOUT": "soon", "MOBD_SHARED_SECRET": "legacy"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			path := filepath.Join(t.TempDir(), "config.json")
			os.WriteFile(path, []byte(`{}`), 0600)
			if _, _, err := Load([]string{"--config", path}); err == nil {
				t.Fatal("invalid env accepted")
			}
		})
	}
}

func TestRunnerConfigurationAndServiceSnapshot(t *testing.T) {
	cfg := loadTest(t, `{
		"runners":["claude"], "defaultRunner":"claude",
		"codexMode":"exec", "codexPermissions":"approve", "codexModels":["file-model"],
		"claudeBin":"/opt/claude", "claudeWorkdir":"/tmp/claude", "claudeModels":["sonnet"],
		"claudePermissions":"approve", "claudeSettingSources":"all", "questionTimeout":"15m"
	}`)
	if cfg.DefaultRunner != "claude" || !reflect.DeepEqual(cfg.Runners, []string{"claude"}) ||
		cfg.CodexMode != "exec" || cfg.CodexPermissions != "approve" ||
		cfg.ClaudeBin != "/opt/claude" || cfg.ClaudeWorkDir != "/tmp/claude" ||
		cfg.ClaudePermissions != "approve" || cfg.ClaudeSettingSources != "all" ||
		!reflect.DeepEqual(cfg.ClaudeModels, []string{"sonnet"}) || cfg.QuestionTimeout != 15*time.Minute {
		t.Fatalf("runner JSON: %+v", cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadTest(t, string(data)); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("service snapshot changed configuration: %+v", got)
	}
	for key, value := range map[string]string{
		"MOBD_RUNNERS": "codex,claude", "MOBD_DEFAULT_RUNNER": "codex",
		"MOBD_CODEX_MODE": "app-server", "MOBD_CODEX_PERMISSIONS": "bypass",
		"MOBD_MODELS": "legacy-model", "MOBD_CODEX_MODELS": "new-model",
		"MOBD_CLAUDE_BIN": "/other/claude", "MOBD_CLAUDE_WORKDIR": "/tmp/other",
		"MOBD_CLAUDE_MODELS": "opus,haiku", "MOBD_CLAUDE_PERMISSIONS": "bypass",
		"MOBD_CLAUDE_SETTING_SOURCES": "project", "MOBD_QUESTION_TIMEOUT": "2m",
	} {
		t.Setenv(key, value)
	}
	got := loadTest(t, string(data))
	if got.DefaultRunner != "codex" || len(got.Runners) != 2 || got.CodexMode != "app-server" ||
		got.CodexPermissions != "bypass" || !reflect.DeepEqual(got.CodexModels, []string{"new-model"}) ||
		got.ClaudeBin != "/other/claude" || got.ClaudeWorkDir != "/tmp/other" ||
		!reflect.DeepEqual(got.ClaudeModels, []string{"opus", "haiku"}) ||
		got.ClaudePermissions != "bypass" || got.ClaudeSettingSources != "project" || got.QuestionTimeout != 2*time.Minute {
		t.Fatalf("runner environment: %+v", got)
	}
}

func TestPiConfiguration(t *testing.T) {
	cfg := loadTest(t, `{"runners":["pi"],"defaultRunner":"pi","piBin":"/opt/pi","piModels":["provider/model"],"piPermissions":"approve"}`)
	if cfg.DefaultRunner != "pi" || cfg.PiBin != "/opt/pi" || cfg.PiPermissions != "approve" || !reflect.DeepEqual(cfg.PiModels, []string{"provider/model"}) {
		t.Fatalf("Pi config: %+v", cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadTest(t, string(data)); !reflect.DeepEqual(got, cfg) {
		t.Fatal("Pi service snapshot changed")
	}
	t.Setenv("MOBD_PI_BIN", "/other/pi")
	t.Setenv("MOBD_PI_MODELS", "local/one, local/two")
	t.Setenv("MOBD_PI_PERMISSIONS", "bypass")
	got := loadTest(t, string(data))
	if got.PiBin != "/other/pi" || got.PiPermissions != "bypass" || !reflect.DeepEqual(got.PiModels, []string{"local/one", "local/two"}) {
		t.Fatalf("Pi env: %+v", got)
	}
}

func TestInvalidPiConfiguration(t *testing.T) {
	for _, data := range []string{`{"piPermissions":"manual"}`, `{"runners":["pi"],"defaultRunner":"pi","piBin":""}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, []byte(data), 0600)
		if _, _, err := Load([]string{"--config", path}); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestDefaultsAndNoDotEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MOBD_LISTEN=invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, path, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, ".mobd", "config.json") || cfg.ListenAddr != ":8080" || cfg.StreamPort != 9527 ||
		cfg.CodexMode != "app-server" || cfg.DefaultRunner != "codex" || len(cfg.Runners) != 2 ||
		cfg.ClaudeWorkDir != cfg.CodexWorkDir || cfg.QuestionTimeout != 30*time.Minute || len(cfg.CORSOrigins) != 4 {
		t.Fatalf("defaults: %+v; path=%s", cfg, path)
	}
	t.Setenv("MOBD_CORS_ORIGINS", " https://example.com, , http://localhost:3000 ")
	if got := loadTest(t, `{}`).CORSOrigins; !reflect.DeepEqual(got, []string{"https://example.com", "http://localhost:3000"}) {
		t.Fatal(got)
	}
}

func TestStateAndMCP(t *testing.T) {
	cfg := loadTest(t, `{}`)
	// macOS and Windows keep the user config directory, where existing history lives;
	// Linux keeps data under XDG_DATA_HOME.
	dir, err := dataHome()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StatePath != filepath.Join(dir, "state.json") {
		t.Fatal("history path changed")
	}
	if runtime.GOOS != "linux" {
		if config, _ := os.UserConfigDir(); dir != filepath.Join(config, "mob") {
			t.Fatal("existing history path changed")
		}
	}
	if !cfg.UsesBuiltinMCP() {
		t.Fatal("missing builtin MCP")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("MOBD_STATE_PATH", path)
	t.Setenv("MOBD_MOB_MCP_COMMAND", "/opt/mcp")
	t.Setenv("MOBD_MOB_MCP_ARGS", `["--flag"]`)
	cfg = loadTest(t, `{}`)
	if cfg.StatePath != path || cfg.DataDir != filepath.Dir(path) || cfg.MobMCPCommand != "/opt/mcp" || !reflect.DeepEqual(cfg.MobMCPArgs, []string{"--flag"}) {
		t.Fatalf("config: %+v", cfg)
	}
	t.Setenv("MOBD_MOB_MCP_COMMAND", "")
	if !loadTest(t, `{}`).UsesBuiltinMCP() {
		t.Fatal("empty command must restore builtin MCP")
	}
}

func TestInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mobd", "config.json")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions: %s %v", path, err)
		}
	}
	if _, _, err := Load([]string{"--config", path}); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); !os.IsExist(err) {
		t.Fatalf("must not overwrite config: %v", err)
	}
}

// Configs written before the VNC relay was removed still load; the keys are ignored.
func TestRemovedVNCKeysStillLoad(t *testing.T) {
	cfg := loadTest(t, `{"listen":"0.0.0.0:8080","vncAddr":"127.0.0.1:5900","vncViewUrl":"vnc://office"}`)
	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Fatalf("listen %q", cfg.ListenAddr)
	}
}
