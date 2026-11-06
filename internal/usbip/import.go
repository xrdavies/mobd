package usbip

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Import asks the exporter on rw for the device busID and returns it. After a nil error, rw carries
// that device's URB traffic. Only this one device is ever requested: mobd imports what the user
// selected, never what an exporter happens to list.
func Import(rw io.ReadWriter, busID string) (Device, error) {
	id, err := busIDBytes(busID)
	if err != nil {
		return Device{}, err
	}
	if err := writeOp(rw, OpReqImport, 0, id); err != nil {
		return Device{}, err
	}
	h, err := readOp(rw)
	if err != nil {
		return Device{}, err
	}
	if h.Code != OpRepImport {
		return Device{}, fmt.Errorf("%w: reply %#04x to import", ErrProtocol, h.Code)
	}
	if h.Status != 0 {
		return Device{}, fmt.Errorf("usbip: the exporter refused %s (status %d)", busID, h.Status)
	}
	b := make([]byte, deviceSize)
	if _, err := io.ReadFull(rw, b); err != nil {
		return Device{}, err
	}
	d := unmarshalDevice(b)
	if d.BusID != busID {
		return Device{}, fmt.Errorf("%w: asked for %s, the exporter sent %s", ErrProtocol, busID, d.BusID)
	}
	return d, nil
}

// ListDevices asks the exporter on rw for its device list. The connection is done afterwards, as
// in USB/IP: an import needs a new one.
func ListDevices(rw io.ReadWriter) ([]Device, error) {
	if err := writeOp(rw, OpReqDevlist, 0, nil); err != nil {
		return nil, err
	}
	h, err := readOp(rw)
	if err != nil {
		return nil, err
	}
	if h.Code != OpRepDevlist || h.Status != 0 {
		return nil, fmt.Errorf("%w: reply %#04x status %d to the device list", ErrProtocol, h.Code, h.Status)
	}
	var n [4]byte
	if _, err := io.ReadFull(rw, n[:]); err != nil {
		return nil, err
	}
	count := binary.BigEndian.Uint32(n[:])
	if count > 256 {
		return nil, fmt.Errorf("%w: %d devices", ErrProtocol, count)
	}
	out := make([]Device, 0, count)
	for range count {
		b := make([]byte, deviceSize)
		if _, err := io.ReadFull(rw, b); err != nil {
			return nil, err
		}
		d := unmarshalDevice(b)
		for range d.NumInterfaces {
			var f [4]byte
			if _, err := io.ReadFull(rw, f[:]); err != nil {
				return nil, err
			}
			d.Interfaces = append(d.Interfaces, Interface{f[0], f[1], f[2]})
		}
		out = append(out, d)
	}
	return out, nil
}

// ServeOp answers one operation on rw from an exporter's devices. For an import of a listed device
// it returns that device, and rw then carries its URB traffic; otherwise it returns ok=false.
func ServeOp(rw io.ReadWriter, devices []Device) (d Device, ok bool, err error) {
	h, err := readOp(rw)
	if err != nil {
		return Device{}, false, err
	}
	switch h.Code {
	case OpReqDevlist:
		body := binary.BigEndian.AppendUint32(nil, uint32(len(devices)))
		for _, d := range devices {
			body = append(body, d.marshal()...)
			for _, f := range d.Interfaces {
				body = append(body, f.Class, f.SubClass, f.Protocol, 0)
			}
		}
		return Device{}, false, writeOp(rw, OpRepDevlist, 0, body)
	case OpReqImport:
		b := make([]byte, busIDSize)
		if _, err := io.ReadFull(rw, b); err != nil {
			return Device{}, false, err
		}
		want := getString(b)
		for _, d := range devices {
			if d.BusID == want {
				return d, true, writeOp(rw, OpRepImport, 0, d.marshal())
			}
		}
		return Device{}, false, writeOp(rw, OpRepImport, 1, nil)
	default:
		return Device{}, false, fmt.Errorf("%w: operation %#04x", ErrProtocol, h.Code)
	}
}
