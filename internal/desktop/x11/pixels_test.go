package x11

import "testing"

func TestToRGBA(t *testing.T) {
	// Two pixels a row (8 bytes) padded to a 12-byte stride: blue, then red; green, then white.
	pix := []byte{
		0xff, 0, 0, 0, 0, 0, 0xff, 0, 9, 9, 9, 9,
		0, 0xff, 0, 0, 0xff, 0xff, 0xff, 0,
	}
	img, err := toRGBA(pix, 2, 2, 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		x, y       int
		r, g, b, a uint8
	}{{0, 0, 0, 0, 0xff, 0xff}, {1, 0, 0xff, 0, 0, 0xff}, {0, 1, 0, 0xff, 0, 0xff}, {1, 1, 0xff, 0xff, 0xff, 0xff}} {
		got := img.RGBAAt(c.x, c.y)
		if got.R != c.r || got.G != c.g || got.B != c.b || got.A != c.a {
			t.Errorf("(%d,%d) = %v", c.x, c.y, got)
		}
	}
	if _, err := toRGBA(pix[:10], 2, 2, 12); err == nil {
		t.Error("short image accepted")
	}
}
