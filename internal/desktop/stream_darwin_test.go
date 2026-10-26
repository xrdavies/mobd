package desktop

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// nextUnit waits for a unit matching want, discarding others.
func nextUnit(t *testing.T, s Stream, want func(AccessUnit) bool) (AccessUnit, time.Duration) {
	t.Helper()
	start := time.Now()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case u, ok := <-s.Units():
			if !ok {
				t.Fatalf("stream ended: %v", s.Err())
			}
			if want(u) {
				return u, time.Since(start)
			}
		case <-timeout:
			t.Fatal("no matching access unit within 3 s")
		}
	}
}

func checkKeyframe(t *testing.T, u AccessUnit, w, h int) {
	t.Helper()
	types := nalTypes(u.Data)
	if !u.Key || len(types) == 0 || types[0] != 9 || !slices.Contains(types, 7) || !slices.Contains(types, 8) || !slices.Contains(types, 5) {
		t.Fatalf("keyframe NAL types %v key=%t", types, u.Key)
	}
	sw, sh, err := spsSize(u.Data)
	if err != nil || sw != w || sh != h || u.Width != w || u.Height != h {
		t.Fatalf("keyframe size: SPS %dx%d reported %dx%d want %dx%d (%v)", sw, sh, u.Width, u.Height, w, h, err)
	}
}

// Encode benchmark and behaviour of a running stream (M3). Needs Screen Recording for the test process.
func TestMacOSStreamEncodesAndAdjusts(t *testing.T) {
	if p, _ := CurrentPermissions(); !p.ScreenRecording {
		t.Skip("no Screen Recording permission")
	}
	if st, _ := State(); st.Locked || st.Asleep {
		t.Skipf("screen unavailable: %+v", st)
	}
	start := time.Now()
	s, err := StartStream(context.Background(), StreamOptions{Width: 1920, Height: 1080, FPS: 30, Kbps: 4000})
	if err != nil {
		t.Fatal(err)
	}
	isKey := func(w int) func(AccessUnit) bool { return func(u AccessUnit) bool { return u.Key && u.Width == w } }
	u, _ := nextUnit(t, s, isKey(1920))
	checkKeyframe(t, u, 1920, 1080)
	t.Logf("start → first keyframe 1920x1080: %s, %d KB", time.Since(start).Round(time.Millisecond), len(u.Data)/1024)

	for _, size := range [][2]int{{1280, 720}, {960, 540}, {1920, 1080}} {
		s.SetSize(size[0], size[1])
		u, took := nextUnit(t, s, isKey(size[0]))
		checkKeyframe(t, u, size[0], size[1])
		t.Logf("resize → keyframe %dx%d: %s, %d KB", size[0], size[1], took.Round(time.Millisecond), len(u.Data)/1024)
	}

	// Idle: no periodic keyframes; only real screen changes produce units.
	drain := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case <-s.Units():
		case <-drain:
			done = true
		}
	}
	idle, idleBytes := 0, 0
	window := time.After(2 * time.Second)
	for done := false; !done; {
		select {
		case u := <-s.Units():
			idle++
			idleBytes += len(u.Data)
		case <-window:
			done = true
		}
	}
	t.Logf("2 s without forced frames: %d units, %d KB (screen changes only)", idle, idleBytes/1024)

	s.Keyframe()
	u, took := nextUnit(t, s, func(u AccessUnit) bool { return u.Key })
	checkKeyframe(t, u, 1920, 1080)
	t.Logf("keyframe on request (idle screen), 4000 kbps: %s, %d KB", took.Round(time.Millisecond), len(u.Data)/1024)

	s.SetBitrate(500)
	s.Keyframe()
	u, took = nextUnit(t, s, func(u AccessUnit) bool { return u.Key })
	t.Logf("keyframe on request at 500 kbps: %s, %d KB", took.Round(time.Millisecond), len(u.Data)/1024)

	s.Close()
	closed := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-s.Units():
			if !ok {
				if s.Err() != nil {
					t.Fatalf("Close reported %v", s.Err())
				}
				return
			}
		case <-closed:
			t.Fatal("Units not closed after Close")
		}
	}
}

// Adjustments racing Close must never reach the freed native stream (a crash, not an error).
func TestMacOSStreamAdjustDuringClose(t *testing.T) {
	if p, _ := CurrentPermissions(); !p.ScreenRecording {
		t.Skip("no Screen Recording permission")
	}
	if st, _ := State(); st.Locked || st.Asleep {
		t.Skipf("screen unavailable: %+v", st)
	}
	for range 5 {
		s, err := StartStream(context.Background(), StreamOptions{Width: 640, Height: 360, FPS: 30, Kbps: 800})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				for j := range 200 {
					switch (i + j) % 4 {
					case 0:
						s.SetBitrate(500 + j)
					case 1:
						s.SetSize(640-2*(j%8), 360)
					case 2:
						s.SetFrameRate(10 + j%20)
					case 3:
						s.Keyframe()
					}
				}
			})
		}
		s.Close()
		wg.Wait()
		s.Keyframe() // after Close: a no-op
	}
}
