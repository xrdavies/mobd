//go:build linux

package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/desktop/gst"
	"github.com/xrdavies/mobd/internal/proc"
)

// Desktop sound (docs/desktop-audio.md, stage 2): each desktop runs its own PulseAudio server in
// session/, with one virtual output ("mob"). The agent's environment names its socket
// (PULSE_SERVER), so a conversation's apps play into its own output and never into another
// desktop's. The live view captures that output's monitor. Without PulseAudio a desktop works
// silently; `mobd desktops doctor` says what to install.

const (
	soundSink = "mob"
	// maxSoundRestarts a minute, then the desktop stays silent until it restarts.
	maxSoundRestarts  = 5
	soundStartTimeout = 5 * time.Second
)

// soundConfig is the server's whole configuration (-n: no default.pa): a virtual output, and
// the socket in the desktop's private runtime directory (mode 0700), so no other auth is needed.
func soundConfig(socket string) []byte {
	return []byte(fmt.Sprintf(`load-module module-null-sink sink_name=%s sink_properties=device.description=Mob rate=48000 channels=2
set-default-sink %s
set-default-source %s.monitor
load-module module-native-protocol-unix socket=%s auth-anonymous=1
`, soundSink, soundSink, soundSink, socket))
}

// soundDir is where a desktop's sound server keeps its socket, cookie and state.
func soundDir(rec record) string { return filepath.Join(rec.RuntimeDir, "pulse") }

// Sound reports whether desktops can share their sound: PulseAudio for the desktops, GStreamer
// with pulsesrc and opusenc for the live view.
func (m *Manager) Sound() bool { return m.soundOK() }

func (m *Manager) soundCheck() desktop.DoctorCheck {
	path, err := m.look("pulseaudio")
	switch {
	case err != nil:
		return desktop.DoctorCheck{Name: "sound", Warn: true, Detail: "pulseaudio not found: the live view has no sound",
			Fix: "sudo apt install pulseaudio"}
	case !gst.CanCaptureAudio():
		return desktop.DoctorCheck{Name: "sound", Warn: true, Detail: "GStreamer cannot capture PulseAudio or encode Opus: the live view has no sound",
			Fix: "sudo apt install gstreamer1.0-pulseaudio gstreamer1.0-plugins-base"}
	}
	return desktop.DoctorCheck{Name: "sound", OK: true, Detail: path + ", one output per desktop"}
}

// startSound starts the desktop's sound server; rec.Sound names its socket once it listens. A
// server that does not come up leaves the desktop silent, not failed.
func (d *desk) startSound(ctx context.Context, rec *record, logf, sessionFD *os.File) {
	path, err := d.m.look("pulseaudio")
	if err != nil {
		return
	}
	c, socket, err := d.spawnSound(ctx, *rec, path, logf, sessionFD)
	if err != nil {
		log.Printf("desktop %s: no sound: %v", d.sid, err)
		return
	}
	rec.PIDs["pulse"] = c.pid
	rec.Sound = socket
}

func (d *desk) spawnSound(ctx context.Context, rec record, path string, logf, sessionFD *os.File) (*child, string, error) {
	dir := soundDir(rec)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, "", err
	}
	socket := filepath.Join(dir, "native")
	_ = os.Remove(socket)
	conf := filepath.Join(dir, "default.pa")
	if err := os.WriteFile(conf, soundConfig(socket), 0600); err != nil {
		return nil, "", err
	}
	// Its runtime, state and cookie stay in the desktop's runtime directory, never ~/.config.
	env := childEnv(append(rec.env(), "XDG_RUNTIME_DIR="+rec.RuntimeDir, "PULSE_RUNTIME_PATH="+dir,
		"PULSE_STATE_PATH="+dir, "PULSE_CONFIG_PATH="+dir, "PULSE_COOKIE="+filepath.Join(dir, "cookie"))...)
	c, err := d.spawn("pulse", proc.Spec{
		Path: path, Args: []string{"-n", "--file=" + conf, "--daemonize=no", "--exit-idle-time=-1",
			"--disallow-exit", "--disallow-module-loading", "--use-pid-file=no", "--system=no", "--log-target=stderr"},
		Env: env, Stdout: logf, Stderr: logf, NewSession: true, CgroupDir: sessionFD,
	})
	if err != nil {
		return nil, "", err
	}
	wctx, cancel := context.WithTimeout(ctx, soundStartTimeout)
	defer cancel()
	sctx, scancel := untilExit(wctx, c)
	err = d.m.soundReady(sctx, socket)
	scancel()
	switch {
	case err == nil:
		return c, socket, nil
	case c.gone():
		return nil, "", errors.New("pulseaudio exited; see logs/sound.log")
	}
	_ = d.m.kill(c.pid)
	return nil, "", fmt.Errorf("pulseaudio did not open its socket: %w", err)
}

