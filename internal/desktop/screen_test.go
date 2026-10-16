package desktop

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestEncodeScalesAndConverts(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := range 400 {
		src.Set(x, 10, color.RGBA{255, 0, 0, 255})
	}
	var capture bytes.Buffer
	_ = png.Encode(&capture, src)

	out, mime, err := Encode(capture.Bytes(), "jpeg", 100, 80)
	if err != nil || mime != "image/jpeg" {
		t.Fatalf("mime=%s err=%v", mime, err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil || img.Bounds().Dx() != 100 || img.Bounds().Dy() != 50 {
		t.Fatalf("scaled bounds %v err=%v", img.Bounds(), err)
	}
	same, mime, _ := Encode(capture.Bytes(), "png", 0, 0)
	if mime != "image/png" || !bytes.Equal(same, capture.Bytes()) {
		t.Fatal("full-size PNG should pass through unchanged")
	}
	if _, _, err := Encode(capture.Bytes(), "bmp", 0, 0); err == nil {
		t.Fatal("unknown format accepted")
	}
}
