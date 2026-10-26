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

type macStream struct {
	id     uintptr
	native unsafe.Pointer
	queue  *UnitQueue

	mu     sync.Mutex // guards native, closed and err against the native callbacks and Close
	closed bool
	err    error
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
	s := &macStream{id: nextStream.Add(1)}
	// The keyframe request is asynchronous; safe from the encoder's callback. The queue asks
	// from deliver, which holds mu and has checked closed, so the stream is not freed meanwhile.
	s.queue = NewUnitQueue(func() {
		if s.native != nil {
			C.mob_stream_keyframe(s.native)
		}
	})
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
	s.mu.Lock()
	s.native = native
	s.mu.Unlock()
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

func (s *macStream) Units() <-chan AccessUnit { return s.queue.Units() }

func (s *macStream) SetBitrate(kbps int) {
	s.withNative(func(n unsafe.Pointer) { C.mob_stream_bitrate(n, C.int(max(kbps, 50))) })
}

func (s *macStream) SetSize(width, height int) {
	if width >= 16 && height >= 16 {
		s.withNative(func(n unsafe.Pointer) { C.mob_stream_size(n, C.int(width&^1), C.int(height&^1)) })
	}
}

func (s *macStream) SetFrameRate(fps int) {
	s.withNative(func(n unsafe.Pointer) { C.mob_stream_fps(n, C.int(min(max(fps, 1), 60))) })
}

func (s *macStream) Keyframe() {
	s.withNative(func(n unsafe.Pointer) { C.mob_stream_keyframe(n) })
}

// withNative calls the native stream unless it is closed, holding mu throughout: Close frees the
// native stream after setting closed, so it cannot be freed while a call is on its way. The
// calls only queue work on the stream's queue, so holding mu here never waits for the encoder.
func (s *macStream) withNative(call func(unsafe.Pointer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && s.native != nil {
		call(s.native)
	}
}

func (s *macStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
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
	s.queue.Close()
}

func (s *macStream) deliver(u AccessUnit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.queue.Deliver(u)
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