// socketReady waits until path exists (the server listens once it made its socket).
func socketReady(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// restartSound starts the sound server again after it exited, at the same socket so the
// desktop's apps find it; past the limit the desktop stays silent. False when the run ended.
func (d *desk) restartSound(done chan struct{}) bool {
	if err := d.lock(context.Background()); err != nil {
		return false
	}
	defer d.unlock()
	if d.run() != done || !live(d.State()) {
		return false
	}
	now := d.m.now()
	d.mu.Lock()
	var recent []time.Time
	for _, t := range d.soundRestarts {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	d.soundRestarts = append(recent, now)
	delete(d.procs, "pulse")
	d.mu.Unlock()
	rec := d.record()
	rec.PIDs = clonePIDs(rec.PIDs)
	delete(rec.PIDs, "pulse")
	if len(recent) >= maxSoundRestarts {
		log.Printf("desktop %s: sound server exited %d times in a minute; no sound until the desktop restarts", d.sid, maxSoundRestarts+1)
		rec.Sound = ""
		d.commit(rec)
		return true
	}
	path, err := d.m.look("pulseaudio")
	if err == nil {
		var l *logFile
		if l, err = openLog(filepath.Join(d.dir, "logs"), "sound"); err == nil {
			defer l.Close()
			var sessionFD *os.File
			if sessionFD, err = d.m.opts.Cgroups.Open(filepath.Join(d.m.opts.Cgroups.Path(rec.Cgroup), "session")); err == nil {
				defer sessionFD.Close()
				ctx, cancel := context.WithTimeout(context.Background(), soundStartTimeout)
				var c *child
				c, _, err = d.spawnSound(ctx, rec, path, l.File, sessionFD)
				cancel()
				if err == nil {
					rec.PIDs["pulse"] = c.pid
				}
			}
		}
	}
	if err != nil {
		log.Printf("desktop %s: sound server could not be restarted: %v", d.sid, err)
		rec.Sound = ""
	} else {
		log.Printf("desktop %s: sound server restarted", d.sid)
	}
	d.commit(rec)
	return true
}

// soundHub shares the desktop's sound capture between its viewers.
func (d *desk) soundHub() *desktop.AudioHub {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sound == nil {
		d.sound = desktop.NewAudioHub(d.captureSound)
	}
	return d.sound
}

func (d *desk) captureSound() (desktop.AudioStream, error) {
	rec := d.record()
	if !live(rec.State) {
		return nil, desktop.ErrNoDesktop
	}
	if rec.Sound == "" {
		return nil, errors.New("this desktop has no sound server; run mobd desktops doctor")
	}
	a, err := gst.CaptureAudio("unix:"+rec.Sound, soundSink+".monitor")
	if err != nil {
		return nil, fmt.Errorf("desktop sound: %w", err)
	}
	return newSoundStream(a), nil
}

// soundStream adapts a GStreamer capture to desktop.AudioStream.
type soundStream struct {
	a   gst.Audio
	out chan desktop.AudioPacket
}

func newSoundStream(a gst.Audio) *soundStream {
	s := &soundStream{a: a, out: make(chan desktop.AudioPacket, 4)}
	go func() {
		defer close(s.out)
		for p := range a.Packets() {
			s.out <- desktop.AudioPacket{Data: p.Data, PTS: p.PTS}
		}
	}()
	return s
}

func (s *soundStream) Packets() <-chan desktop.AudioPacket { return s.out }
func (s *soundStream) Err() error                          { return s.a.Err() }

// Close stops the capture; the hub keeps reading Packets until it closes.
func (s *soundStream) Close() { s.a.Close() }

// StartAudio joins the desktop's sound capture (desktop.AudioSource).
func (s source) StartAudio(ctx context.Context) (desktop.AudioStream, error) {
	return s.d.soundHub().StartAudio(ctx)
}

// HasSound reports whether this desktop can share sound now; the stream answers unavailable
// otherwise instead of offering an audio track.
func (s source) HasSound() bool {
	r := s.d.record()
	return s.d.m.soundOK() && (r.Sound != "" || r.State == desktop.StateStarting)
}
