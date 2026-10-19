//go:build linux

package manager

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maxDisplayTries bounds the search for a free display number.
const maxDisplayTries = 50

const cookieName = "MIT-MAGIC-COOKIE-1"

// displayFree reports whether nothing holds display n: no lock file naming a live process, and
// no server answering on the abstract or the file socket. tmp is "/tmp" outside tests.
func displayFree(tmp string, n int) bool {
	if b, err := os.ReadFile(filepath.Join(tmp, ".X"+strconv.Itoa(n)+"-lock")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pidAlive(pid) {
			return false
		}
	}
	for _, addr := range xSockets(tmp, n) {
		if c, err := net.DialTimeout("unix", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return false
		}
	}
	return true
}

func xSockets(tmp string, n int) []string {
	path := filepath.Join(tmp, ".X11-unix", "X"+strconv.Itoa(n))
	return []string{"@" + path, path}
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// writeXauthority writes one MIT-MAGIC-COOKIE-1 entry for display n, family wild (any host), in
// the Xau file format; 0600.
func writeXauthority(path string, n int, cookie []byte) error {
	var b bytes.Buffer
	field := func(v []byte) {
		_ = binary.Write(&b, binary.BigEndian, uint16(len(v)))
		b.Write(v)
	}
	_ = binary.Write(&b, binary.BigEndian, uint16(0xffff)) // FamilyWild
	field(nil)
	field([]byte(strconv.Itoa(n)))
	field([]byte(cookieName))
	field(cookie)
	return os.WriteFile(path, b.Bytes(), 0600)
}

// xHandshake connects to display n and completes the X11 connection setup with the cookie,
// polling until the server accepts or ctx ends. A refused cookie fails at once.
func xHandshake(ctx context.Context, tmp string, n int, cookie []byte) error {
	var last error
	for {
		for _, addr := range xSockets(tmp, n) {
			err := xSetup(ctx, addr, cookie)
			if err == nil {
				return nil
			}
			var refused setupRefused
			if errors.As(err, &refused) {
				return err
			}
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("X server on :%d did not answer: %w", n, errors.Join(ctx.Err(), last))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// setupRefused: the server answered and rejected the connection (bad cookie).
type setupRefused string

func (s setupRefused) Error() string { return "X server refused the connection: " + string(s) }

func xSetup(ctx context.Context, addr string, cookie []byte) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	pad := func(n int) int { return (4 - n%4) % 4 }
	var req bytes.Buffer
	req.Write([]byte{'l', 0}) // little-endian
	_ = binary.Write(&req, binary.LittleEndian, []uint16{11, 0, uint16(len(cookieName)), uint16(len(cookie)), 0})
	req.WriteString(cookieName)
	req.Write(make([]byte, pad(len(cookieName))))
	req.Write(cookie)
	req.Write(make([]byte, pad(len(cookie))))
	if _, err := c.Write(req.Bytes()); err != nil {
		return err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	switch head[0] {
	case 1:
		return nil
	case 0:
		reason := make([]byte, head[1])
		_, _ = io.ReadFull(c, reason)
		return setupRefused(strings.TrimSpace(string(reason)))
	default:
		return setupRefused("further authentication required")
	}
}

// dialReady polls a unix socket until it accepts a connection (the desktop's D-Bus).
func dialReady(ctx context.Context, path string) error {
	for {
		c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", path, errors.Join(ctx.Err(), err))
		case <-time.After(50 * time.Millisecond):
		}
	}
}
