package desktop

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
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

func TestAgentScreenshotIsPointSizedAndCapped(t *testing.T) {
	if w := screenshotWidth(Display{Width: 1512, Height: 982, Scale: 2}); w != 1512 {
		t.Fatalf("MacBook: %d", w)
	}
	if w := screenshotWidth(Display{Width: 2560, Height: 1440, Scale: 2}); w != agentMaxWidth {
		t.Fatalf("5K display: %d", w)
	}
	if w := screenshotWidth(Display{}); w != agentMaxWidth {
		t.Fatalf("unknown display: %d", w)
	}
	m := screenMeta(Display{Width: 1512, Height: 982, Scale: 2}, Image{Width: 1512, Height: 982})
	if m.Format != "jpeg" || m.PixelWidth != 3024 || m.ImageWidth != 1512 || !strings.Contains(m.Hint, "as they are") {
		t.Fatalf("point-sized meta %+v", m)
	}
	m = screenMeta(Display{Width: 2560, Height: 1440, Scale: 2}, Image{Width: 1568, Height: 882})
	if m.PointWidth != 2560 || m.ImageWidth != 1568 || !strings.Contains(m.Hint, "1.633") {
		t.Fatalf("scaled meta %+v", m)
	}
}
