//go:build unix

package usb

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/xrdavies/mobd/internal/usbip"
)

// Fake is an importer for tests that acts like the kernel: on Attach it reads the device
// descriptor over the socket, so a test proves URBs cross the tunnel, and then keeps the socket
// until Detach.
type Fake struct {
	NotReady string // Ready's reason; "" means ready
	BusyWith string // Busy's answer

	mu       sync.Mutex
	files    map[int]*os.File
	desc     map[int][]byte
	next     int
	Detached []int
}

func (f *Fake) Backend() string { return "fake" }

func (f *Fake) Ready() (bool, string) { return f.NotReady == "", f.NotReady }

func (f *Fake) Attach(fd int, d usbip.Device) (int, error) {
	dup, err := syscall.Dup(fd)
	if err != nil {
		return 0, err
	}
	file := os.NewFile(uintptr(dup), "fake-vhci")
	// GET_DESCRIPTOR (device), as Linux sends first.
	u := &usbip.URB{Command: usbip.CmdSubmit, SeqNum: 1, DevID: d.DevID(), Direction: usbip.DirIn, BufferLength: 18, NumPackets: -1,
		Setup: [8]byte{0x80, 6, 0, 1, 0, 0, 18, 0}}
	if err := usbip.WriteURB(file, u); err != nil {
		file.Close()
		return 0, err
	}
	r, err := usbip.ReadURB(file, func(uint32) bool { return true })
	if err != nil || r.Status != usbip.StatusOK || len(r.Data) != 18 {
		file.Close()
		return 0, fmt.Errorf("fake vhci: no device descriptor: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files, f.desc = map[int]*os.File{}, map[int][]byte{}
	}
	port := f.next
	f.next++
	f.files[port], f.desc[port] = file, r.Data
	return port, nil
}

// Descriptor is the device descriptor read on port.
func (f *Fake) Descriptor(port int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.desc[port]
}

func (f *Fake) LocalBusID(port int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files[port] == nil {
		return "", nil
	}
	return fmt.Sprintf("9-%d", port+1), nil
}

func (f *Fake) Detach(port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	file := f.files[port]
	if file == nil {
		return errors.New("fake vhci: port not attached")
	}
	delete(f.files, port)
	f.Detached = append(f.Detached, port)
	return file.Close()
}

func (f *Fake) Busy(int) string { return f.BusyWith }
