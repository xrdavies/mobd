package desktop

/*
#cgo LDFLAGS: -framework AudioToolbox -framework CoreMedia
#include <stdlib.h>
#include "snapshot_darwin.h"
#include "audio_darwin.h"
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

// macAudio shares the system audio capture between live views.
var macAudio = newAudioHub(startMacAudio)

// StartAudio joins the capture of the computer's playback (Opus; see AudioPacket).
func (macOS) StartAudio(ctx context.Context) (AudioStream, error) { return macAudio.StartAudio(ctx) }

type macAudioCapture struct {
	id     uintptr
	native unsafe.Pointer
	ch     chan AudioPacket

	mu     sync.Mutex
	closed bool
	err    error
}

var (
	audios    sync.Map // id -> *macAudioCapture, looked up by the native callbacks
	nextAudio atomic.Uintptr
)

func startMacAudio() (nativeAudio, error) {
	a := &macAudioCapture{id: nextAudio.Add(1), ch: make(chan AudioPacket, audioQueue)}
	audios.Store(a.id, a)
	var rc C.int
	msg := make([]byte, 256)
	native := C.mob_audio_start(C.uintptr_t(a.id), C.double(10), &rc, (*C.char)(unsafe.Pointer(&msg[0])), C.int(len(msg)))
	if native == nil {
		audios.Delete(a.id)
		return nil, audioError(rc, C.GoString((*C.char)(unsafe.Pointer(&msg[0]))))
	}
	a.mu.Lock()
	a.native = native
	a.mu.Unlock()
	return a, nil
}

func audioError(rc C.int, detail string) error {
	switch rc {
	case C.MOB_SNAP_DENIED:
		return errNoScreenRecording
	case C.MOB_SNAP_NO_DISPLAY:
		return unavailable()
	}
	return fmt.Errorf("desktop sound: %s", detail)
}

func (a *macAudioCapture) Packets() <-chan AudioPacket { return a.ch }

func (a *macAudioCapture) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (a *macAudioCapture) Close() {
	audios.Delete(a.id)
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	native := a.native
	a.mu.Unlock()
	if native != nil {
		C.mob_audio_stop(native)
	}
	a.mu.Lock()
	close(a.ch)
	a.mu.Unlock()
}

// deliver drops the oldest packet when Go falls behind.
func (a *macAudioCapture) deliver(p AudioPacket) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	select {
	case a.ch <- p:
	default:
		select {
		case <-a.ch:
		default:
		}
		select {
		case a.ch <- p:
		default:
		}
	}
}

//export mobAudioPacket
func mobAudioPacket(id C.uintptr_t, data unsafe.Pointer, n C.int, ptsUs C.longlong) {
	if v, ok := audios.Load(uintptr(id)); ok {
		v.(*macAudioCapture).deliver(AudioPacket{Data: C.GoBytes(data, n), PTS: time.Duration(ptsUs) * time.Microsecond})
	}
}

// mobAudioEnded runs when ScreenCaptureKit stops on its own (display gone, permission revoked).
//
//export mobAudioEnded
func mobAudioEnded(id C.uintptr_t, rc C.int, msg *C.char) {
	v, ok := audios.Load(uintptr(id))
	if !ok {
		return
	}
	a := v.(*macAudioCapture)
	err := fmt.Errorf("sound ended: %w", audioError(rc, C.GoString(msg)))
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
	go a.Close()
}
