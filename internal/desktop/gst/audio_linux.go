//go:build linux && cgo

package gst

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// CanCaptureAudio reports whether GStreamer has what CaptureAudio needs: PulseAudio capture
// (gstreamer1.0-pulseaudio) and the Opus encoder (gstreamer1.0-plugins-base).
func CanCaptureAudio() bool {
	for _, f := range []string{"pulsesrc", "opusenc", "audioconvert", "audioresample", "appsink"} {
		if !has(f) {
			return false
		}
	}
	return true
}

type audioCapture struct {
	p    *pipe
	out  chan AudioPacket
	stop chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
	done chan struct{}
}

// CaptureAudio records a PulseAudio source (a sink's monitor) on server and encodes it as 48 kHz
// stereo Opus, 20 ms packets, 96 kbit/s, for music and effects rather than speech.
func CaptureAudio(server, device string) (Audio, error) {
	desc := fmt.Sprintf("pulsesrc name=pulse server=%s device=%s buffer-time=40000 latency-time=10000 client-name=mobd ! "+
		"audioconvert ! audioresample ! audio/x-raw,format=S16LE,rate=48000,channels=2 ! "+
		"opusenc bitrate=96000 frame-size=20 audio-type=generic ! "+
		"appsink name=sink sync=false max-buffers=10 drop=true", quote(server), quote(device))
	p, err := newPipe(desc)
	if err != nil {
		return nil, err
	}
	c := &audioCapture{p: p, out: make(chan AudioPacket, 10), stop: make(chan struct{}), done: make(chan struct{})}
	go c.read()
	return c, nil
}

// quote makes a gst-launch property value of s (paths may hold spaces).
func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func (c *audioCapture) read() {
	defer close(c.done)
	defer close(c.out)
	var n int64
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		data, _, eos := c.p.pull(100 * time.Millisecond)
		if eos {
			c.setErr(c.p.err())
			return
		}
		if data == nil {
			if err := c.p.err(); err != nil {
				c.setErr(err)
				return
			}
			continue
		}
		pkt := AudioPacket{Data: data, PTS: time.Duration(n) * AudioFrame}
		n++
		select {
		case c.out <- pkt:
		default: // the reader is behind: drop the oldest rather than play it late
			select {
			case <-c.out:
			default:
			}
			select {
			case c.out <- pkt:
			default:
			}
		}
	}
}

func (c *audioCapture) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil && err != nil {
		c.err = err
	}
}

func (c *audioCapture) Packets() <-chan AudioPacket { return c.out }

func (c *audioCapture) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close stops the capture; Packets is closed once it returns.
func (c *audioCapture) Close() {
	c.once.Do(func() {
		close(c.stop)
		<-c.done
		c.p.eos()
		c.p.free()
	})
}
