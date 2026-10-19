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
var ErrUnsupported = errors.New("desktop actuation is not supported on this system yet")

// Encode converts a PNG capture to the requested format, downscaling to
// maxWidth (0 keeps the original size). It returns the bytes and MIME type.
func Encode(capture []byte, format string, maxWidth, quality int) ([]byte, string, error) {
	if format == "png" && maxWidth == 0 {
		return capture, "image/png", nil
	}
	img, err := png.Decode(bytes.NewReader(capture))
	if err != nil {
		return nil, "", fmt.Errorf("decode capture: %w", err)
	}
	if b := img.Bounds(); format == "png" && b.Dx() <= maxWidth {
		return capture, "image/png", nil
	}
	out, err := EncodeImage(img, format, maxWidth, quality)
	return out.Data, out.MIME, err
}

// EncodeImage encodes a captured frame as JPEG or PNG, downscaled to maxWidth (0 keeps its size).
func EncodeImage(img image.Image, format string, maxWidth, quality int) (Image, error) {
	if b := img.Bounds(); maxWidth > 0 && b.Dx() > maxWidth {
		h := max(1, b.Dy()*maxWidth/b.Dx())
		dst := image.NewRGBA(image.Rect(0, 0, maxWidth, h))
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
		img = dst
	}
	var buf bytes.Buffer
	out := Image{Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}
	var err error
	switch format {
	case "png":
		err, out.MIME = png.Encode(&buf, img), "image/png"
	case "jpeg":
		err, out.MIME = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}), "image/jpeg"
	default:
		return Image{}, fmt.Errorf("unsupported format %q", format)
	}
	out.Data = buf.Bytes()
	return out, err
}
