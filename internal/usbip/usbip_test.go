package usbip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

func pipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestDeviceIs312Bytes(t *testing.T) {
	if deviceSize != 312 || len(Device{}.marshal()) != 312 {
		t.Fatalf("device size %d", deviceSize)
	}
	d := (&Loopback{BusID: "1-1"}).Info()
	if got := unmarshalDevice(d.marshal()); got.BusID != "1-1" || got.VendorID != LoopbackVendor || got.Speed != SpeedHigh || got.DevID() != 1<<16|2 {
		t.Fatalf("round trip %+v", got)
	}
}

func TestImportGetsOnlyTheSelectedDevice(t *testing.T) {
	devices := []Device{{BusID: "1-1", NumInterfaces: 1, Interfaces: []Interface{{8, 6, 80}}}, {BusID: "1-2"}}
	client, server := pipe(t)
	go ServeOp(server, devices)
	d, err := Import(client, "1-2")
	if err != nil || d.BusID != "1-2" {
		t.Fatalf("import %+v %v", d, err)
	}

	client, server = pipe(t)
	go ServeOp(server, devices)
	if _, err := Import(client, "9-9"); err == nil {
		t.Fatal("imported a device the exporter does not have")
	}

	client, server = pipe(t)
	go ServeOp(server, devices)
	list, err := ListDevices(client)
	if err != nil || len(list) != 2 || list[0].Interfaces[0] != (Interface{8, 6, 80}) {
		t.Fatalf("list %+v %v", list, err)
	}
}

// An exporter that answers an import with another device is refused.
func TestImportRejectsAnotherDevice(t *testing.T) {
	client, server := pipe(t)
	go func() {
		b := make([]byte, 8+busIDSize)
		_, _ = server.Read(b)
		_ = writeOp(server, OpRepImport, 0, Device{BusID: "1-7"}.marshal())
	}()
	if _, err := Import(client, "1-1"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
}

func TestReadURBBoundsInput(t *testing.T) {
	big := &URB{Command: CmdSubmit, Direction: DirIn, BufferLength: MaxTransfer + 1, NumPackets: -1}
	var b bytes.Buffer
	_ = WriteURB(&b, big)
	if _, err := ReadURB(&b, nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversized transfer: %v", err)
	}
	b.Reset()
	_ = WriteURB(&b, &URB{Command: CmdSubmit, Endpoint: 16})
	if _, err := ReadURB(&b, nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("endpoint 16: %v", err)
	}
	b.Reset()
	b.Write(append(binary.BigEndian.AppendUint32(nil, 9), make([]byte, headerSize-4)...))
	if _, err := ReadURB(&b, nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("unknown command: %v", err)
	}
}

// importer drives an emulated device the way the kernel would, for tests.
type importer struct {
	t    *testing.T
	c    net.Conn
	seq  uint32
	dirs map[uint32]uint32
}

func newImporter(t *testing.T) *importer {
	client, server := pipe(t)
	go Serve(server, &Loopback{BusID: "1-1"})
	if _, err := Import(client, "1-1"); err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	return &importer{t: t, c: client, dirs: map[uint32]uint32{}}
}

func (m *importer) submit(ep, dir uint32, setup []byte, out []byte, length int) uint32 {
	m.seq++
	u := &URB{Command: CmdSubmit, SeqNum: m.seq, DevID: 1<<16 | 2, Direction: dir, Endpoint: ep, BufferLength: int32(length), NumPackets: -1, Data: out}
	if dir == DirOut {
		u.BufferLength = int32(len(out))
	}
	copy(u.Setup[:], setup)
	m.dirs[m.seq] = dir
	if err := WriteURB(m.c, u); err != nil {
		m.t.Fatal(err)
	}
	return m.seq
}

func (m *importer) read() *URB {
	u, err := ReadURB(m.c, func(seq uint32) bool { return m.dirs[seq] == DirIn })
	if err != nil {
		m.t.Fatal(err)
	}
	return u
}

func TestLoopbackEnumeratesAndEchoes(t *testing.T) {
	m := newImporter(t)
	m.submit(0, DirIn, []byte{0x80, 6, 0, 1, 0, 0, 18, 0}, nil, 18) // GET_DESCRIPTOR device
	if r := m.read(); r.Status != StatusOK || len(r.Data) != 18 || binary.LittleEndian.Uint16(r.Data[8:]) != LoopbackVendor {
		t.Fatalf("device descriptor %+v", r)
	}
	m.submit(0, DirIn, []byte{0x80, 6, 0, 2, 0, 0, 9, 0}, nil, 9) // the config header only
	if r := m.read(); len(r.Data) != 9 || r.Data[2] != 32 {
		t.Fatalf("config header %+v", r.Data)
	}
	m.submit(0, DirIn, []byte{0xc0, 1, 0, 0, 0, 0, 1, 0}, nil, 1) // vendor request
	if r := m.read(); r.Status != StatusStall {
		t.Fatalf("vendor request not stalled: %+v", r)
	}

	m.submit(1, DirOut, nil, []byte("hello usb"), 0)
	if r := m.read(); r.Status != StatusOK || r.BufferLength != 9 {
		t.Fatalf("bulk out %+v", r)
	}
	m.submit(1, DirIn, nil, nil, 512)
	if r := m.read(); r.Status != StatusOK || string(r.Data) != "hello usb" {
		t.Fatalf("bulk in %+v", r)
	}
}

// A pending IN is cancelled by CMD_UNLINK: one RET_UNLINK, and no RET_SUBMIT for it.
func TestUnlinkCancelsAPendingTransfer(t *testing.T) {
	m := newImporter(t)
	pending := m.submit(1, DirIn, nil, nil, 512)
	m.seq++
	if err := WriteURB(m.c, &URB{Command: CmdUnlink, SeqNum: m.seq, DevID: 1<<16 | 2, UnlinkSeqNum: pending}); err != nil {
		t.Fatal(err)
	}
	if r := m.read(); r.Command != RetUnlink || r.Status != StatusUnlinked || r.SeqNum != m.seq {
		t.Fatalf("unlink %+v", r)
	}
	// The next message answers the next submit, not the unlinked one.
	m.submit(1, DirOut, nil, []byte("x"), 0)
	if r := m.read(); r.Command != RetSubmit || r.SeqNum != m.seq {
		t.Fatalf("after unlink %+v", r)
	}
}
