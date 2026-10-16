package desktop

/*
#cgo LDFLAGS: -framework VideoToolbox -framework CoreMedia -framework CoreVideo
#include <stdlib.h>
#include "snapshot_darwin.h"
#include "stream_darwin.h"
*/
import "C"

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// unitBuffer holds about half a second at 30 fps; a slower consumer loses frames, not latency.
const unitBuffer = 16

type macStream struct {
	id     uintptr
	native unsafe.Pointer
	units  chan AccessUnit

	mu      sync.Mutex // guards closed and err against the native callbacks
	closed  bool
	err     error
	needKey atomic.Bool // a unit was dropped: discard deltas until the next keyframe
}

var (
	streams    sync.Map // id -> *macStream, looked up by the native callbacks
	nextStream atomic.Uintptr
)

// StartStream starts ScreenCaptureKit capture feeding a VideoToolbox H.264 encoder.
func (macOS) StartStream(ctx context.Context, opts StreamOptions) (Stream, error) {
	timeout := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	s := &macStream{id: nextStream.Add(1), units: make(chan AccessUnit, unitBuffer)}
	streams.Store(s.id, s)
	cursor := 0
	if opts.ShowCursor {
		cursor = 1
	}
	var rc C.int
	msg := make([]byte, 256)
	native := C.mob_stream_start(C.uintptr_t(s.id), C.int(opts.Width), C.int(opts.Height), C.int(opts.FPS), C.int(opts.Kbps),
		C.int(cursor), C.double(timeout.Seconds()), &rc, (*C.char)(unsafe.Pointer(&msg[0])), C.int(len(msg)))
	if native == nil {
		streams.Delete(s.id)
		return nil, streamError(rc, C.GoString((*C.char)(unsafe.Pointer(&msg[0]))))
	}
	s.native = native
	return s, nil
}

func streamError(rc C.int, detail string) error {
	switch rc {
	case C.MOB_SNAP_DENIED:
		return errNoScreenRecording
	case C.MOB_SNAP_NO_DISPLAY:
		return unavailable()
	case C.MOB_SNAP_TIMEOUT:
		return fmt.Errorf("screen capture: %s", detail)
	}
	return fmt.Errorf("screen stream failed: %s", detail)
}

func (s *macStream) Units() <-chan AccessUnit { return s.units }

func (s *macStream) SetBitrate(kbps int) {
	if s.live() {
		C.mob_stream_bitrate(s.native, C.int(max(kbps, 50)))
	}
}

func (s *macStream) SetSize(width, height int) {
	if s.live() && width >= 16 && height >= 16 {
		C.mob_stream_size(s.native, C.int(width&^1), C.int(height&^1))
	}
}

func (s *macStream) Keyframe() {
	if s.live() {
		C.mob_stream_keyframe(s.native)
	}
}

func (s *macStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *macStream) live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// Close stops capture and the encoder; Units is closed once it returns.
func (s *macStream) Close() {
	streams.Delete(s.id)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	C.mob_stream_stop(s.native)
	close(s.units)
}

func (s *macStream) deliver(u AccessUnit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.needKey.Load() && !u.Key {
		return
	}
	select {
	case s.units <- u:
		if u.Key {
			s.needKey.Store(false)
		}
	default:
		if !s.needKey.Swap(true) {
			C.mob_stream_keyframe(s.native) // asynchronous; safe from the encoder's callback
		}
	}
}

//export mobStreamFrame
func mobStreamFrame(id C.uintptr_t, data unsafe.Pointer, n C.int, key C.int, width C.int, height C.int, ptsUs C.longlong) {
	v, ok := streams.Load(uintptr(id))
	if !ok {
		return
	}
	v.(*macStream).deliver(AccessUnit{
		Data: C.GoBytes(data, n), Key: key != 0, PTS: time.Duration(ptsUs) * time.Microsecond,
		Width: int(width), Height: int(height),
	})
}

// mobStreamEnded runs when ScreenCaptureKit stops on its own (display gone, screen locked, permission revoked).
//
//export mobStreamEnded
func mobStreamEnded(id C.uintptr_t, rc C.int, msg *C.char) {
	v, ok := streams.Load(uintptr(id))
	if !ok {
		return
	}
	s := v.(*macStream)
	err := fmt.Errorf("stream ended: %w", streamError(rc, C.GoString(msg)))
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	// The native stream is freed by Close; the owner sees Units close only then, so close it here
	// from a goroutine to keep the native queue unblocked.
	go s.Close()
}
