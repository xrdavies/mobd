// Package usb shares a USB device from a paired client's computer with this one
// (docs/usb-redirection.md). A session is owned by the paired client that created it and names one
// device. Its tunnel carries USB/IP: mobd imports that one device over it and hands the connection
// to the operating system's importer, so the device appears here like any other USB device.
package usb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/xrdavies/mobd/internal/usbip"
)

// States of a session (docs/usb-redirection.md#discovery-and-lifecycle-contract).
const (
	StateConnecting    = "connecting"
	StateConnected     = "connected"
	StateDisconnecting = "disconnecting"
	StateDisconnected  = "disconnected"
	StateFailed        = "failed"
)

var (
	ErrNotFound    = errors.New("USB session not found")
	ErrState       = errors.New("the USB session is not waiting for its tunnel")
	ErrDuplicate   = errors.New("this device is already shared with this computer")
	ErrTooMany     = errors.New("too many USB sessions for this device")
	ErrNotReady    = errors.New("this computer cannot import USB devices")
	ErrBusy        = errors.New("the device is in use")
	ErrInvalidBus  = errors.New("busId must be 1-31 characters")
	errTunnelEnded = errors.New("the tunnel closed")
)

// Limits: sessions per client, and how long each step of attaching may take.
const (
	maxSessions   = 4
	importTimeout = 15 * time.Second
	enumerateWait = 10 * time.Second
	// A session's tunnel must open within this.
	tunnelWait = 2 * time.Minute
	// Ended sessions stay listed this long, so the client can read why.
	keepEnded = 10 * time.Minute
)

// Importer attaches imported devices to this operating system.
type Importer interface {
	// Backend names the importer ("vhci"), or "" when there is none.
	Backend() string
	// Ready reports whether devices can be attached now, and why not.
	Ready() (bool, string)
	// Attach gives the OS the stream socket fd, on which Import returned d. The OS keeps its own
	// reference; the caller closes fd afterwards. It returns a port for Detach.
	Attach(fd int, d usbip.Device) (int, error)
	// LocalBusID is where the device on port appears once enumerated ("" until then).
	LocalBusID(port int) (string, error)
	// Detach removes the device on port.
	Detach(port int) error
	// Busy reports why the device on port should not be detached now, such as a mounted
	// filesystem; "" when it can be.
	Busy(port int) string
}

// Device is what the client says about the shared device, shown to users; mobd trusts only BusID,
// and checks it against the exporter's reply.
type Device struct {
	BusID        string `json:"busId"`
	Name         string `json:"name,omitempty"`
	VendorID     uint16 `json:"vendorId,omitempty"`
	ProductID    uint16 `json:"productId,omitempty"`
	SerialNumber string `json:"serialNumber,omitempty"`
}

// Session is one shared device, as the API shows it.
type Session struct {
	ID     string `json:"id"`
	Device Device `json:"device"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// LocalBusID is where the device appeared on this computer, once connected.
	LocalBusID string    `json:"localBusId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type session struct {
	Session
	client string
	tunnel bool
	port   int
	ported bool
	stop   context.CancelFunc
	done   chan struct{}
}

// Manager holds the USB sessions of every client.
type Manager struct {
	imp      Importer
	now      func() time.Time
	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	// notify is called after a session changes, without the lock held.
	notify func(client string, s Session)
}

// New makes a manager attaching through imp. notify may be nil.
func New(imp Importer, notify func(client string, s Session)) *Manager {
	return &Manager{imp: imp, now: time.Now, sessions: map[string]*session{}, notify: notify}
}

// Readiness is the importer's state for /health.
type Readiness struct {
	Backend string `json:"backend"`
	Ready   bool   `json:"ready"`
	Detail  string `json:"detail,omitempty"`
}

