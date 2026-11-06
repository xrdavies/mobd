// Package usbip speaks the USB/IP protocol (docs/usb-redirection.md): the device list and import
// operations that pick one exported device, then the URB commands that carry its traffic. The wire
// format is the kernel's (Documentation/usb/usbip_protocol.rst); all fields are big-endian.
package usbip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the USB/IP protocol version in operation headers.
const Version = 0x0111

// Operation codes, used before a device is imported.
const (
	OpReqDevlist = 0x8005
	OpRepDevlist = 0x0005
	OpReqImport  = 0x8003
	OpRepImport  = 0x0003
)

// URB commands, used once a device is imported.
const (
	CmdSubmit = 0x0001
	CmdUnlink = 0x0002
	RetSubmit = 0x0003
	RetUnlink = 0x0004
)

// Directions in a URB header.
const (
	DirOut = 0
	DirIn  = 1
)

// USB speeds, as the kernel numbers them (enum usb_device_speed).
const (
	SpeedLow   = 1
	SpeedFull  = 2
	SpeedHigh  = 3
	SpeedSuper = 5
)

// Status values in RET_SUBMIT and RET_UNLINK: 0 or a negative errno.
const (
	StatusOK       = 0
	StatusStall    = -32  // EPIPE: the endpoint stalled
	StatusUnlinked = -104 // ECONNRESET: the URB was unlinked
)

const (
	busIDSize  = 32
	pathSize   = 256
	deviceSize = pathSize + busIDSize + 4*3 + 2*3 + 6 // 312
	headerSize = 48                                   // every URB header, padded
	// MaxTransfer bounds one URB's buffer, so a peer cannot make either side allocate without limit.
	MaxTransfer = 1 << 20
	// maxIsoPackets bounds the isochronous descriptors that may follow a URB.
	maxIsoPackets = 1024
)

// ErrProtocol is a message that does not follow USB/IP.
var ErrProtocol = errors.New("usbip: protocol error")

// Device describes an exported device, as in OP_REP_DEVLIST and OP_REP_IMPORT.
type Device struct {
	Path               string
	BusID              string
	BusNum, DevNum     uint32
	Speed              uint32
	VendorID           uint16
	ProductID          uint16
	BCDDevice          uint16
	Class              uint8
	SubClass           uint8
	Protocol           uint8
	ConfigurationValue uint8
	NumConfigurations  uint8
	NumInterfaces      uint8
	// Interfaces is sent only in the device list.
	Interfaces []Interface
}

// Interface is one interface's class triple in the device list.
type Interface struct{ Class, SubClass, Protocol uint8 }

// DevID is the device's ID in URB headers: bus and device number.
func (d Device) DevID() uint32 { return d.BusNum<<16 | d.DevNum }

func putString(b []byte, s string) { copy(b, s) }

func getString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func (d Device) marshal() []byte {
	b := make([]byte, deviceSize)
	putString(b[0:pathSize], d.Path)
	putString(b[pathSize:pathSize+busIDSize], d.BusID)
	o := pathSize + busIDSize
	binary.BigEndian.PutUint32(b[o:], d.BusNum)
	binary.BigEndian.PutUint32(b[o+4:], d.DevNum)
	binary.BigEndian.PutUint32(b[o+8:], d.Speed)
	binary.BigEndian.PutUint16(b[o+12:], d.VendorID)
	binary.BigEndian.PutUint16(b[o+14:], d.ProductID)
	binary.BigEndian.PutUint16(b[o+16:], d.BCDDevice)
	copy(b[o+18:], []byte{d.Class, d.SubClass, d.Protocol, d.ConfigurationValue, d.NumConfigurations, d.NumInterfaces})
	return b
}

func unmarshalDevice(b []byte) Device {
	o := pathSize + busIDSize
	return Device{
		Path: getString(b[0:pathSize]), BusID: getString(b[pathSize : pathSize+busIDSize]),
		BusNum: binary.BigEndian.Uint32(b[o:]), DevNum: binary.BigEndian.Uint32(b[o+4:]), Speed: binary.BigEndian.Uint32(b[o+8:]),
		VendorID: binary.BigEndian.Uint16(b[o+12:]), ProductID: binary.BigEndian.Uint16(b[o+14:]), BCDDevice: binary.BigEndian.Uint16(b[o+16:]),
		Class: b[o+18], SubClass: b[o+19], Protocol: b[o+20], ConfigurationValue: b[o+21], NumConfigurations: b[o+22], NumInterfaces: b[o+23],
	}
}

// opHeader is the 8-byte header of every operation.
type opHeader struct {
	Version, Code uint16
	Status        uint32
}

func writeOp(w io.Writer, code uint16, status uint32, body []byte) error {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint16(b, Version)
	binary.BigEndian.PutUint16(b[2:], code)
	binary.BigEndian.PutUint32(b[4:], status)
	_, err := w.Write(append(b, body...))
	return err
}

func readOp(r io.Reader) (opHeader, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return opHeader{}, err
	}
	h := opHeader{binary.BigEndian.Uint16(b[:]), binary.BigEndian.Uint16(b[2:]), binary.BigEndian.Uint32(b[4:])}
	if h.Version != Version {
		return h, fmt.Errorf("%w: version %#04x", ErrProtocol, h.Version)
	}
	return h, nil
}

func busIDBytes(busID string) ([]byte, error) {
	if busID == "" || len(busID) >= busIDSize {
		return nil, fmt.Errorf("usbip: bus ID %q must be 1-%d bytes", busID, busIDSize-1)
	}
	b := make([]byte, busIDSize)
	putString(b, busID)
	return b, nil
}

