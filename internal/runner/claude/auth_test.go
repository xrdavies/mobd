package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/runner"
	"github.com/xrdavies/mobd/internal/task"
)

// personal writes Claude's personal settings into a fresh CLAUDE_CONFIG_DIR.
func personal(t *testing.T, settings string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
}

const personalSettings = `{
  "env": {"ANTHROPIC_BASE_URL": "https://gw.example.invalid", "ANTHROPIC_AUTH_TOKEN": "secret-token"},
  "apiKeyHelper": "~/bin/key",
  "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "touch /tmp/never"}]}]},
  "permissions": {"allow": ["Bash"]},
  "enabledPlugins": {"x@y": true},
  "model": "opus"
}`

// mobd#7: with "project", only the settings that connect Claude to its API are passed on.
func TestLoginSettingsCarryOnlyTheConnection(t *testing.T) {
	personal(t, personalSettings)
	data := t.TempDir()
	r := New(config.Config{DataDir: data, ClaudeSettingSources: "project"})
	path, err := r.connectionSettings()
	if err != nil || path != filepath.Join(data, "claude", "login-settings.json") {
		t.Fatalf("%q %v", path, err)
	}
	b, _ := os.ReadFile(path)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["env"] == nil || string(got["apiKeyHelper"]) != `"~/bin/key"` {
		t.Fatalf("carried %s", b)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v", st.Mode().Perm())
		}
		if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", st.Mode().Perm())
		}
	}
	// The token is in the file, never in the arguments.
	args := strings.Join(r.buildArgs(&task.Task{}, runner.Options{}, path), " ")
	if !strings.Contains(args, "--setting-sources project --settings "+path) || strings.Contains(args, "secret-token") {
		t.Fatalf("args %s", args)
	}

	// Claude reads them itself: nothing to pass on.
	for _, src := range []string{"user,project", "all", ""} {
		if p, err := New(config.Config{DataDir: data, ClaudeSettingSources: src}).connectionSettings(); p != "" || err != nil {
			t.Fatalf("%q: %q %v", src, p, err)
		}
	}
	// No connection settings any more: the old file goes.
	personal(t, `{"hooks": {}}`)
	if p, err := r.connectionSettings(); p != "" || err != nil {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale file: %v", err)
	}
	personal(t, `{not json`)
	if _, err := r.connectionSettings(); err == nil {
		t.Fatal("invalid settings accepted")
	}
}

// fakeClaude answers --version and `auth status` as Claude Code does, recording how it was run.
func fakeClaude(t *testing.T, status string) (bin, record string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	bin, record = filepath.Join(dir, "claude"), filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = --version ] && { echo '2.1.296 (Claude Code)'; exit 0; }\n" +
		"echo \"$PWD $*\" > " + record + "\n" +
		"cat <<'J'\n" + status + "\nJ\n" +
		"case '" + status + "' in *'\"loggedIn\": true'*) exit 0 ;; esac\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func TestCheckReportsWhetherClaudeFindsCredentials(t *testing.T) {
	personal(t, personalSettings)
	work := t.TempDir()
	work, _ = filepath.EvalSymlinks(work)
	for _, c := range []struct {
		status string
		ok     bool
		detail string
	}{
		{`{"loggedIn": true, "authMethod": "api_key_helper", "apiProvider": "firstParty"}`, true, "logged in (api_key_helper)"},
		{`{"loggedIn": false, "authMethod": "none", "apiProvider": "firstParty"}`, false, "not logged in: run claude /login"},
		{`{"loggedIn": false, "authMethod": "none", "apiProvider": "bedrock"}`, true, "uses bedrock"},
		// A Claude Code without `auth status`.
		{`error: unknown command 'auth'`, true, "2.1.296 (Claude Code)"},
	} {
		bin, record := fakeClaude(t, c.status)
		data := t.TempDir()
		st := New(config.Config{ClaudeBin: bin, ClaudeWorkDir: work, DataDir: data, ClaudeSettingSources: "project"}).Check(context.Background())
		if st.OK != c.ok || !strings.Contains(st.Detail, c.detail) || strings.Contains(st.Detail, "secret-token") {
			t.Fatalf("%s: %+v", c.status, st)
		}
		ran, _ := os.ReadFile(record)
		want := work + " --setting-sources project --settings " + filepath.Join(data, "claude", "login-settings.json") + " auth status --json"
		if strings.TrimSpace(string(ran)) != want {
			t.Fatalf("ran %q, want %q", ran, want)
		}
	}
}

func TestNotLoggedInTurnSaysHowToLogIn(t *testing.T) {
	stream := init1 + "\n" +
		`{"type":"assistant","error":"authentication_failed","message":{"id":"m1","content":[{"type":"text","text":"Not logged in · Please run /login"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}`
	err := streamJSON(strings.NewReader(stream), func(task.Event) error { return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "Not logged in") || !strings.Contains(err.Error(), "apiKeyHelper in ~/.claude/settings.json") {
		t.Fatalf("%v", err)
	}
}
