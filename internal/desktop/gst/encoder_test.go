//go:build linux && cgo

package gst

import (
	"context"
	"testing"
	"time"
)

// frame is a test pattern that moves with i.
func frame(w, h, i int) Frame {
	pix := make([]byte, w*h*4)
	for y := range h {
		for x := range w {
			o := (y*w + x) * 4
			pix[o], pix[o+1], pix[o+2] = byte(x+i*7), byte(y+i*3), byte((x^y)+i)
		}
	}
	return Frame{Pix: pix, Stride: w * 4, Width: w, Height: h, PTS: time.Duration(i) * 33 * time.Millisecond}
}

func openh264(t *testing.T, o EncoderOptions) Encoder {
	t.Helper()
	for _, k := range kinds {
		if k.name == "openh264enc" {
			if !k.available() {
				t.Skip("openh264enc not installed (gstreamer1.0-plugins-bad with OpenH264)")
			}
			e, err := k.factory().New(o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(e.Close)
			return e
		}
	}
	t.Fatal("no openh264enc kind")
	return nil
}

// pushing pushes frames 10 ms apart while collecting what comes out (a live viewer reads all
// the time; a reader that stops makes the appsink drop units).
func pushing(t *testing.T, e Encoder, frames []Frame, within time.Duration) []Unit {
	t.Helper()
	got := make(chan []Unit, 1)
	go func() { got <- collect(t, e, len(frames), within) }()
	for _, f := range frames {
		e.Push(f)
		time.Sleep(10 * time.Millisecond)
	}
	return <-got
}

// collect reads units until n arrived or the time is up.
func collect(t *testing.T, e Encoder, n int, within time.Duration) []Unit {
	t.Helper()
	var units []Unit
	deadline := time.After(within)
	for len(units) < n {
		select {
		case u, ok := <-e.Units():
			if !ok {
				t.Fatalf("units closed: %v", e.Err())
			}
			units = append(units, u)
		case <-deadline:
			return units
		}
	}
	return units
}

// decode runs the units through avdec_h264 and returns the decoded frames' sizes.
func decode(t *testing.T, units []Unit) [][2]int {
	t.Helper()
	if !has("avdec_h264") {
		t.Skip("avdec_h264 not installed (gstreamer1.0-libav)")
	}
	p, err := newPipe("appsrc name=src format=time ! h264parse ! avdec_h264 ! videoconvert ! " +
		"video/x-raw,format=BGRx ! appsink name=sink sync=false emit-signals=false")
	if err != nil {
		t.Fatal(err)
	}
	defer p.free()
	for i, u := range units {
		caps := ""
		if i == 0 {
			caps = outCaps
		}
		if err := p.push(u.Data, time.Duration(i)*33*time.Millisecond, caps); err != nil {
			t.Fatal(err)
		}
	}
	p.eos()
	var sizes [][2]int
	for {
		data, _, eos := p.pull(2 * time.Second)
		if err := p.err(); err != nil {
			t.Fatalf("decoder: %v", err)
		}
		if eos || data == nil {
			return sizes
		}
		sizes = append(sizes, [2]int{len(data), 0})
	}
}

// nalTypes lists the NAL unit types of an Annex B access unit.
func nalTypes(b []byte) []int {
	var out []int
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && (b[i+2] == 1 || (b[i+2] == 0 && b[i+3] == 1)) {
			j := i + 3
			if b[i+2] == 0 {
				j++
			}
			if j < len(b) {
				out = append(out, int(b[j]&0x1f))
			}
			i = j
		}
	}
	return out
}

func TestOpenH264EncodesDecodableVideo(t *testing.T) {
	e := openh264(t, EncoderOptions{Width: 320, Height: 240, FPS: 30, Kbps: 800})
	var frames []Frame
	for i := range 30 {
		frames = append(frames, frame(320, 240, i))
	}
	units := pushing(t, e, frames, 5*time.Second)
	if len(units) != 30 {
		t.Fatalf("%d units for 30 frames", len(units))
	}
	if !units[0].Key {
		t.Error("first unit is not a keyframe")
	}
	if types := nalTypes(units[0].Data); !contains(types, 7) || !contains(types, 8) || !contains(types, 5) {
		t.Errorf("first unit NAL types %v: want SPS, PPS and IDR", types)
	}
	keys := 0
	for _, u := range units[1:] {
		if u.Key {
			keys++
		}
	}
	if keys != 0 {
		t.Errorf("%d unrequested keyframes", keys)
	}
	decoded := decode(t, units)
	if len(decoded) != len(units) || decoded[0][0] != 320*240*4 {
		t.Fatalf("decoded %d frames (first %d bytes) from %d units", len(decoded), decoded[0][0], len(units))
	}

	// Idle: no frames in, no units out.
	if extra := collect(t, e, 1, 300*time.Millisecond); len(extra) != 0 {
		t.Errorf("%d units while idle", len(extra))
	}
	// A keyframe on request, then a new size (a keyframe too) and a new bitrate.
	e.ForceKeyframe()
	e.Push(frame(320, 240, 31))
	if u := collect(t, e, 1, 2*time.Second); len(u) != 1 || !u[0].Key {
		t.Errorf("forced keyframe: %+v", u)
	}
	e.SetSize(160, 120)
	e.SetBitrate(300)
	frames = frames[:0]
	for i := range 10 {
		frames = append(frames, frame(320, 240, 40+i))
	}
	resized := pushing(t, e, frames, 3*time.Second)
	if len(resized) == 0 || !resized[0].Key {
		t.Fatalf("after SetSize: %d units, first key %v", len(resized), len(resized) > 0 && resized[0].Key)
	}
	if d := decode(t, resized); len(d) == 0 || d[0][0] != 160*120*4 {
		t.Errorf("resized frames decode to %v", d)
	}
	if e.Err() != nil {
		t.Error(e.Err())
	}
}

func TestPushNeverBlocks(t *testing.T) {
	e := openh264(t, EncoderOptions{Width: 320, Height: 240, FPS: 30, Kbps: 800})
	start := time.Now()
	f := frame(320, 240, 0)
	for i := range 500 { // nobody reads Units
		f.PTS = time.Duration(i) * time.Millisecond
		e.Push(f)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("500 pushes took %v", d)
	}
	e.Close()
	e.Close()
	e.Push(f) // after Close: dropped
}

func TestProbe(t *testing.T) {
	if !has("openh264enc") {
		t.Skip("openh264enc not installed")
	}
	got := Probe(context.Background())
	if len(got) == 0 || got[len(got)-1].Name != "openh264enc" || got[len(got)-1].Detail != "software" {
		t.Fatalf("Probe %+v", got)
	}
	if _, err := got[0].New(EncoderOptions{Width: 321, Height: 240}); err == nil {
		t.Error("odd width accepted")
	}
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
