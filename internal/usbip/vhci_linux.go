package usbip

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// VHCI is Linux's USB/IP importer: the vhci_hcd virtual host controller. Attaching hands it a
// connected socket after Import; the kernel then carries the device's URBs itself, and the device
// appears like any other USB device. Attaching and detaching need root.
type VHCI struct {
	// Dir is the controller in sysfs; empty means /sys/devices/platform/vhci_hcd.0.
	Dir string
}

// ErrNoPort means every virtual port of the needed speed is in use.
var ErrNoPort = errors.New("usbip: no free vhci port")

func (v VHCI) dir() string {
	if v.Dir != "" {
		return v.Dir
	}
	return "/sys/devices/platform/vhci_hcd.0"
}

// Available reports whether the controller exists (the vhci-hcd module is loaded).
func (v VHCI) Available() bool {
	_, err := os.Stat(filepath.Join(v.dir(), "attach"))
	return err == nil
}

// Port is one row of the controller's status file.
type Port struct {
	Hub        string // "hs" (USB 2 and slower) or "ss" (USB 3)
	Number     int
	State      int // 4 (VDEV_ST_NULL) when free
	LocalBusID string
}

const portFree = 4

// Ports reads the controller's status.
func (v VHCI) Ports() ([]Port, error) {
	f, err := os.Open(filepath.Join(v.dir(), "status"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Port
	s := bufio.NewScanner(f)
	for s.Scan() {
		// hub port sta spd dev      sockfd local_busid
		// hs  0000 004 000 00000000 000000 0-0
		f := strings.Fields(s.Text())
		if len(f) < 7 || (f[0] != "hs" && f[0] != "ss") {
			continue
		}
		n, err1 := strconv.Atoi(f[1])
		st, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, Port{Hub: f[0], Number: n, State: st, LocalBusID: f[6]})
	}
	return out, s.Err()
}

func hubFor(speed uint32) string {
	if speed == SpeedSuper {
		return "ss"
	}
	return "hs"
}

// Attach gives the kernel fd, a stream socket on which Import returned d, on a free port. The
// kernel takes its own reference to the socket; the caller may close fd afterwards. It returns the
// port, for Detach.
func (v VHCI) Attach(fd int, d Device) (int, error) {
	ports, err := v.Ports()
	if err != nil {
		return 0, err
	}
	for _, p := range ports {
		if p.Hub != hubFor(d.Speed) || p.State != portFree {
			continue
		}
		line := fmt.Sprintf("%d %d %d %d", p.Number, fd, d.DevID(), d.Speed)
		if err := os.WriteFile(filepath.Join(v.dir(), "attach"), []byte(line), 0); err != nil {
			if errors.Is(err, os.ErrExist) || strings.Contains(err.Error(), "busy") {
				continue // taken since the status was read
			}
			return 0, fmt.Errorf("usbip: attach on port %d: %w", p.Number, err)
		}
		return p.Number, nil
	}
	return 0, ErrNoPort
}

// LocalBusID is where the device on port shows in /sys/bus/usb/devices, once enumerated.
func (v VHCI) LocalBusID(port int) (string, error) {
	ports, err := v.Ports()
	if err != nil {
		return "", err
	}
	for _, p := range ports {
		if p.Number == port && p.State != portFree && p.LocalBusID != "0-0" {
			return p.LocalBusID, nil
		}
	}
	return "", nil
}

// Detach removes the device on port; the kernel closes its socket.
func (v VHCI) Detach(port int) error {
	return os.WriteFile(filepath.Join(v.dir(), "detach"), []byte(strconv.Itoa(port)), 0)
}
