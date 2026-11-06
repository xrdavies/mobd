//go:build unix

package usb

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrdavies/mobd/internal/usbip"
)

// pipeTunnel is the client's end of a tunnel, with the exporter (an emulated device) on the other
// side of an in-memory connection.
type pipeTunnel struct {
	c      net.Conn
	mu     sync.Mutex
	states []string
}

func newTunnel(t *testing.T, exporter usbip.EmulatedDevice) *pipeTunnel {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go usbip.Serve(b, exporter)
	return &pipeTunnel{c: a}
}

func (p *pipeTunnel) Read(ctx context.Context) ([]byte, error) {
	buf := make([]byte, 32<<10)
	type res struct {
		n   int
		err error
	}
	got := make(chan res, 1)
	go func() { n, err := p.c.Read(buf); got <- res{n, err} }()
	select {
	case r := <-got:
		return buf[:r.n], r.err
	case <-ctx.Done():
		p.c.Close()
		return nil, ctx.Err()
	}
}

func (p *pipeTunnel) Write(ctx context.Context, b []byte) error { _, err := p.c.Write(b); return err }
func (p *pipeTunnel) Ping(context.Context) error                { return nil }

func (p *pipeTunnel) Status(_ context.Context, s Session) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states = append(p.states, s.State)
	return nil
}

func (p *pipeTunnel) sawState(state string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.states {
		if s == state {
			return true
		}
	}
	return false
}

func waitState(t *testing.T, m *Manager, client, id, state string) Session {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if s, err := m.Get(client, id); err == nil && s.State == state {
			return s
		}
	}
	s, _ := m.Get(client, id)
	t.Fatalf("session %s: %s (%s), want %s", id, s.State, s.Reason, state)
	return s
}

func TestShareAttachReleaseDetach(t *testing.T) {
	imp := &Fake{}
	m := New(imp, nil)
	defer m.Close()
	s, err := m.Create("alice", Device{BusID: "1-1", Name: "Test probe"})
	if err != nil || s.State != StateConnecting {
		t.Fatalf("create %+v %v", s, err)
	}
	tun := newTunnel(t, &usbip.Loopback{BusID: "1-1"})
	served := make(chan error, 1)
	go func() { served <- m.Serve(context.Background(), "alice", s.ID, tun) }()

	got := waitState(t, m, "alice", s.ID, StateConnected)
	if got.LocalBusID != "9-1" {
		t.Fatalf("local bus %q", got.LocalBusID)
	}
	// The fake kernel read the device descriptor through the tunnel.
	if d := imp.Descriptor(0); len(d) != 18 || d[8] != 0x09 || d[9] != 0x12 {
		t.Fatalf("descriptor %x", d)
	}

	if _, err := m.Release("bob", s.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another client released it: %v", err)
	}
	if _, err := m.Get("bob", s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another client sees it")
	}
	if len(m.List("bob")) != 0 || len(m.List("alice")) != 1 {
		t.Fatal("list is not per client")
	}
	if err := m.Serve(context.Background(), "alice", s.ID, tun); !errors.Is(err, ErrState) {
		t.Fatalf("a second tunnel was accepted: %v", err)
	}

	r, err := m.Release("alice", s.ID, false)
	if err != nil || r.State != StateDisconnected {
		t.Fatalf("release %+v %v", r, err)
	}
	if <-served != nil || len(imp.Detached) != 1 {
		t.Fatalf("detached %v", imp.Detached)
	}
	if !tun.sawState(StateConnected) || !tun.sawState(StateDisconnected) {
		t.Fatalf("tunnel states %v", tun.states)
	}
}

