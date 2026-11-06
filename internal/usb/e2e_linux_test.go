package usb

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/xrdavies/mobd/internal/usbip"
)

// The production path on Linux: the manager imports an emulated device over a tunnel, the root
// helper attaches it, Linux enumerates it and usbfs moves data through it; release detaches it.
// Opt in after `sudo mobd usb setup`, as the user (not root): MOBD_USB_E2E=1.
func TestEndToEndThroughTheHelper(t *testing.T) {
	if os.Getenv("MOBD_USB_E2E") != "1" {
		t.Skip("set MOBD_USB_E2E=1 after `sudo mobd usb setup`")
	}
	imp := NewImporter()
	if ok, why := imp.Ready(); !ok {
		t.Fatal(why)
	}
	m := New(imp, nil)
	defer m.Close()
	s, err := m.Create("e2e", Device{BusID: "1-1", Name: "loopback"})
	if err != nil {
		t.Fatal(err)
	}
	go m.Serve(context.Background(), "e2e", s.ID, newTunnel(t, &usbip.Loopback{BusID: "1-1"}))
	got := waitState(t, m, "e2e", s.ID, StateConnected)
	t.Logf("attached as %s", got.LocalBusID)

	dev := filepath.Join("/sys/bus/usb/devices", got.LocalBusID)
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dev, name))
		return strings.TrimSpace(string(b))
	}
	if read("idVendor") != "1209" || read("product") != "Mob USB/IP loopback" {
		t.Fatalf("enumerated as %s %q", read("idVendor"), read("product"))
	}
	var bn, dn int
	fmt.Sscan(read("busnum"), &bn)
	fmt.Sscan(read("devnum"), &dn)
	// The device node is root's: the e2e user is in plugdev, as udev rules for tools grant.
	f, err := os.OpenFile(fmt.Sprintf("/dev/bus/usb/%03d/%03d", bn, dn), os.O_RDWR, 0)
	if err != nil {
		t.Skipf("enumerated, but this user cannot open the device node (%v); give it access with a udev rule", err)
	}
	defer f.Close()
	iface := uint32(0)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x8004550f, uintptr(unsafe.Pointer(&iface))); e != 0 {
		t.Fatalf("claim: %v", e)
	}
	bulk := func(ep uint32, b []byte) (int, error) {
		bt := struct {
			Ep, Len, Timeout, _ uint32
			Data                uintptr
		}{ep, uint32(len(b)), 5000, 0, uintptr(unsafe.Pointer(&b[0]))}
		n, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0xc0185502, uintptr(unsafe.Pointer(&bt)))
		if e != 0 {
			return 0, e
		}
		return int(n), nil
	}
	sent := make([]byte, 64<<10)
	_, _ = rand.Read(sent)
	if n, err := bulk(0x01, sent); err != nil || n != len(sent) {
		t.Fatalf("out %d %v", n, err)
	}
	back := make([]byte, len(sent))
	for off := 0; off < len(sent); {
		n, err := bulk(0x81, back[off:])
		if err != nil {
			t.Fatalf("in %v", err)
		}
		off += n
	}
	if !bytes.Equal(back, sent) {
		t.Fatal("data differs")
	}
	f.Close()

	if r, err := m.Release("e2e", s.ID, false); err != nil || r.State != StateDisconnected {
		t.Fatalf("release %+v %v", r, err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(dev); err == nil {
		t.Fatal("the device is still present after release")
	}
}