// URB is one URB message after import: CMD_SUBMIT, CMD_UNLINK, RET_SUBMIT or RET_UNLINK.
type URB struct {
	Command   uint32
	SeqNum    uint32
	DevID     uint32
	Direction uint32
	Endpoint  uint32

	// CMD_SUBMIT
	TransferFlags uint32
	BufferLength  int32 // CMD_SUBMIT: requested; RET_SUBMIT: actual
	StartFrame    int32
	NumPackets    int32 // -1 (0xffffffff) for non-isochronous
	Interval      int32
	Setup         [8]byte

	// RET_SUBMIT and RET_UNLINK
	Status     int32
	ErrorCount int32

	// CMD_UNLINK
	UnlinkSeqNum uint32

	// Data is the transfer buffer: sent with CMD_SUBMIT OUT and RET_SUBMIT IN.
	Data []byte
	// Iso is the raw isochronous packet descriptors (16 bytes each), if any.
	Iso []byte
}

// WriteURB writes u and its buffer.
func WriteURB(w io.Writer, u *URB) error {
	b := make([]byte, headerSize)
	be := binary.BigEndian
	be.PutUint32(b[0:], u.Command)
	be.PutUint32(b[4:], u.SeqNum)
	be.PutUint32(b[8:], u.DevID)
	be.PutUint32(b[12:], u.Direction)
	be.PutUint32(b[16:], u.Endpoint)
	switch u.Command {
	case CmdSubmit:
		be.PutUint32(b[20:], u.TransferFlags)
		be.PutUint32(b[24:], uint32(u.BufferLength))
		be.PutUint32(b[28:], uint32(u.StartFrame))
		be.PutUint32(b[32:], uint32(u.NumPackets))
		be.PutUint32(b[36:], uint32(u.Interval))
		copy(b[40:], u.Setup[:])
	case RetSubmit:
		be.PutUint32(b[20:], uint32(u.Status))
		be.PutUint32(b[24:], uint32(u.BufferLength))
		be.PutUint32(b[28:], uint32(u.StartFrame))
		be.PutUint32(b[32:], uint32(u.NumPackets))
		be.PutUint32(b[36:], uint32(u.ErrorCount))
	case CmdUnlink:
		be.PutUint32(b[20:], u.UnlinkSeqNum)
	case RetUnlink:
		be.PutUint32(b[20:], uint32(u.Status))
	default:
		return fmt.Errorf("%w: command %d", ErrProtocol, u.Command)
	}
	b = append(b, u.Data...)
	b = append(b, u.Iso...)
	_, err := w.Write(b)
	return err
}

// ReadURB reads one URB message. A buffer follows CMD_SUBMIT only when it is OUT and RET_SUBMIT
// only when it is IN; for RET_SUBMIT the direction comes from the submit it answers (`in`).
func ReadURB(r io.Reader, in func(seq uint32) bool) (*URB, error) {
	b := make([]byte, headerSize)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	be := binary.BigEndian
	u := &URB{Command: be.Uint32(b[0:]), SeqNum: be.Uint32(b[4:]), DevID: be.Uint32(b[8:]), Direction: be.Uint32(b[12:]), Endpoint: be.Uint32(b[16:])}
	if u.Endpoint > 15 || u.Direction > DirIn {
		return nil, fmt.Errorf("%w: endpoint %d direction %d", ErrProtocol, u.Endpoint, u.Direction)
	}
	data := 0
	switch u.Command {
	case CmdSubmit:
		u.TransferFlags = be.Uint32(b[20:])
		u.BufferLength = int32(be.Uint32(b[24:]))
		u.StartFrame = int32(be.Uint32(b[28:]))
		u.NumPackets = int32(be.Uint32(b[32:]))
		u.Interval = int32(be.Uint32(b[36:]))
		copy(u.Setup[:], b[40:48])
		if u.BufferLength < 0 || u.BufferLength > MaxTransfer {
			return nil, fmt.Errorf("%w: transfer of %d bytes", ErrProtocol, u.BufferLength)
		}
		if u.Direction == DirOut {
			data = int(u.BufferLength)
		}
	case RetSubmit:
		u.Status = int32(be.Uint32(b[20:]))
		u.BufferLength = int32(be.Uint32(b[24:]))
		u.StartFrame = int32(be.Uint32(b[28:]))
		u.NumPackets = int32(be.Uint32(b[32:]))
		u.ErrorCount = int32(be.Uint32(b[36:]))
		if u.BufferLength < 0 || u.BufferLength > MaxTransfer {
			return nil, fmt.Errorf("%w: transfer of %d bytes", ErrProtocol, u.BufferLength)
		}
		if in != nil && in(u.SeqNum) {
			data = int(u.BufferLength)
		}
	case CmdUnlink:
		u.UnlinkSeqNum = be.Uint32(b[20:])
	case RetUnlink:
		u.Status = int32(be.Uint32(b[20:]))
	default:
		return nil, fmt.Errorf("%w: command %d", ErrProtocol, u.Command)
	}
	if data > 0 {
		u.Data = make([]byte, data)
		if _, err := io.ReadFull(r, u.Data); err != nil {
			return nil, err
		}
	}
	if (u.Command == CmdSubmit || u.Command == RetSubmit) && u.NumPackets > 0 {
		if u.NumPackets > maxIsoPackets {
			return nil, fmt.Errorf("%w: %d isochronous packets", ErrProtocol, u.NumPackets)
		}
		u.Iso = make([]byte, 16*int(u.NumPackets))
		if _, err := io.ReadFull(r, u.Iso); err != nil {
			return nil, err
		}
	}
	return u, nil
}
