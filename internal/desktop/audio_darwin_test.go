package desktop

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestLiveAudioCapture captures this Mac's sound while a system sound plays: stereo 20 ms Opus
// packets at 50 a second, larger while something is audible. Needs Screen Recording.
func TestLiveAudioCapture(t *testing.T) {
	if p, _ := CurrentPermissions(); !p.ScreenRecording {
		t.Skip("no Screen Recording permission")
	}
	s, err := macOS{}.StartAudio(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	quiet, loud := 0, 0
	count := 0
	var first, last time.Duration
	end := time.After(2 * time.Second)
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command("afplay", "-v", "0.3", "/System/Library/Sounds/Submarine.aiff").Run()
	}()
loop:
	for {
		select {
		case p, ok := <-s.Packets():
			if !ok {
				t.Fatalf("capture ended: %v", s.Err())
			}
			// TOC byte: the stereo flag, and one frame per packet.
			if toc := p.Data[0]; toc&0x04 == 0 || toc&0x03 != 0 {
				t.Fatalf("TOC %#x is not one stereo frame", toc)
			}
			if count == 0 {
				first = p.PTS
			}
			last = p.PTS
			count++
			if len(p.Data) > 100 {
				loud++
			} else {
				quiet++
			}
		case <-end:
			break loop
		}
	}
	t.Logf("%d packets (%d with sound, %d quiet) spanning %s", count, loud, quiet, last-first)
	if count < 60 || count > 110 {
		t.Fatalf("%d packets in 2 s, want about 100", count)
	}
	if loud == 0 {
		t.Fatal("no packet carried the system sound")
	}
}
