package desktop

/*
#cgo LDFLAGS: -framework Foundation -framework ScreenCaptureKit -framework ImageIO -framework CoreGraphics
#include <stdlib.h>
#include "snapshot_darwin.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"time"
	"unsafe"
)

var errNoScreenRecording = errors.New("screen capture failed; grant Screen Recording to the app that launches mobd " +
	"(System Settings > Privacy & Security > Screen & System Audio Recording)")

// Snapshot captures with ScreenCaptureKit at the requested size, so a 4K display is never
// encoded at full size for a small view. macOS 13 falls back to screencapture(1).
func (macOS) Snapshot(ctx context.Context, opts SnapshotOptions) (Image, error) {
	timeout := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return Image{}, context.DeadlineExceeded
	}
	quality := 0
	if opts.Format == "jpeg" {
		quality = min(max(opts.Quality, 1), 100)
	}
	var out unsafe.Pointer
	var n C.long
	var w, h C.int
	msg := make([]byte, 256)
	rc := C.mob_snapshot(C.int(opts.MaxWidth), C.int(quality), C.double(timeout.Seconds()),
		&out, &n, &w, &h, (*C.char)(unsafe.Pointer(&msg[0])), C.int(len(msg)))
	detail := C.GoString((*C.char)(unsafe.Pointer(&msg[0])))
	switch rc {
	case C.MOB_SNAP_OK:
		defer C.free(out)
		return Image{Data: C.GoBytes(out, C.int(n)), MIME: "image/" + opts.Format, Width: int(w), Height: int(h)}, nil
	case C.MOB_SNAP_UNAVAILABLE:
		return screencaptureSnapshot(ctx, opts)
	case C.MOB_SNAP_DENIED:
		return Image{}, errNoScreenRecording
	case C.MOB_SNAP_NO_DISPLAY:
		return Image{}, unavailable()
	case C.MOB_SNAP_TIMEOUT:
		return Image{}, fmt.Errorf("screen capture timed out after %s", timeout.Round(time.Millisecond))
	}
	return Image{}, fmt.Errorf("screen capture failed: %s", detail)
}

func screencaptureSnapshot(ctx context.Context, opts SnapshotOptions) (Image, error) {
	capture, err := screencapturePNG(ctx)
	if err != nil {
		return Image{}, err
	}
	data, mime, err := Encode(capture, opts.Format, opts.MaxWidth, opts.Quality)
	if err != nil {
		return Image{}, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(capture))
	if err != nil {
		return Image{}, fmt.Errorf("decode capture: %w", err)
	}
	width, height := cfg.Width, cfg.Height
	if opts.MaxWidth > 0 && width > opts.MaxWidth {
		height, width = max(1, height*opts.MaxWidth/width), opts.MaxWidth
	}
	return Image{Data: data, MIME: mime, Width: width, Height: height}, nil
}

// State reads the console lock flag and the main display's sleep state.
func (macOS) State() (DisplayState, error) {
	var locked, asleep C.int
	C.mob_display_state(&locked, &asleep)
	return DisplayState{Locked: locked != 0, Asleep: asleep != 0}, nil
}
