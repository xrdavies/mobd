package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/runner"
)

// Login settings (mobd#7). With claudeSettingSources leaving out "user" (the default "project"),
// Claude skips ~/.claude/settings.json, and with it a gateway token or apiKeyHelper set up there:
// turns fail with "Not logged in". mobd passes on only the settings that connect Claude to its
// API, in a private file named by --settings; hooks, plugins, permissions and the rest of the
// personal settings stay out of office runs. A file, not inline JSON: arguments are visible to
// every user in ps.

// connectionKeys are the user settings passed on: the environment (API address, token, model
// names, Bedrock/Vertex selection) and the commands that produce credentials.
var connectionKeys = []string{"env", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh"}

const loginHint = "run claude /login as the user that runs mobd, or set ANTHROPIC_API_KEY, " +
	"env.ANTHROPIC_AUTH_TOKEN or apiKeyHelper in ~/.claude/settings.json"

// userSettings is Claude's personal settings file (CLAUDE_CONFIG_DIR moves it).
func userSettings() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// loadsUser reports whether Claude reads the personal settings file itself.
func (r *Runner) loadsUser() bool {
	src := r.cfg.ClaudeSettingSources
	if src == "" || src == "all" {
		return true
	}
	for _, s := range strings.Split(src, ",") {
		if strings.TrimSpace(s) == "user" {
			return true
		}
	}
	return false
}

func (r *Runner) connectionFile() string {
	return filepath.Join(r.cfg.DataDir, "claude", "login-settings.json")
}

// connectionSettings writes the personal connection settings for --settings and returns the
// file, or "" when there are none or Claude reads them itself. Written on every use, so a
// change to ~/.claude/settings.json applies to the next turn.
func (r *Runner) connectionSettings() (string, error) {
	if r.loadsUser() || r.cfg.DataDir == "" {
		return "", nil
	}
	path := r.connectionFile()
	keep := map[string]json.RawMessage{}
	if src := userSettings(); src != "" {
		data, err := os.ReadFile(src)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read %s: %w", src, err)
		}
		if len(bytes.TrimSpace(data)) > 0 {
			var all map[string]json.RawMessage
			if err := json.Unmarshal(data, &all); err != nil {
				return "", fmt.Errorf("%s is not valid JSON: %w", src, err)
			}
			for _, k := range connectionKeys {
				if v, ok := all[k]; ok {
					keep[k] = v
				}
			}
		}
	}
	if len(keep) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", nil
	}
	data, err := json.Marshal(keep)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	// Turns run side by side on Linux: each writes its own file and renames it into place.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".login-settings-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// settingArgs are the flags that choose Claude's settings, the same for turns and the check.
func (r *Runner) settingArgs(settings string) []string {
	var args []string
	if src := r.cfg.ClaudeSettingSources; src != "" && src != "all" {
		args = append(args, "--setting-sources", src)
	}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	return args
}

// authStatus is `claude auth status --json`; it reads the local configuration and calls no model.
type authStatus struct {
	LoggedIn    bool   `json:"loggedIn"`
	AuthMethod  string `json:"authMethod"`
	APIProvider string `json:"apiProvider"`
}

// checkLogin runs `claude auth status` as a turn would start Claude: same settings, working
// directory and environment. It says whether Claude finds credentials, not whether the API
// accepts them.
func (r *Runner) checkLogin(ctx context.Context, path, version string) runner.Status {
	settings, err := r.connectionSettings()
	if err != nil {
		return runner.Status{Detail: fmt.Sprintf("%s; personal Claude settings unusable: %v", version, err)}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append(r.settingArgs(settings), "auth", "status", "--json")...)
	cmd.Dir = r.cfg.ClaudeWorkDir
	cmd.Env = os.Environ()
	// Exits 1 when not logged in, with the status on stdout.
	out, _ := cmd.Output()
	var st authStatus
	if json.Unmarshal(bytes.TrimSpace(out), &st) != nil {
		// A Claude Code without `auth status`: only the version can be checked.
		return runner.Status{OK: true, Detail: version}
	}
	switch {
	case st.APIProvider != "" && st.APIProvider != "firstParty":
		// Bedrock, Vertex, Foundry: their own cloud credentials, which this does not check.
		return runner.Status{OK: true, Detail: fmt.Sprintf("%s; uses %s", version, st.APIProvider)}
	case !st.LoggedIn:
		return runner.Status{Detail: fmt.Sprintf("%s; not logged in: %s", version, loginHint)}
	}
	return runner.Status{OK: true, Detail: fmt.Sprintf("%s; logged in (%s)", version, st.AuthMethod)}
}
