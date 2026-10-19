//go:build linux

package x11

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/desktop/gst"
)

// fakeEncoder records what the stream asks of it; tests emit units by hand.
type fakeEncoder struct {
	mu           sync.Mutex
	pushes, keys int
	last         gst.Frame
	sizes        [][2]int
	out          chan gst.Unit
	closed       bool
	closeOnce    sync.Once
}

func (e *fakeEncoder) Push(f gst.Frame) {
	e.mu.Lock()
	e.pushes++
	e.last = f
	e.mu.Unlock()
}
func (e *fakeEncoder) Units() <-chan gst.Unit { return e.out }
func (e *fakeEncoder) SetBitrate(int)         {}
func (e *fakeEncoder) SetSize(w, h int) {
	e.mu.Lock()
	e.sizes = append(e.sizes, [2]int{w, h})
	e.mu.Unlock()
}
func (e *fakeEncoder) ForceKeyframe() { e.mu.Lock(); e.keys++; e.mu.Unlock() }
func (e *fakeEncoder) Err() error     { return nil }
func (e *fakeEncoder) Close()         { e.closeOnce.Do(func() { close(e.out) }) }
func (e *fakeEncoder) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pushes, e.keys
}

func waitCount(t *testing.T, what string, get func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for get() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d, want %d", what, get(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamPushesOnlyChanges(t *testing.T) {
	addrs, cookie := xvfb(t, 196)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	enc := &fakeEncoder{out: make(chan gst.Unit, 4)}
	var opened gst.EncoderOptions
	factory := gst.Factory{Name: "fake", New: func(o gst.EncoderOptions) (gst.Encoder, error) { opened = o; return enc, nil }}
	x, err := Open(ctx, ":196", addrs, cookie, Options{Scale: 1, Encoders: []gst.Factory{factory}})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	b := NewBackend(x)
	st, err := b.StartStream(ctx, desktop.StreamOptions{Width: 321, Height: 240, FPS: 30, Kbps: 500})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if opened.Width != 320 || opened.Height != 240 || opened.FPS != 30 {
		t.Errorf("encoder options %+v", opened)
	}
	pushes := func() int { p, _ := enc.counts(); return p }
	waitCount(t, "first frame", pushes, 1)
	if f := enc.last; f.Width != 640 || f.Height != 480 || f.Stride != 640*4 || len(f.Pix) < 640*480*4 {
		t.Errorf("frame %dx%d stride %d", f.Width, f.Height, f.Stride)
	}
	time.Sleep(600 * time.Millisecond)
	if n := pushes(); n != 1 {
		t.Errorf("%d frames on a still screen", n)
	}

	// A change: frames follow within a couple of frame intervals.
	other, err := xgbConn(addrs[0], cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	root := xproto.Setup(other).DefaultScreen(other).Root
	paint := func(c uint32) {
		xproto.ChangeWindowAttributes(other, root, xproto.CwBackPixel, []uint32{c})
		xproto.ClearArea(other, false, root, 0, 0, 0, 0)
		other.Sync()
	}
	start := time.Now()
	paint(0xff0000)
	waitCount(t, "frame after a change", pushes, 2)
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("change took %v to reach the encoder", d)
	}
	if f := enc.last; f.Pix[2] != 0xff || f.Pix[0] != 0 { // BGRX: red
		t.Errorf("frame after painting red: % x", f.Pix[:4])
	}
	time.Sleep(300 * time.Millisecond)
	settled := pushes()

	// A keyframe on a still screen re-encodes the last frame.
	st.Keyframe()
	waitCount(t, "keyframe push", pushes, settled+1)
	if _, keys := enc.counts(); keys != 1 {
		t.Errorf("%d forced keyframes", keys)
	}
	st.SetSize(160, 120)
	waitCount(t, "push after resize", pushes, settled+2)
	if len(enc.sizes) != 1 || enc.sizes[0] != [2]int{160, 120} {
		t.Errorf("sizes %v", enc.sizes)
	}

	// Units come out with an AUD first and Key from the IDR.
	enc.out <- gst.Unit{Data: []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 3}}
	select {
	case u := <-st.Units():
		if !u.Key || !bytes.HasPrefix(u.Data, []byte{0, 0, 0, 1, 9}) || u.Width != 160 {
			t.Errorf("unit %+v", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no unit")
	}
	if x.fastAck.Load() != int64(time.Second/30) {
		t.Errorf("acknowledging every %v while streaming", time.Duration(x.fastAck.Load()))
	}
	st.Close()
	st.Close()
	if _, ok := <-st.Units(); ok {
		t.Error("units open after Close")
	}
	if st.Err() != nil || x.fastAck.Load() != 0 {
		t.Errorf("after Close: err %v, fastAck %v", st.Err(), x.fastAck.Load())
	}

	none := NewBackend(&X11{done: make(chan struct{})})
	if _, err := none.StartStream(ctx, desktop.StreamOptions{}); !errors.Is(err, desktop.ErrUnsupported) {
		t.Errorf("without encoders: %v", err)
	}
}

// With GStreamer and OpenH264 installed: real units, a keyframe first and on request, none
// while the screen is still.
func TestStreamOpenH264(t *testing.T) {
	encoders := gst.Probe(context.Background())
	if len(encoders) == 0 {
		t.Skip("no H.264 encoder (GStreamer with OpenH264, cgo)")
	}
	addrs, cookie := xvfb(t, 197)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	x, err := Open(ctx, ":197", addrs, cookie, Options{Scale: 1, Encoders: encoders[len(encoders)-1:]})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	st, err := NewBackend(x).StartStream(ctx, desktop.StreamOptions{Width: 640, Height: 480, FPS: 30, Kbps: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	next := func(within time.Duration) (desktop.AccessUnit, bool) {
		select {
		case u, ok := <-st.Units():
			return u, ok
		case <-time.After(within):
			return desktop.AccessUnit{}, false
		}
	}
	u, ok := next(3 * time.Second)
	if !ok || !u.Key || !bytes.Contains(u.Data, []byte{0, 0, 0, 1, 0x67}) {
		t.Fatalf("first unit: key %v, %d bytes", u.Key, len(u.Data))
	}
	if _, ok := next(500 * time.Millisecond); ok {
		t.Error("units from a still screen")
	}
	st.Keyframe()
	if u, ok := next(2 * time.Second); !ok || !u.Key {
		t.Errorf("keyframe on request: %v %v", ok, u.Key)
	}
}
