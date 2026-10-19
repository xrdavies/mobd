//go:build linux

package x11

import (
	"context"
	"errors"
	"fmt"
	"image"

	"github.com/jezek/xgb/xproto"
)

// Capture reads the whole screen. Xvfb draws the pointer in software and GetImage leaves it
// out, so frames have no cursor.
func (x *X11) Capture(ctx context.Context) (*image.RGBA, error) {
	pix, stride, err := x.captureRaw(ctx)
	if err != nil {
		return nil, err
	}
	return toRGBA(pix, x.width, x.height, stride)
}

// captureRaw reads the screen as the server keeps it: BGRX rows of stride bytes.
func (x *X11) captureRaw(ctx context.Context) ([]byte, int, error) {
	if err := x.alive(); err != nil {
		return nil, 0, err
	}
	setup := xproto.Setup(x.c)
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		return nil, 0, errors.New("X image: most significant byte first is not supported")
	}
	bpp, pad := 0, 0
	for _, f := range setup.PixmapFormats {
		if f.Depth == x.depth {
			bpp, pad = int(f.BitsPerPixel), int(f.ScanlinePad)
		}
	}
	if bpp != 32 {
		return nil, 0, fmt.Errorf("X image: depth %d at %d bits per pixel is not supported", x.depth, bpp)
	}
	type result struct {
		r   *xproto.GetImageReply
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := xproto.GetImage(x.c, xproto.ImageFormatZPixmap, xproto.Drawable(x.root),
			0, 0, uint16(x.width), uint16(x.height), 0xffffffff).Reply()
		ch <- result{r, err}
	}()
	var r result
	select {
	case r = <-ch:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case <-x.done:
		return nil, 0, errClosed
	}
	if r.err != nil {
		return nil, 0, fmt.Errorf("X GetImage: %w", r.err)
	}
	if r.r == nil {
		return nil, 0, errClosed
	}
	stride := (x.width*bpp + pad - 1) / pad * pad / 8
	if len(r.r.Data) < stride*x.height {
		return nil, 0, fmt.Errorf("X image: %d bytes for %dx%d", len(r.r.Data), x.width, x.height)
	}
	return r.r.Data, stride, nil
}