func (m *Manager) Readiness() Readiness {
	ok, why := m.imp.Ready()
	b := m.imp.Backend()
	if b == "" {
		b = "none"
	}
	return Readiness{Backend: b, Ready: ok, Detail: why}
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (m *Manager) changed(s *session) {
	if m.notify != nil {
		c, v := s.client, s.Session
		go m.notify(c, v)
	}
}

// Create starts a session for client's device; the client then opens its tunnel.
func (m *Manager) Create(client string, d Device) (Session, error) {
	if d.BusID == "" || len(d.BusID) > 31 {
		return Session{}, ErrInvalidBus
	}
	if len(d.Name) > 200 || len(d.SerialNumber) > 200 {
		return Session{}, fmt.Errorf("device name and serial number are limited to 200 characters")
	}
	if ok, why := m.imp.Ready(); !ok {
		return Session{}, fmt.Errorf("%w: %s", ErrNotReady, why)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gc()
	if m.closed {
		return Session{}, ErrNotReady
	}
	n := 0
	for _, s := range m.sessions {
		if s.client != client || ended(s.State) {
			continue
		}
		n++
		if s.Device.BusID == d.BusID {
			return Session{}, ErrDuplicate
		}
	}
	if n >= maxSessions {
		return Session{}, ErrTooMany
	}
	now := m.now().UTC()
	s := &session{Session: Session{ID: newID(), Device: d, State: StateConnecting, CreatedAt: now, UpdatedAt: now}, client: client, done: make(chan struct{})}
	m.sessions[s.ID] = s
	m.changed(s)
	return s.Session, nil
}

func ended(state string) bool { return state == StateDisconnected || state == StateFailed }

// gc drops sessions that ended a while ago; the caller holds m.mu.
func (m *Manager) gc() {
	for _, s := range m.sessions {
		if s.State == StateConnecting && !s.tunnel && m.now().Sub(s.CreatedAt) > tunnelWait {
			m.set(s, StateFailed, "the tunnel was never opened")
		}
	}
	for id, s := range m.sessions {
		if ended(s.State) && m.now().Sub(s.UpdatedAt) > keepEnded {
			delete(m.sessions, id)
		}
	}
}

// List returns client's sessions, newest first. Other clients' sessions are never shown.
func (m *Manager) List(client string) []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gc()
	var out []Session
	for _, s := range m.sessions {
		if s.client == client {
			out = append(out, s.Session)
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// Get returns client's session id.
func (m *Manager) Get(client, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil || s.client != client {
		return Session{}, ErrNotFound
	}
	return s.Session, nil
}

// set changes a session's state; the caller holds m.mu.
func (m *Manager) set(s *session, state, reason string) {
	if s.State == state && s.Reason == reason {
		return
	}
	s.State, s.Reason, s.UpdatedAt = state, reason, m.now().UTC()
	m.changed(s)
}

// Release stops client's session id: the device is detached here, then the tunnel closes. A
// device still in use (a mounted drive) is left attached and ErrBusy returned, unless force: then
// pending writes may be lost.
func (m *Manager) Release(client, id string, force bool) (Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil || s.client != client {
		m.mu.Unlock()
		return Session{}, ErrNotFound
	}
	if ended(s.State) {
		delete(m.sessions, id)
		m.mu.Unlock()
		return s.Session, nil
	}
	if s.ported && !force {
		if why := m.imp.Busy(s.port); why != "" {
			m.mu.Unlock()
			return s.Session, fmt.Errorf("%w: %s", ErrBusy, why)
		}
	}
	if !s.tunnel {
		m.set(s, StateDisconnected, "released")
		v := s.Session
		m.mu.Unlock()
		return v, nil
	}
	m.set(s, StateDisconnecting, "")
	stop, done := s.stop, s.done
	m.mu.Unlock()
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	return m.Get(client, id)
}

// ReleaseClient ends every session of client (revoked or deleted), without waiting.
func (m *Manager) ReleaseClient(client string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.client == client && !ended(s.State) {
			if s.stop != nil {
				s.stop()
			} else {
				m.set(s, StateDisconnected, "pairing revoked")
			}
		}
	}
}

// Close ends every session and waits for their devices to be detached.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var waits []chan struct{}
	for _, s := range m.sessions {
		if s.stop != nil {
			s.stop()
			waits = append(waits, s.done)
		}
	}
	m.mu.Unlock()
	for _, d := range waits {
		select {
		case <-d:
		case <-time.After(10 * time.Second):
		}
	}
}

// Tunnel is the client's side of a session's tunnel: a message stream. Read returns the next
// binary message; Write sends one; Status sends a text status message.
type Tunnel interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, b []byte) error
	Status(ctx context.Context, s Session) error
	Ping(ctx context.Context) error
}

