//go:build linux

package x11

import (
	"encoding/binary"
	"slices"
	"strings"

	"github.com/jezek/xgb/xproto"
	"github.com/xrdavies/mobd/internal/desktop"
)

// property reads a window property (up to 64 KB).
func (x *X11) property(w xproto.Window, name string) (*xproto.GetPropertyReply, error) {
	atom, err := x.intern(name)
	if err != nil {
		return nil, err
	}
	return xproto.GetProperty(x.c, false, w, atom, xproto.GetPropertyTypeAny, 0, 1<<14).Reply()
}

func (x *X11) windowsProp(w xproto.Window, name string) []xproto.Window {
	p, err := x.property(w, name)
	if err != nil || p.Format != 32 {
		return nil
	}
	var out []xproto.Window
	for i := 0; i+4 <= len(p.Value); i += 4 {
		out = append(out, xproto.Window(binary.LittleEndian.Uint32(p.Value[i:])))
	}
	return out
}

func (x *X11) cardinals(w xproto.Window, name string) []uint32 {
	p, err := x.property(w, name)
	if err != nil || p.Format != 32 {
		return nil
	}
	var out []uint32
	for i := 0; i+4 <= len(p.Value); i += 4 {
		out = append(out, binary.LittleEndian.Uint32(p.Value[i:]))
	}
	return out
}

func (x *X11) text(w xproto.Window, name string) string {
	p, err := x.property(w, name)
	if err != nil || p.Format != 8 {
		return ""
	}
	return string(p.Value)
}

// WindowManager reports whether a window manager runs (it set _NET_SUPPORTING_WM_CHECK).
func (x *X11) WindowManager() bool {
	return x.alive() == nil && len(x.windowsProp(x.root, "_NET_SUPPORTING_WM_CHECK")) == 1
}

// ListWindows lists the application windows top first: the window manager's
// _NET_CLIENT_LIST_STACKING (openbox), else the root's mapped children. Bounds include the
// frame, in points.
func (b *Backend) ListWindows(onScreenOnly bool) ([]desktop.Window, error) {
	if err := b.alive(); err != nil {
		return nil, err
	}
	wins := b.windowsProp(b.root, "_NET_CLIENT_LIST_STACKING")
	if wins == nil {
		wins = b.windowsProp(b.root, "_NET_CLIENT_LIST")
	}
	if wins == nil {
		tree, err := xproto.QueryTree(b.c, b.root).Reply()
		if err != nil {
			return nil, err
		}
		wins = tree.Children
	}
	hidden, _ := b.intern("_NET_WM_STATE_HIDDEN")
	var out []desktop.Window
	for _, w := range slices.Backward(wins) {
		attrs, err := xproto.GetWindowAttributes(b.c, w).Reply()
		if err != nil || attrs.OverrideRedirect || attrs.Class == xproto.WindowClassInputOnly {
			continue
		}
		viewable := attrs.MapState == xproto.MapStateViewable
		if slices.Contains(b.cardinals(w, "_NET_WM_STATE"), uint32(hidden)) {
			viewable = false
		}
		if onScreenOnly && !viewable {
			continue
		}
		title := b.text(w, "_NET_WM_NAME")
		if title == "" {
			title = b.text(w, "WM_NAME")
		}
		class := strings.Split(strings.TrimRight(b.text(w, "WM_CLASS"), "\x00"), "\x00")
		if title == "" && class[0] == "" {
			continue // not an application window
		}
		win := desktop.Window{ID: int(w), Title: title, App: class[len(class)-1]}
		if pid := b.cardinals(w, "_NET_WM_PID"); len(pid) == 1 {
			win.PID = int(pid[0])
		}
		geo, err := xproto.GetGeometry(b.c, xproto.Drawable(w)).Reply()
		if err != nil {
			continue
		}
		at, err := xproto.TranslateCoordinates(b.c, w, b.root, 0, 0).Reply()
		if err != nil {
			continue
		}
		x0, y0, wd, ht := float64(at.DstX), float64(at.DstY), float64(geo.Width), float64(geo.Height)
		if ext := b.cardinals(w, "_NET_FRAME_EXTENTS"); len(ext) == 4 { // left, right, top, bottom
			x0, y0 = x0-float64(ext[0]), y0-float64(ext[2])
			wd, ht = wd+float64(ext[0]+ext[1]), ht+float64(ext[2]+ext[3])
		} else if bw := float64(geo.BorderWidth); bw > 0 { // no frame: the window's own border
			x0, y0, wd, ht = x0-bw, y0-bw, wd+2*bw, ht+2*bw
		}
		s := float64(b.scale)
		win.Bounds = desktop.Bounds{X: x0 / s, Y: y0 / s, Width: wd / s, Height: ht / s}
		out = append(out, win)
	}
	return out, nil
}
