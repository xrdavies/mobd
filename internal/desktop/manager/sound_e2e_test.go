//go:build linux

package manager

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/sysd"
)

// TestRealDesktopSound plays a tone on one of two real desktops and captures both: the playing
// desktop's sound carries it, the other stays silent (no crossing between conversations). Needs
// what TestRealDesktop needs plus pulseaudio, gstreamer1.0-pulseaudio, gstreamer1.0-plugins-base
// and gstreamer1.0-tools.
func TestRealDesktopSound(t *testing.T) {
	if os.Getenv("MOBD_DESKTOP_TEST") != "1" {
		t.Skip("set MOBD_DESKTOP_TEST=1 to start a real desktop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sd, err := sysd.User(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sd.Close()
	m := realManager(t, sd, t.TempDir())
	defer m.Shutdown()
	if !m.Sound() {
		t.Fatal("sound unavailable: install pulseaudio, gstreamer1.0-pulseaudio and gstreamer1.0-plugins-base")
	}
	ids := []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	desks := make([]desktop.Desktop, 2)
	for i, id := range ids {
		defer m.Remove(context.Background(), id)
		if desks[i], err = m.Ensure(ctx, id, "test"); err != nil {
			t.Fatal(err)
		}
	}
	r, _, _ := m.Reserve(ids[0])
	env, err := r.Turn(desks[0])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	// An app of the first conversation plays a tone, as the agent's apps would.
	tone := exec.CommandContext(ctx, "gst-launch-1.0", "-q", "audiotestsrc", "wave=sine", "freq=440", "volume=0.5", "!", "pulsesink")
	tone.Env = append(os.Environ(), env.Env...)
	if err := tone.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tone.Process.Kill(); _ = tone.Wait() }()

	capture := func(d desktop.Desktop) (packets, loud int) {
		s, err := d.Source().(desktop.AudioSource).StartAudio(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// Two seconds from the first packet: connecting to the server takes a moment.
		start := time.Now()
		var end <-chan time.Time
		for {
			select {
			case p, ok := <-s.Packets():
				if !ok {
					t.Fatalf("capture ended: %v", s.Err())
				}
				if toc := p.Data[0]; toc&0x04 == 0 || toc&0x03 != 0 {
					t.Fatalf("TOC %#x is not one stereo frame", toc)
				}
				if end == nil {
					t.Logf("first packet after %v", time.Since(start).Round(time.Millisecond))
					end = time.After(2 * time.Second)
				}
				packets++
				if len(p.Data) > 100 {
					loud++
				}
			case <-end:
				return packets, loud
			}
		}
	}
	time.Sleep(time.Second) // the tone reaches the server
	n, loud := capture(desks[0])
	t.Logf("playing desktop: %d packets, %d with sound", n, loud)
	if n < 90 || loud < n/2 {
		t.Errorf("playing desktop: %d packets, %d with sound", n, loud)
	}
	n, loud = capture(desks[1])
	t.Logf("other desktop: %d packets, %d with sound", n, loud)
	if n < 90 || loud > 2 {
		t.Errorf("the other conversation hears it: %d packets, %d with sound", n, loud)
	}
}