// Serve runs client's tunnel for session id until it ends: the device is imported over it,
// attached here, and detached when it ends. ctx ends with the client's pairing.
func (m *Manager) Serve(ctx context.Context, client, id string, t Tunnel) error {
	m.mu.Lock()
	s := m.sessions[id]
	switch {
	case s == nil || s.client != client:
		m.mu.Unlock()
		return ErrNotFound
	case s.tunnel || s.State != StateConnecting || m.closed:
		m.mu.Unlock()
		return ErrState
	}
	ctx, stop := context.WithCancel(ctx)
	s.tunnel, s.stop = true, stop
	m.mu.Unlock()
	defer close(s.done)
	defer stop()

	reason := m.run(ctx, s, t)
	m.mu.Lock()
	s.LocalBusID = ""
	switch {
	case s.State == StateDisconnecting || reason == nil:
		m.set(s, StateDisconnected, "released")
	case errors.Is(reason, errTunnelEnded) && s.State == StateConnected:
		m.set(s, StateDisconnected, "the connection to the other computer closed")
	default:
		m.set(s, StateFailed, reason.Error())
	}
	v := s.Session
	m.mu.Unlock()
	log.Printf("usb %s: %s (%s)", s.ID, v.State, v.Reason)
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = t.Status(sctx, v)
	cancel()
	return nil
}

func (m *Manager) detach(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !s.ported {
		return
	}
	if err := m.imp.Detach(s.port); err != nil {
		log.Printf("usb %s: detach port %d: %v", s.ID, s.port, err)
	}
	s.ported = false
}

// run attaches the device and keeps it until ctx ends or the tunnel fails; nil means released.
func (m *Manager) run(ctx context.Context, s *session, t Tunnel) error {
	kernel, bridge, err := socketPair()
	if err != nil {
		return err
	}
	defer kernel.Close()
	defer bridge.Close()

	failed := make(chan error, 3)
	// Bytes from the exporter to the importer; and back.
	go func() {
		for {
			b, err := t.Read(ctx)
			if err != nil {
				failed <- errTunnelEnded
				return
			}
			if _, err := bridge.Write(b); err != nil {
				failed <- fmt.Errorf("the importer closed the device: %w", err)
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := bridge.Read(buf)
			if n > 0 {
				if werr := t.Write(ctx, buf[:n]); werr != nil {
					failed <- errTunnelEnded
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					failed <- fmt.Errorf("the importer closed the device: %w", err)
				} else {
					failed <- errors.New("the device was detached on this computer")
				}
				return
			}
		}
	}()
	// Liveness: a tunnel that stops answering pings ends the session, so no device stays leased.
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := t.Ping(pctx)
				cancel()
				if err != nil && ctx.Err() == nil {
					failed <- errors.New("the other computer stopped answering")
					return
				}
			}
		}
	}()

	// Import the one device the client named, over the tunnel.
	type imported struct {
		d   usbip.Device
		err error
	}
	got := make(chan imported, 1)
	_ = kernel.SetDeadline(time.Now().Add(importTimeout))
	go func() {
		d, err := usbip.Import(kernel, s.Device.BusID)
		got <- imported{d, err}
	}()
	var d usbip.Device
	select {
	case r := <-got:
		if errors.Is(r.err, os.ErrDeadlineExceeded) {
			return errors.New("the other computer did not answer the import")
		}
		if r.err != nil {
			return fmt.Errorf("the other computer did not share %s: %w", s.Device.BusID, r.err)
		}
		d = r.d
	case err := <-failed:
		// An exporter that refuses the device then closes: its answer says more than the close.
		select {
		case r := <-got:
			if r.err != nil && !errors.Is(r.err, os.ErrDeadlineExceeded) {
				return fmt.Errorf("the other computer did not share %s: %w", s.Device.BusID, r.err)
			}
		case <-time.After(200 * time.Millisecond):
		}
		return err
	case <-ctx.Done():
		return nil
	}
	_ = kernel.SetDeadline(time.Time{})
	// Fd puts the socket in blocking mode, as the kernel's importer expects; Go does no more I/O on it.
	port, err := m.imp.Attach(int(kernel.Fd()), d)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	// The OS holds the socket now; ours can go.
	kernel.Close()
	m.mu.Lock()
	s.port, s.ported = port, true
	m.mu.Unlock()
	// Detach first, while the tunnel is still up: the OS removes the device in order instead of
	// seeing its connection drop. Deferred after the socket pair's Close, so it runs before it.
	defer m.detach(s)

	deadline := time.After(enumerateWait)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for connected := false; !connected; {
		select {
		case err := <-failed:
			return err
		case <-ctx.Done():
			return nil
		case <-deadline:
			return errors.New("the device did not appear on this computer")
		case <-tick.C:
			if id, _ := m.imp.LocalBusID(port); id != "" {
				m.mu.Lock()
				s.LocalBusID = id
				m.set(s, StateConnected, "")
				v := s.Session
				m.mu.Unlock()
				log.Printf("usb %s: %s attached as %s", s.ID, s.Device.BusID, id)
				_ = t.Status(ctx, v)
				connected = true
			}
		}
	}
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		return nil
	}
}
