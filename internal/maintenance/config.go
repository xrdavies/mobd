// Package maintenance changes mobd's startup config and restarts it under control, from the apps or
// the CLI (docs/remote-maintenance.md): only some keys are editable remotely, every change is
// validated before it is written, and a restart that does not come up falls back to the last config
// that worked.
package maintenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/xrdavies/mobd/internal/config"
)

// Key is a config.json key the apps may change, with the environment variable and flag that
// override it (then it is read-only there).
type Key struct {
	JSON, Env, Flag string
}

// Editable are the keys paired clients may change (D3). The others can cut off the client making
// the change (listen, CORS, relay), move data or credentials, or run a program for every turn
// (mcpCommand), so they stay local.
var Editable = []Key{
	{"runners", "MOBD_RUNNERS", ""},
	{"codexBin", "MOBD_CODEX_BIN", "codex-bin"},
	{"claudeBin", "MOBD_CLAUDE_BIN", ""},
	{"piBin", "MOBD_PI_BIN", ""},
	{"workdir", "MOBD_CODEX_WORKDIR", "workdir"},
	{"claudeWorkdir", "MOBD_CLAUDE_WORKDIR", ""},
	{"claudeSettingSources", "MOBD_CLAUDE_SETTING_SOURCES", ""},
	{"codexMode", "MOBD_CODEX_MODE", ""},
	{"maxRunning", "MOBD_MAX_RUNNING", "max-running"},
	{"questionTimeout", "MOBD_QUESTION_TIMEOUT", ""},
	{"filesDir", "MOBD_FILES_DIR", ""},
}

func editable(name string) bool {
	return slices.ContainsFunc(Editable, func(k Key) bool { return k.JSON == name })
}

var (
	// ErrStale: config.json changed since the client read it.
	ErrStale = errors.New("config changed since it was read; reload it")
)

// NotEditableError names keys that cannot be changed remotely.
type NotEditableError struct{ Keys []string }

func (e *NotEditableError) Error() string {
	return "not editable from the apps: " + strings.Join(e.Keys, ", ")
}

// InvalidError is a change that fails mobd's startup checks; nothing was written.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

// ConfigPath is the config.json the args select (the default one without --config).
func ConfigPath(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		name, value, eq := strings.Cut(args[i], "=")
		if name != "--config" && name != "-config" {
			continue
		}
		if !eq {
			if i+1 >= len(args) {
				return "", errors.New("missing --config value")
			}
			value = args[i+1]
		}
		return filepath.Abs(value)
	}
	return config.DefaultPath()
}

