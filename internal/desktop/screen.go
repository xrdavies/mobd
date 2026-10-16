// Package desktop captures and drives the office computer's desktop through one backend per OS.
package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
)

// ErrUnsupported reports desktop access on a non-macOS host.
var ErrUnsupported = errors.New("desktop actuation is only supported on macOS")

// Encode converts a PNG capture to the requested format, downscaling to
// maxWidth (0 keeps the original size). It returns the bytes and MIME type.
func Encode(capture []byte, format string, maxWidth, quality int) ([]byte, string, error) {
	img, err := png.Decode(bytes.NewReader(capture))
	if err != nil {
		return nil, "", fmt.Errorf("decode capture: %w", err)
	}
	if b := img.Bounds(); maxWidth > 0 && b.Dx() > maxWidth {
		h := max(1, b.Dy()*maxWidth/b.Dx())
		dst := image.NewRGBA(image.Rect(0, 0, maxWidth, h))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
		img = dst
	} else if format == "png" {
		return capture, "image/png", nil
	}
	var buf bytes.Buffer
	switch format {
	case "png":
		err = png.Encode(&buf, img)
		return buf.Bytes(), "image/png", err
	case "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
		return buf.Bytes(), "image/jpeg", err
	}
	return nil, "", fmt.Errorf("unsupported format %q", format)
}
