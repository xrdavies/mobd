//go:build linux

package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/cgroup"
	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/desktop/manager"
	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/proc"
	"github.com/xrdavies/mobd/internal/runner/codex"
	"github.com/xrdavies/mobd/internal/sysd"
	"github.com/xrdavies/mobd/internal/task"
)

// TestTwoConversationsRunOnTheirOwnDesktops dispatches two turns through the real desktop
// manager: MOBD_DESKTOP_TEST=1, run inside the user manager (systemd-run --user --scope) with
// xvfb, dbus, openbox, x11-xkb-utils and x11-utils installed. Each turn waits for the other, so
// both must run at once.
func TestTwoConversationsRunOnTheirOwnDesktops(t *testing.T) {
	if os.Getenv("MOBD_DESKTOP_TEST") != "1" {
		t.Skip("set MOBD_DESKTOP_TEST=1 to run turns on real desktops")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sd, err := sysd.User(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sd.Close()
	dir := t.TempDir()
	m, err := manager.New(manager.Options{
		DataDir: filepath.Join(dir, "desktops"), RuntimeDir: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "mobd-test"),
		Size: [2]int{1024, 768}, Scale: 1, Layout: "us", DisplayBase: 160, Version: "test",
		Systemd: sd, Cgroups: cgroup.New(cgroup.DefaultRoot), Spawner: proc.Exec{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()

	bin := filepath.Join(dir, "fake-codex")
	script := `cat > /dev/null
cd "` + dir + `"
out="out-$$"
echo "$DISPLAY" > "$out"
grep -o '[^:]*$' /proc/self/cgroup >> "$out"
cat /proc/self/oom_score_adj >> "$out"
touch "ready-$$"
while [ "$(ls ready-* | wc -l)" -lt 2 ]; do sleep 0.05; done
xdpyinfo > /dev/null && echo xdpyinfo-ok >> "$out"
printf '%s\n' '{"type":"thread.started","thread_id":"th"}' '{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}'
`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{CodexBin: bin, CodexWorkDir: dir, MobMCPCommand: "mob-mcp", MaxRunning: 4, DataDir: dir}
	auth, err := pairing.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, task.NewStore(), auth, m, codex.New(cfg))
	defer auth.Close()
	defer s.Close()

	a, b := postTask(t, s, `{"prompt":"a"}`), postTask(t, s, `{"prompt":"b"}`)
	for _, id := range []string{a.ID, b.ID} {
		if got := waitTask(t, s, id); got.Status != task.StatusSucceeded {
			t.Fatalf("turn %s: %+v", id, got)
		}
	}
	outs, _ := filepath.Glob(filepath.Join(dir, "out-*"))
	if len(outs) != 2 {
		t.Fatalf("outputs %v", outs)
	}
	displays := map[string]bool{}
	for _, o := range outs {
		b, _ := os.ReadFile(o)
		lines := strings.Fields(string(b))
		if len(lines) != 4 {
			t.Fatalf("%s: %q", o, b)
		}
		display, group, oom, x := lines[0], lines[1], lines[2], lines[3]
		displays[display] = true
		if !strings.HasPrefix(display, ":16") || !strings.HasSuffix(group, ".scope/turn") || oom != "300" || x != "xdpyinfo-ok" {
			t.Errorf("%s: display %s, cgroup %s, oom_score_adj %s, %s", o, display, group, oom, x)
		}
	}
	if len(displays) != 2 {
		t.Errorf("both turns on %v", displays)
	}
	for _, sess := range []string{a.SessionID, b.SessionID} {
		if err := m.Remove(context.Background(), sess); err != nil {
			t.Error(err)
		}
	}
}