// withConfig replaces or adds --config in args.
func withConfig(args []string, path string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		name, _, eq := strings.Cut(args[i], "=")
		if name == "--config" || name == "-config" {
			if !eq {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return append(out, "--config", path)
}

// Rev identifies a config file's content; "" for a missing file.
func Rev(data []byte) string {
	if data == nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func readRaw(path string) (map[string]json.RawMessage, []byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	raw := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, data, fmt.Errorf("%s is not a JSON object: %w", filepath.Base(path), err)
		}
	}
	return raw, data, nil
}

// File is the editable part of config.json as written (keys the file does not set are absent).
type File struct {
	Rev    string                     `json:"rev"`
	Values map[string]json.RawMessage `json:"values"`
}

// Read returns the editable keys config.json sets.
func Read(args []string) (File, error) {
	path, err := ConfigPath(args)
	if err != nil {
		return File{}, err
	}
	raw, data, err := readRaw(path)
	if err != nil {
		return File{}, err
	}
	f := File{Rev: Rev(data), Values: map[string]json.RawMessage{}}
	for k, v := range raw {
		if editable(k) {
			f.Values[k] = v
		}
	}
	return f, nil
}

// Effective are the editable keys' values in a loaded config (after defaults, environment and flags).
// Programs are resolved like `mobd start` does, so a file naming "codex" and a running service
// started with its full path compare equal.
func Effective(cfg config.Config) (map[string]any, error) {
	for _, bin := range []*string{&cfg.CodexBin, &cfg.ClaudeBin, &cfg.PiBin} {
		if found, err := exec.LookPath(*bin); err == nil {
			if abs, err := filepath.Abs(found); err == nil {
				*bin = abs
			}
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var all map[string]any
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range Editable {
		out[k.JSON] = all[k.JSON]
	}
	return out, nil
}

// Overridden are the editable keys set by an environment variable or a flag in args, which win
// over config.json.
func Overridden(args []string) []string {
	var out []string
	for _, k := range Editable {
		_, env := os.LookupEnv(k.Env)
		flag := k.Flag != "" && slices.ContainsFunc(args, func(a string) bool {
			name, _, _ := strings.Cut(a, "=")
			return name == "--"+k.Flag || name == "-"+k.Flag
		})
		if env || flag {
			out = append(out, k.JSON)
		}
	}
	return out
}

// Check loads a config like startup does, then checks what startup leaves to the first turn: the
// working folders exist and every enabled agent's program is found.
func Check(args []string) (config.Config, error) {
	cfg, _, err := config.Load(args)
	if err != nil {
		return cfg, err
	}
	for name, dir := range map[string]string{"workdir": cfg.CodexWorkDir, "claudeWorkdir": cfg.ClaudeWorkDir} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return cfg, fmt.Errorf("%s %s is not a folder", name, dir)
		}
	}
	bins := map[string]string{"codex": cfg.CodexBin, "claude": cfg.ClaudeBin, "pi": cfg.PiBin}
	for _, name := range cfg.Runners {
		if _, err := exec.LookPath(bins[name]); err != nil {
			return cfg, fmt.Errorf("%s is enabled but its program %q is not found", name, bins[name])
		}
	}
	return cfg, nil
}

// Diff is one changed key.
type Diff struct {
	Key  string          `json:"key"`
	From json.RawMessage `json:"from,omitempty"`
	To   json.RawMessage `json:"to,omitempty"`
}

// Change applies changes (a null value removes the key, back to its default) to config.json read at
// rev. It writes only after the result passes Check; unrelated keys are kept.
func Change(args []string, rev string, changes map[string]json.RawMessage) (string, []Diff, error) {
	var bad []string
	for k := range changes {
		if !editable(k) {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return "", nil, &NotEditableError{Keys: bad}
	}
	path, err := ConfigPath(args)
	if err != nil {
		return "", nil, err
	}
	raw, data, err := readRaw(path)
	if err != nil {
		return "", nil, err
	}
	if Rev(data) != rev {
		return "", nil, ErrStale
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var diff []Diff
	for _, k := range keys {
		v := changes[k]
		old := raw[k]
		if string(bytes.TrimSpace(v)) == "null" {
			if old == nil {
				continue
			}
			delete(raw, k)
			diff = append(diff, Diff{Key: k, From: old})
			continue
		}
		if bytes.Equal(compact(old), compact(v)) {
			continue
		}
		raw[k] = v
		diff = append(diff, Diff{Key: k, From: old, To: v})
	}
	if len(diff) == 0 {
		return rev, nil, nil
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return "", nil, err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", nil, err
	}
	candidate := path + ".candidate"
	if err := os.WriteFile(candidate, out, 0o600); err != nil {
		return "", nil, err
	}
	if _, err := Check(withConfig(args, candidate)); err != nil {
		os.Remove(candidate)
		return "", nil, &InvalidError{Reason: strings.ReplaceAll(err.Error(), candidate, filepath.Base(path))}
	}
	if err := os.Rename(candidate, path); err != nil {
		os.Remove(candidate)
		return "", nil, err
	}
	return Rev(out), diff, nil
}

func compact(v json.RawMessage) []byte {
	if v == nil {
		return nil
	}
	var b bytes.Buffer
	if json.Compact(&b, v) != nil {
		return v
	}
	return b.Bytes()
}
