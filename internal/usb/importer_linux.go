package usb

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xrdavies/mobd/internal/usbip"
)

// NewImporter returns Linux's importer: vhci_hcd directly when mobd runs as root, else through the
// root helper (`sudo mobd usb setup`), which attaches and detaches only for this user.
func NewImporter() Importer {
	v := usbip.VHCI{}
	if os.Geteuid() == 0 {
		return vhciImporter{v: v}
	}
	return &helperImporter{v: v, socket: HelperSocket(os.Getuid())}
}

const setupHint = "run `sudo mobd usb setup` once on this computer"

// vhciImporter attaches directly; mobd is root.
type vhciImporter struct{ v usbip.VHCI }

func (i vhciImporter) Backend() string { return "vhci" }

func (i vhciImporter) Ready() (bool, string) {
	if !i.v.Available() {
		return false, "the vhci-hcd kernel module is not loaded: " + setupHint
	}
	return true, ""
}

func (i vhciImporter) Attach(fd int, d usbip.Device) (int, error) { return i.v.Attach(fd, d) }
func (i vhciImporter) LocalBusID(port int) (string, error)        { return i.v.LocalBusID(port) }
func (i vhciImporter) Detach(port int) error                      { return i.v.Detach(port) }
func (i vhciImporter) Busy(port int) string                       { return mounted(i.v, port) }

// helperImporter attaches through the root helper's socket.
type helperImporter struct {
	v      usbip.VHCI
	socket string
}

func (i *helperImporter) Backend() string { return "vhci" }

func (i *helperImporter) Ready() (bool, string) {
	if !i.v.Available() {
		return false, "the vhci-hcd kernel module is not loaded: " + setupHint
	}
	if _, err := os.Stat(i.socket); err != nil {
		return false, "the USB helper is not running: " + setupHint
	}
	return true, ""
}

func (i *helperImporter) Attach(fd int, d usbip.Device) (int, error) {
	r, err := callHelper(i.socket, helperRequest{Op: "attach", DevID: d.DevID(), Speed: d.Speed}, fd)
	if err != nil {
		return 0, err
	}
	return r.Port, nil
}

func (i *helperImporter) LocalBusID(port int) (string, error) { return i.v.LocalBusID(port) }

func (i *helperImporter) Detach(port int) error {
	_, err := callHelper(i.socket, helperRequest{Op: "detach", Port: port}, -1)
	return err
}

func (i *helperImporter) Busy(port int) string { return mounted(i.v, port) }

// mounted names a filesystem of the device on port that is still mounted: detaching it now could
// lose writes.
func mounted(v usbip.VHCI, port int) string {
	id, _ := v.LocalBusID(port)
	if id == "" {
		return ""
	}
	// The device's disks are under .../block/sdb, their partitions under .../block/sdb/sdb1. The
	// device path is a symlink, which WalkDir does not follow.
	root, err := filepath.EvalSymlinks(filepath.Join("/sys/bus/usb/devices", id))
	if err != nil {
		return ""
	}
	blocks := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil || !e.IsDir() {
			return nil
		}
		parent := filepath.Base(filepath.Dir(p))
		if parent == "block" || (blocks[parent] && strings.HasPrefix(e.Name(), parent)) {
			blocks[e.Name()] = true
		}
		return nil
	})
	if len(blocks) == 0 {
		return ""
	}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && blocks[strings.TrimPrefix(fields[0], "/dev/")] {
			return fmt.Sprintf("%s is mounted at %s; unmount it first", fields[0], fields[1])
		}
	}
	return ""
}
