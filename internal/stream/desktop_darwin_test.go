package stream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"

	"github.com/xrdavies/mobd/internal/desktop"
)

// The real pipeline on this Mac: ScreenCaptureKit + VideoToolbox through Pion to a viewer.
// Needs Screen Recording for the test process.
func TestRealDesktopReachesTheViewer(t *testing.T) {
	if p, _ := desktop.CurrentPermissions(); !p.ScreenRecording {
		t.Skip("no Screen Recording permission")
	}
	if st, _ := desktop.State(); st.Locked {
		t.Skip("screen locked")
	}
	m, err := NewManager(Config{
		Loopback: true, Start: desktop.StartStream, KeepAwake: desktop.KeepAwake,
		Display: func() (desktop.Display, error) {
			d, err := desktop.Current().Displays()
			if err != nil {
				return desktop.Display{}, err
			}
			return d[0], nil
		},
		Attach: func(parent context.Context, _ string) (context.Context, func(), error) {
			ctx, cancel := context.WithCancel(parent)
			return ctx, cancel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	start := time.Now()
	v, a := connectReal(t, m)
	var keyframes, bytes atomic.Int64
	var firstKey atomic.Int64
	go func() {
		track := <-v.track
		var h codecs.H264Packet
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			bytes.Add(int64(len(pkt.Payload)))
			if h.IsPartitionHead(pkt.Payload) && isIDR(pkt.Payload) {
				if keyframes.Add(1) == 1 {
					firstKey.Store(int64(time.Since(start)))
				}
			}
		}
	}()
	waitFor(t, "a keyframe over RTP", func() bool { return keyframes.Load() > 0 })
	t.Logf("offer → first keyframe at the viewer: %s (%dx%d)", time.Duration(firstKey.Load()).Round(time.Millisecond), a.Width, a.Height)
	b0 := bytes.Load()
	time.Sleep(2 * time.Second)
	t.Logf("next 2 s: %d kbps", (bytes.Load()-b0)*8/2000)
}

// isIDR reports whether an H.264 RTP payload starts or carries (STAP-A) an IDR or SPS.
func isIDR(p []byte) bool {
	if len(p) < 2 {
		return false
	}
	switch t := p[0] & 0x1f; t {
	case 5, 7:
		return true
	case 24: // STAP-A: sizes and NAL units
		for i := 1; i+2 < len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			if i+2 < len(p) && (p[i+2]&0x1f == 5 || p[i+2]&0x1f == 7) {
				return true
			}
			i += 2 + n
		}
	case 28: // FU-A
		return p[1]&0x1f == 5
	}
	return false
}
