package usbip

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The kernel importer end to end: the emulated loopback device attached to vhci_hcd, enumerated by
// Linux, and driven through usbfs like a debug probe's tool would. Opt in on a Linux machine with
// vhci-hcd loaded, as root: MOBD_USBIP_VHCI=1.
func TestVHCIAttachesAnEmulatedDevice(t *testing.T) {
	if os.Getenv("MOBD_USBIP_VHCI") != "1" {
		t.Skip("set MOBD_USBIP_VHCI=1 (root, vhci-hcd loaded)")
	}
	v := VHCI{}
	if !v.Available() {
		t.Fatal("vhci_hcd is not loaded: modprobe vhci-hcd")
	}
	t.Run("tcp", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			if c, err := ln.Accept(); err == nil {
				_ = Serve(c, &Loopback{BusID: "1-1"})
			}
		}()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		f, err := c.(*net.TCPConn).File()
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		exercise(t, v, f)
	})
	// A socket pair: no listening port at all, the way mobd bridges the kernel to its tunnel.
	t.Run("socketpair", func(t *testing.T) {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		server, err := net.FileConn(os.NewFile(uintptr(fds[1]), "exporter"))
		if err != nil {
			t.Fatal(err)
		}
		unix.Close(fds[1])
		go func() { _ = Serve(server, &Loopback{BusID: "1-1"}) }()
		exercise(t, v, os.NewFile(uintptr(fds[0]), "importer"))
	})
}

func exercise(t *testing.T, v VHCI, f *os.File) {
	t.Helper()
	defer f.Close()
	fd := int(f.Fd())
	if err := unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	d, err := Import(f, "1-1")
	if err != nil {
		t.Fatal(err)
	}
	port, err := v.Attach(fd, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("attached on port %d", port)
	detached := false
	defer func() {
		if !detached {
			_ = v.Detach(port)
		}
	}()

	var dev string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if id, _ := v.LocalBusID(port); id != "" {
			if b, err := os.ReadFile(filepath.Join("/sys/bus/usb/devices", id, "idVendor")); err == nil && strings.TrimSpace(string(b)) == "1209" {
				dev = filepath.Join("/sys/bus/usb/devices", id)
				break
			}
		}
	}
	if dev == "" {
		t.Fatal("the device did not enumerate")
	}
	product, _ := os.ReadFile(filepath.Join(dev, "product"))
	t.Logf("enumerated %s: %q", filepath.Base(dev), strings.TrimSpace(string(product)))
	if strings.TrimSpace(string(product)) != "Mob USB/IP loopback" {
		t.Fatalf("product string %q", product)
	}

	busnum, _ := os.ReadFile(filepath.Join(dev, "busnum"))
	devnum, _ := os.ReadFile(filepath.Join(dev, "devnum"))
	var bn, dn int
	fmt.Sscan(strings.TrimSpace(string(busnum)), &bn)
	fmt.Sscan(strings.TrimSpace(string(devnum)), &dn)
	u, err := os.OpenFile(fmt.Sprintf("/dev/bus/usb/%03d/%03d", bn, dn), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	iface := uint32(0)
	if err := ioctl(u, usbdevfsClaimInterface, unsafe.Pointer(&iface)); err != nil {
		t.Fatalf("claim interface: %v", err)
	}

	// 256 KiB in 64 KiB chunks, read back and compared.
	sent := make([]byte, 256<<10)
	_, _ = rand.Read(sent)
	for off := 0; off < len(sent); off += 64 << 10 {
		if n, err := bulk(u, 0x01, sent[off:off+64<<10]); err != nil || n != 64<<10 {
			t.Fatalf("bulk out at %d: %d %v", off, n, err)
		}
	}
	var got []byte
	buf := make([]byte, 64<<10)
	for len(got) < len(sent) {
		n, err := bulk(u, 0x81, buf)
		if err != nil {
			t.Fatalf("bulk in after %d bytes: %v", len(got), err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, sent) {
		t.Fatal("bytes read back differ")
	}
	t.Logf("%d bytes out and back over USB/IP", len(sent))

	u.Close()
	if err := v.Detach(port); err != nil {
		t.Fatal(err)
	}
	detached = true
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if id, _ := v.LocalBusID(port); id == "" {
			return
		}
	}
	t.Fatal("the port did not become free after detach")
}

// usbfs, as tools such as openocd and dfu-util use it (linux/usbdevice_fs.h).
const (
	usbdevfsBulk           = 0xc0185502 // _IOWR('U', 2, struct usbdevfs_bulktransfer), 64-bit
	usbdevfsClaimInterface = 0x8004550f // _IOR('U', 15, unsigned int)
)

type bulkTransfer struct {
	Endpoint, Length, Timeout uint32
	_                         uint32
	Data                      uintptr
}

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func bulk(f *os.File, ep uint32, b []byte) (int, error) {
	bt := bulkTransfer{Endpoint: ep, Length: uint32(len(b)), Timeout: 5000, Data: uintptr(unsafe.Pointer(&b[0]))}
	n, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), usbdevfsBulk, uintptr(unsafe.Pointer(&bt)))
	if e != 0 {
		return 0, e
	}
	return int(n), nil
}
