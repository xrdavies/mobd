//go:build linux && cgo

package gst

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0 gstreamer-video-1.0
#include <stdlib.h>
#include "gst_linux.h"
*/
import "C"

import (
	"errors"
	"sync"
	"time"
	"unsafe"
)

var initOnce = sync.OnceValue(func() error {
	if C.mob_gst_init() != 0 {
		return errors.New("GStreamer did not initialize")
	}
	return nil
})

func has(factory string) bool {
	if initOnce() != nil {
		return false
	}
	c := C.CString(factory)
	defer C.free(unsafe.Pointer(c))
	return C.mob_gst_has(c) == 1
}

// pipe is a running pipeline with an appsrc "src" and an appsink "sink".
type pipe struct{ p *C.MobPipe }

func newPipe(desc string) (*pipe, error) {
	if err := initOnce(); err != nil {
		return nil, err
	}
	c := C.CString(desc)
	defer C.free(unsafe.Pointer(c))
	var cerr *C.char
	p := C.mob_pipe_new(c, &cerr)
	if p == nil {
		defer C.mob_free(unsafe.Pointer(cerr))
		return nil, errors.New(C.GoString(cerr))
	}
	return &pipe{p}, nil
}

// push copies data into the appsrc; caps "" keeps the current caps.
func (p *pipe) push(data []byte, pts time.Duration, caps string) error {
	var ccaps *C.char
	if caps != "" {
		ccaps = C.CString(caps)
		defer C.free(unsafe.Pointer(ccaps))
	}
	if len(data) == 0 {
		return errors.New("empty buffer")
	}
	if C.mob_pipe_push(p.p, unsafe.Pointer(&data[0]), C.int(len(data)), C.int64_t(pts), ccaps) != 0 {
		return errors.New("the pipeline refused a buffer")
	}
	return nil
}

// pull waits up to timeout for an output buffer; eos when the pipeline ended.
func (p *pipe) pull(timeout time.Duration) (data []byte, key, eos bool) {
	var ptr unsafe.Pointer
	var size, k C.int
	switch C.mob_pipe_pull(p.p, C.int64_t(timeout), &ptr, &size, &k) {
	case 1:
		defer C.mob_free(ptr)
		return C.GoBytes(ptr, size), k != 0, false
	case -1:
		return nil, false, true
	}
	return nil, false, false
}

func (p *pipe) err() error {
	c := C.mob_pipe_error(p.p)
	if c == nil {
		return nil
	}
	defer C.mob_free(unsafe.Pointer(c))
	return errors.New(C.GoString(c))
}

func (p *pipe) setUint(element, prop string, v uint) {
	e, n := C.CString(element), C.CString(prop)
	defer C.free(unsafe.Pointer(e))
	defer C.free(unsafe.Pointer(n))
	C.mob_pipe_set_uint(p.p, e, n, C.uint(v))
}

func (p *pipe) setCaps(element, caps string) {
	e, c := C.CString(element), C.CString(caps)
	defer C.free(unsafe.Pointer(e))
	defer C.free(unsafe.Pointer(c))
	C.mob_pipe_set_caps(p.p, e, c)
}

func (p *pipe) forceKey() { C.mob_pipe_force_key(p.p) }
func (p *pipe) eos()      { C.mob_pipe_eos(p.p) }
func (p *pipe) free()     { C.mob_pipe_free(p.p) }
