//go:build linux && cgo

package gst

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// kind is how one encoder element is put into a pipeline.
type kind struct {
	name, detail string
	needs        []string // element factories it uses
	// convert scales and converts raw frames into what the encoder takes; caps is the size
	// capsfilter's caps for a size.
	convert string
	caps    func(w, h int) string
	encoder func(kbps, fps int) string
	// bitrate is the encoder property and its value for kbps.
	bitrate func(kbps int) (string, uint)
}

// kinds are tried in this order: hardware first. Only openh264enc is covered by tests; the
// hardware lines follow the elements' documentation (to verify on hardware).
var kinds = []kind{
	{
		name: "vah264enc", detail: "VA-API", needs: []string{"vah264enc", "vapostproc"},
		convert: "vapostproc",
		caps: func(w, h int) string {
			return fmt.Sprintf("video/x-raw(memory:VAMemory),format=NV12,width=%d,height=%d", w, h)
		},
		encoder: func(kbps, _ int) string {
			return fmt.Sprintf("vah264enc name=enc rate-control=cbr bitrate=%d key-int-max=1048576 b-frames=0 target-usage=7", kbps)
		},
		bitrate: func(kbps int) (string, uint) { return "bitrate", uint(kbps) },
	},
	{
		name: "vaapih264enc", detail: "VA-API (gstreamer-vaapi)", needs: []string{"vaapih264enc"},
		convert: "videoconvert ! videoscale",
		caps:    func(w, h int) string { return fmt.Sprintf("video/x-raw,format=NV12,width=%d,height=%d", w, h) },
		encoder: func(kbps, _ int) string {
			return fmt.Sprintf("vaapih264enc name=enc rate-control=cbr bitrate=%d keyframe-period=0 max-bframes=0", kbps)
		},
		bitrate: func(kbps int) (string, uint) { return "bitrate", uint(kbps) },
	},
	{
		name: "nvh264enc", detail: "NVENC", needs: []string{"nvh264enc"},
		convert: "videoconvert ! videoscale",
		caps:    func(w, h int) string { return fmt.Sprintf("video/x-raw,format=NV12,width=%d,height=%d", w, h) },
		encoder: func(kbps, _ int) string {
			return fmt.Sprintf("nvh264enc name=enc rc-mode=cbr bitrate=%d gop-size=-1 bframes=0 zerolatency=true", kbps)
		},
		bitrate: func(kbps int) (string, uint) { return "bitrate", uint(kbps) },
	},
	{
		name: "openh264enc", detail: "software", needs: []string{"openh264enc"},
		convert: "videoconvert ! videoscale",
		caps:    func(w, h int) string { return fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d", w, h) },
		encoder: func(kbps, _ int) string {
			// Screen content, bitrate rate control, keyframes only on request.
			return fmt.Sprintf("openh264enc name=enc usage-type=screen rate-control=bitrate bitrate=%d gop-size=4294967295 complexity=low", kbps*1000)
		},
		bitrate: func(kbps int) (string, uint) { return "bitrate", uint(kbps) * 1000 },
	},
}

const outCaps = "video/x-h264,stream-format=byte-stream,alignment=au"

func (k kind) pipeline(o EncoderOptions) string {
	return fmt.Sprintf("appsrc name=src format=time is-live=true ! %s ! capsfilter name=size caps=%s ! %s ! "+
		"h264parse config-interval=-1 ! %s ! appsink name=sink sync=false max-buffers=4 drop=true",
		k.convert, k.caps(o.Width, o.Height), k.encoder(o.Kbps, o.FPS), outCaps)
}

func (k kind) available() bool {
	for _, n := range append([]string{"h264parse", "appsrc", "appsink", "videoconvert", "videoscale"}, k.needs...) {
		if !has(n) {
			return false
		}
	}
	return true
}

func (k kind) factory() Factory {
	return Factory{Name: k.name, Detail: k.detail, New: func(o EncoderOptions) (Encoder, error) { return newEncoder(k, o) }}
}

// Probe encodes two frames with every encoder this machine has and returns those that work,
// best first. It takes a second or two; mobd runs it once.
func Probe(ctx context.Context) []Factory {
	var out []Factory
	for _, k := range kinds {
		if !k.available() {
			continue
		}
		if err := probeOne(ctx, k); err != nil {
			continue
		}
		out = append(out, k.factory())
	}
	return out
}

// Available lists the encoder elements this machine has, best first, for doctor.
func Available() []string {
	var out []string
	for _, k := range kinds {
		if k.available() {
			out = append(out, k.name)
		}
	}
	return out
}

func probeOne(ctx context.Context, k kind) error {
	e, err := newEncoder(k, EncoderOptions{Width: 320, Height: 240, FPS: 30, Kbps: 500})
	if err != nil {
		return err
	}
	defer e.Close()
	f := Frame{Pix: make([]byte, 320*240*4), Stride: 320 * 4, Width: 320, Height: 240}
	for i := range 2 {
		f.PTS = time.Duration(i) * 33 * time.Millisecond
		e.Push(f)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	select {
	case _, ok := <-e.Units():
		if !ok {
			return e.Err()
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: no output", k.name)
	}
}

// encoder is a running pipeline; one goroutine feeds frames, one reads units.
type encoder struct {
	k    kind
	p    *pipe
	fps  int
	in   chan Frame
	out  chan Unit
	done chan struct{}
	wg   sync.WaitGroup

	mu     sync.Mutex
	err    error
	inCaps string
	closed bool
}

func newEncoder(k kind, o EncoderOptions) (*encoder, error) {
	if o.Width <= 0 || o.Height <= 0 || o.Width%2 != 0 || o.Height%2 != 0 {
		return nil, fmt.Errorf("encoder size %dx%d must be even and positive", o.Width, o.Height)
	}
	o.FPS = max(o.FPS, 1)
	p, err := newPipe(k.pipeline(o))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k.name, err)
	}
	e := &encoder{k: k, p: p, fps: o.FPS, in: make(chan Frame, 2), out: make(chan Unit, 8), done: make(chan struct{})}
	e.wg.Add(2)
	go e.feed()
	go e.read()
	return e, nil
}

func (e *encoder) Push(f Frame) {
	for {
		select {
		case e.in <- f:
			return
		case <-e.done:
			return
		default:
		}
		select { // full: drop the oldest
		case <-e.in:
		default:
		}
	}
}

func (e *encoder) feed() {
	defer e.wg.Done()
	for {
		var f Frame
		select {
		case f = <-e.in:
		case <-e.done:
			return
		}
		caps := fmt.Sprintf("video/x-raw,format=BGRx,width=%d,height=%d,framerate=%d/1", f.Width, f.Height, e.fps)
		if caps == e.inCaps {
			caps = ""
		} else {
			e.inCaps = caps
		}
		pix := f.Pix
		if f.Stride != f.Width*4 { // GStreamer's BGRx rows are width*4 bytes
			pix = make([]byte, f.Width*4*f.Height)
			for y := range f.Height {
				copy(pix[y*f.Width*4:(y+1)*f.Width*4], f.Pix[y*f.Stride:])
			}
		}
		if err := e.p.push(pix, f.PTS, caps); err != nil {
			e.fail(err)
			return
		}
	}
}

func (e *encoder) read() {
	defer e.wg.Done()
	defer close(e.out)
	for {
		select {
		case <-e.done:
			return
		default:
		}
		data, key, eos := e.p.pull(100 * time.Millisecond)
		if err := e.p.err(); err != nil {
			e.fail(fmt.Errorf("%s: %w", e.k.name, err))
			return
		}
		if eos {
			e.fail(errors.New(e.k.name + ": the encoder stopped"))
			return
		}
		if data == nil {
			continue
		}
		select {
		case e.out <- Unit{Data: data, Key: key}:
		case <-e.done:
			return
		}
	}
}

func (e *encoder) fail(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
	go e.Close()
}

func (e *encoder) Units() <-chan Unit { return e.out }

func (e *encoder) SetBitrate(kbps int) {
	prop, v := e.k.bitrate(max(kbps, 100))
	e.p.setUint("enc", prop, v)
}

func (e *encoder) SetSize(w, h int) {
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		return
	}
	e.p.setCaps("size", e.k.caps(w, h))
	e.p.forceKey()
}

func (e *encoder) ForceKeyframe() { e.p.forceKey() }

func (e *encoder) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func (e *encoder) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	close(e.done)
	e.wg.Wait()
	e.p.free()
}