func TestCreateChecks(t *testing.T) {
	m := New(&Fake{}, nil)
	if _, err := m.Create("a", Device{BusID: ""}); !errors.Is(err, ErrInvalidBus) {
		t.Fatalf("empty bus: %v", err)
	}
	if _, err := m.Create("a", Device{BusID: "1-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("a", Device{BusID: "1-1"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := m.Create("b", Device{BusID: "1-1"}); err != nil {
		t.Fatalf("another client's device with the same bus ID: %v", err)
	}
	for i := 2; i <= maxSessions; i++ {
		if _, err := m.Create("a", Device{BusID: "1-" + string(rune('0'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Create("a", Device{BusID: "2-1"}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("too many: %v", err)
	}
	notReady := New(&Fake{NotReady: "run setup"}, nil)
	if _, err := notReady.Create("a", Device{BusID: "1-1"}); !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "run setup") {
		t.Fatalf("not ready: %v", err)
	}
}

// The exporter does not have the device: the session fails with a reason and nothing is attached.
func TestExporterRefusesTheDevice(t *testing.T) {
	imp := &Fake{}
	m := New(imp, nil)
	s, _ := m.Create("a", Device{BusID: "1-9"})
	_ = m.Serve(context.Background(), "a", s.ID, newTunnel(t, &usbip.Loopback{BusID: "1-1"}))
	got, _ := m.Get("a", s.ID)
	if got.State != StateFailed || !strings.Contains(got.Reason, "did not share 1-9") || len(imp.Detached) != 0 {
		t.Fatalf("%+v detached %v", got, imp.Detached)
	}
}

// The other computer goes away: the device is detached here.
func TestTunnelLossDetaches(t *testing.T) {
	imp := &Fake{}
	m := New(imp, nil)
	s, _ := m.Create("a", Device{BusID: "1-1"})
	tun := newTunnel(t, &usbip.Loopback{BusID: "1-1"})
	go m.Serve(context.Background(), "a", s.ID, tun)
	waitState(t, m, "a", s.ID, StateConnected)
	tun.c.Close()
	got := waitState(t, m, "a", s.ID, StateDisconnected)
	if !strings.Contains(got.Reason, "closed") || len(imp.Detached) != 1 {
		t.Fatalf("%+v detached %v", got, imp.Detached)
	}
}

// Revoking the client's pairing ends the tunnel's context: detached.
func TestRevokeDetaches(t *testing.T) {
	imp := &Fake{}
	m := New(imp, nil)
	s, _ := m.Create("a", Device{BusID: "1-1"})
	ctx, revoke := context.WithCancel(context.Background())
	go m.Serve(ctx, "a", s.ID, newTunnel(t, &usbip.Loopback{BusID: "1-1"}))
	waitState(t, m, "a", s.ID, StateConnected)
	revoke()
	waitState(t, m, "a", s.ID, StateDisconnected)
	if len(imp.Detached) != 1 {
		t.Fatalf("detached %v", imp.Detached)
	}
}

// A mounted drive is not detached by a normal release; force detaches it.
func TestBusyDeviceNeedsForce(t *testing.T) {
	imp := &Fake{BusyWith: "/dev/sdb1 is mounted at /mnt"}
	m := New(imp, nil)
	defer m.Close()
	s, _ := m.Create("a", Device{BusID: "1-1"})
	go m.Serve(context.Background(), "a", s.ID, newTunnel(t, &usbip.Loopback{BusID: "1-1"}))
	waitState(t, m, "a", s.ID, StateConnected)
	if _, err := m.Release("a", s.ID, false); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "/mnt") {
		t.Fatalf("busy release: %v", err)
	}
	if got, _ := m.Get("a", s.ID); got.State != StateConnected {
		t.Fatalf("a refused release changed the state: %s", got.State)
	}
	if r, err := m.Release("a", s.ID, true); err != nil || r.State != StateDisconnected {
		t.Fatalf("forced release %+v %v", r, err)
	}
}

func TestUnopenedTunnelFails(t *testing.T) {
	m := New(&Fake{}, nil)
	now := time.Now()
	m.now = func() time.Time { return now }
	s, _ := m.Create("a", Device{BusID: "1-1"})
	now = now.Add(tunnelWait + time.Second)
	if got := m.List("a"); len(got) != 1 || got[0].State != StateFailed || got[0].ID != s.ID {
		t.Fatalf("%+v", got)
	}
}
