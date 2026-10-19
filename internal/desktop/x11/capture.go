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
	if err := x.alive(); err != nil {
		return nil, err
	}
	setup := xproto.Setup(x.c)
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		return nil, errors.New("X image: most significant byte first is not supported")
	}
	bpp, pad := 0, 0
	for _, f := range setup.PixmapFormats {
		if f.Depth == x.depth {
			bpp, pad = int(f.BitsPerPixel), int(f.ScanlinePad)
		}
	}
	if bpp != 32 {
		return nil, fmt.Errorf("X image: depth %d at %d bits per pixel is not supported", x.depth, bpp)
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
		return nil, ctx.Err()
	case <-x.done:
		return nil, errClosed
	}
	if r.err != nil {
		return nil, fmt.Errorf("X GetImage: %w", r.err)
	}
	if r.r == nil {
		return nil, errClosed
	}
	stride := (x.width*bpp + pad - 1) / pad * pad / 8
	return toRGBA(r.r.Data, x.width, x.height, stride)
}
