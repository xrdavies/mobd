//go:build linux

package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
)

func TestDesktopRunsItsOwnSoundServer(t *testing.T) {
	f := newFixture(t)
	d, err := f.m.Ensure(context.Background(), sid, "turn")
	if err != nil {
		t.Fatal(err)
	}
	rec := f.m.desks[sid].record()
	dir := filepath.Join(rec.RuntimeDir, "pulse")
	if rec.Sound != filepath.Join(dir, "native") || rec.PIDs["pulse"] == 0 {
		t.Fatalf("record %+v", rec)
	}
	s := f.sp.spec("pulseaudio")
	if s.CgroupDir == nil || !strings.HasSuffix(s.CgroupDir.Name(), "/session") || !s.NewSession {
		t.Errorf("pulseaudio not started in session/: %+v", s)
	}
	if !slices.Contains(s.Args, "-n") || !slices.Contains(s.Args, "--file="+filepath.Join(dir, "default.pa")) ||
		!slices.Contains(s.Env, "PULSE_RUNTIME_PATH="+dir) || !slices.Contains(s.Env, "XDG_RUNTIME_DIR="+rec.RuntimeDir) {
		t.Errorf("pulseaudio spec %+v", s)
	}
	conf, err := os.ReadFile(filepath.Join(dir, "default.pa"))
	if err != nil || !strings.Contains(string(conf), "module-null-sink sink_name=mob") ||
		!strings.Contains(string(conf), "socket="+rec.Sound) {
		t.Errorf("default.pa %q %v", conf, err)
	}
	// The window manager, and through it every app, plays into this desktop's server.
	if wm := f.sp.spec("openbox"); !slices.Contains(wm.Env, "PULSE_SERVER=unix:"+rec.Sound) {
		t.Errorf("openbox env lacks the sound server: %v", wm.Env)
	}
	if !slices.Contains(f.m.desks[sid].agentEnv(rec), "PULSE_SERVER=unix:"+rec.Sound) {
		t.Error("agent env lacks the sound server")
	}
	if !d.Source().(interface{ HasSound() bool }).HasSound() || !f.m.Sound() {
		t.Error("sound not offered")
	}
	if _, ok := d.Source().(desktop.AudioSource); !ok {
		t.Error("the live view source cannot capture sound")
	}
}

func TestSoundServerIsRestartedThenGivenUp(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.Ensure(context.Background(), sid, "turn"); err != nil {
		t.Fatal(err)
	}
	socket := f.m.desks[sid].record().Sound
	for i := range maxSoundRestarts {
		old := f.m.desks[sid].record().PIDs["pulse"]
		_ = f.sp.kill(old)
		deadline := time.Now().Add(5 * time.Second)
		for f.m.desks[sid].record().PIDs["pulse"] == old {
			if time.Now().After(deadline) {
				t.Fatalf("restart %d: sound server not restarted", i+1)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if rec := f.m.desks[sid].record(); rec.Sound != socket || f.sp.alive("pulseaudio") != 1 {
			t.Fatalf("restart %d: %+v, %d servers", i+1, rec, f.sp.alive("pulseaudio"))
		}
	}
	// Past the limit the desktop keeps running, silent.
	_ = f.sp.kill(f.m.desks[sid].record().PIDs["pulse"])
	deadline := time.Now().Add(5 * time.Second)
	for f.m.desks[sid].record().Sound != "" {
		if time.Now().After(deadline) {
			t.Fatal("sound not given up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := f.m.desks[sid].State(); st != desktop.StateRunning {
		t.Errorf("desktop %s after the sound server gave up", st)
	}
}

func TestDesktopWithoutPulseAudioStartsSilent(t *testing.T) {
	f := newFixture(t)
	look := f.m.look
	f.m.look = func(name string) (string, error) {
		if name == "pulseaudio" {
			return "", errors.New("not found")
		}
		return look(name)
	}
	d, err := f.m.Ensure(context.Background(), sid, "turn")
	if err != nil {
		t.Fatal(err)
	}
	rec := f.m.desks[sid].record()
	if rec.Sound != "" || rec.PIDs["pulse"] != 0 || d.State() != desktop.StateRunning {
		t.Errorf("record %+v", rec)
	}
	for _, e := range f.m.desks[sid].agentEnv(rec) {
		if strings.HasPrefix(e, "PULSE_SERVER=") {
			t.Errorf("agent env names a sound server: %s", e)
		}
	}
	if d.Source().(interface{ HasSound() bool }).HasSound() {
		t.Error("a silent desktop offered sound")
	}
}
