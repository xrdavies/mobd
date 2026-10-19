package x11

import (
	"fmt"
	"image"
)

// toRGBA converts a 32-bit ZPixmap (BGRX in memory, least significant byte first) to RGBA.
// stride is the bytes per row the server sent.
func toRGBA(pix []byte, width, height, stride int) (*image.RGBA, error) {
	if stride < width*4 || len(pix) < stride*(height-1)+width*4 {
		return nil, fmt.Errorf("X image: %d bytes for %dx%d (stride %d)", len(pix), width, height, stride)
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		src, dst := pix[y*stride:y*stride+width*4], img.Pix[y*img.Stride:y*img.Stride+width*4]
		for i := 0; i < len(src); i += 4 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 0xff
		}
	}
	return img, nil
}
