package usbip

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"unicode/utf16"
)

// An emulated device answers an imported device's URBs in software: the exporter side of tests,
// where no hardware or kernel exporter is available. See Serve.
type EmulatedDevice interface {
	// Info is the device as the exporter lists it.
	Info() Device
	// Control answers a control request on endpoint 0: the data for IN, and a status.
	Control(setup Setup, out []byte) (in []byte, status int32)
	// Transfer moves data on a bulk or interrupt endpoint. IN may wait for data until ctx ends
	// (the URB was unlinked or the connection closed).
	Transfer(ctx context.Context, ep uint32, dir uint32, out []byte, length int) (in []byte, status int32)
}

// Setup is a control request's setup packet.
type Setup struct {
	RequestType, Request uint8
	Value, Index, Length uint16
}

func parseSetup(b [8]byte) Setup {
	return Setup{b[0], b[1], binary.LittleEndian.Uint16(b[2:]), binary.LittleEndian.Uint16(b[4:]), binary.LittleEndian.Uint16(b[6:])}
}

// Serve answers one connection for d: the import operation, then its URBs until rw closes.
func Serve(rw io.ReadWriteCloser, d EmulatedDevice) error {
	defer rw.Close()
	if _, ok, err := ServeOp(rw, []Device{d.Info()}); err != nil || !ok {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		wmu     sync.Mutex // one message at a time on rw
		mu      sync.Mutex
		pending = map[uint32]context.CancelFunc{}
		wg      sync.WaitGroup
	)
	defer wg.Wait()
	write := func(u *URB) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = WriteURB(rw, u)
	}
	for {
		u, err := ReadURB(rw, nil)
		if err != nil {
			return nil // the importer closed the connection
		}
		switch u.Command {
		case CmdSubmit:
			uctx, ucancel := context.WithCancel(ctx)
			mu.Lock()
			pending[u.SeqNum] = ucancel
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer ucancel()
				in, status := answer(uctx, d, u)
				mu.Lock()
				_, still := pending[u.SeqNum]
				delete(pending, u.SeqNum)
				mu.Unlock()
				if !still {
					return // unlinked: RET_UNLINK already answered it
				}
				r := &URB{Command: RetSubmit, SeqNum: u.SeqNum, Status: status, NumPackets: -1}
				if u.Direction == DirIn {
					r.Data, r.BufferLength = in, int32(len(in))
				} else if status == StatusOK {
					r.BufferLength = u.BufferLength
				}
				write(r)
			}()
		case CmdUnlink:
			mu.Lock()
			c, still := pending[u.UnlinkSeqNum]
			delete(pending, u.UnlinkSeqNum)
			mu.Unlock()
			status := int32(StatusOK) // already completed: nothing to unlink
			if still {
				c()
				status = StatusUnlinked
			}
			write(&URB{Command: RetUnlink, SeqNum: u.SeqNum, Status: status})
		}
	}
}

func answer(ctx context.Context, d EmulatedDevice, u *URB) ([]byte, int32) {
	if u.Endpoint == 0 {
		s := parseSetup(u.Setup)
		in, status := d.Control(s, u.Data)
		if len(in) > int(s.Length) {
			in = in[:s.Length]
		}
		if len(in) > int(u.BufferLength) {
			in = in[:u.BufferLength]
		}
		return in, status
	}
	return d.Transfer(ctx, u.Endpoint, u.Direction, u.Data, int(u.BufferLength))
}

// Loopback is a high-speed vendor-class device that returns on bulk IN (0x81) what it received on
// bulk OUT (0x01). It needs no class driver: tools reach it through usbfs, like a debug probe.
type Loopback struct {
	BusID string
	mu    sync.Mutex
	buf   []byte
	ready chan struct{} // closed and replaced whenever data arrives
}

// LoopbackVendor and LoopbackProduct are pid.codes' test IDs, meant for exactly this.
const (
	LoopbackVendor  = 0x1209
	LoopbackProduct = 0x0001
	loopbackLimit   = 1 << 20
)

func (l *Loopback) Info() Device {
	return Device{Path: "/mob/emulated/loopback", BusID: l.BusID, BusNum: 1, DevNum: 2, Speed: SpeedHigh,
		VendorID: LoopbackVendor, ProductID: LoopbackProduct, BCDDevice: 0x0100, ConfigurationValue: 1, NumConfigurations: 1,
		NumInterfaces: 1, Interfaces: []Interface{{0xff, 0, 0}}}
}

var loopbackDevice = []byte{18, 1, 0x00, 0x02, 0, 0, 0, 64, 0x09, 0x12, 0x01, 0x00, 0x00, 0x01, 1, 2, 3, 1}

var loopbackConfig = []byte{
	9, 2, 32, 0, 1, 1, 0, 0x80, 50, // configuration: 32 bytes in all, one interface
	9, 4, 0, 0, 2, 0xff, 0, 0, 0, // interface 0: vendor class, two endpoints
	7, 5, 0x01, 0x02, 0x00, 0x02, 0, // bulk OUT 1, 512 bytes
	7, 5, 0x81, 0x02, 0x00, 0x02, 0, // bulk IN 1, 512 bytes
}

var loopbackQualifier = []byte{10, 6, 0x00, 0x02, 0, 0, 0, 64, 1, 0}

var loopbackStrings = []string{"", "Mob", "Mob USB/IP loopback", "0001"}

func stringDescriptor(i int) []byte {
	if i == 0 {
		return []byte{4, 3, 0x09, 0x04} // English (US)
	}
	u := utf16.Encode([]rune(loopbackStrings[i]))
	b := []byte{byte(2 + 2*len(u)), 3}
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return b
}

func (l *Loopback) Control(s Setup, out []byte) ([]byte, int32) {
	const (
		getStatus, clearFeature, setAddress, getDescriptor = 0, 1, 5, 6
		setConfiguration, setInterface                     = 9, 11
	)
	if s.RequestType&0x60 != 0 { // not a standard request
		return nil, StatusStall
	}
	switch s.Request {
	case getDescriptor:
		switch kind, index := s.Value>>8, int(s.Value&0xff); kind {
		case 1:
			return loopbackDevice, StatusOK
		case 2:
			return loopbackConfig, StatusOK
		case 3:
			if index < len(loopbackStrings) {
				return stringDescriptor(index), StatusOK
			}
		case 6:
			return loopbackQualifier, StatusOK
		}
		return nil, StatusStall
	case getStatus:
		return []byte{0, 0}, StatusOK
	case setAddress, setConfiguration, setInterface, clearFeature:
		return nil, StatusOK
	}
	return nil, StatusStall
}

func (l *Loopback) Transfer(ctx context.Context, ep, dir uint32, out []byte, length int) ([]byte, int32) {
	if ep != 1 {
		return nil, StatusStall
	}
	l.mu.Lock()
	if l.ready == nil {
		l.ready = make(chan struct{})
	}
	if dir == DirOut {
		if len(l.buf)+len(out) > loopbackLimit {
			l.mu.Unlock()
			return nil, StatusStall
		}
		l.buf = append(l.buf, out...)
		close(l.ready)
		l.ready = make(chan struct{})
		l.mu.Unlock()
		return nil, StatusOK
	}
	for len(l.buf) == 0 {
		ready := l.ready
		l.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, StatusUnlinked
		}
		l.mu.Lock()
	}
	n := min(length, len(l.buf))
	in := append([]byte(nil), l.buf[:n]...)
	l.buf = l.buf[n:]
	l.mu.Unlock()
	return in, StatusOK
}
