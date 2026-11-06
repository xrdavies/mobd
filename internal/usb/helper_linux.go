package usb

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xrdavies/mobd/internal/usbip"
)

// The root helper (`mobd usb-helper`, installed by `sudo mobd usb setup`): attaching to vhci_hcd
// needs root and mobd runs as the user. It accepts only that user, and only two operations: attach
// a socket mobd hands it (SCM_RIGHTS), and detach a port it attached. No shell, no paths, no
// network: mobd still does the import and owns the tunnel.

// HelperSocket is where the helper for uid listens.
func HelperSocket(uid int) string { return filepath.Join("/run/mobd-usb", strconv.Itoa(uid)+".sock") }

type helperRequest struct {
	Op    string `json:"op"` // attach | detach
	DevID uint32 `json:"devId,omitempty"`
	Speed uint32 `json:"speed,omitempty"`
	Port  int    `json:"port,omitempty"`
}

type helperReply struct {
	Port  int    `json:"port,omitempty"`
	Error string `json:"error,omitempty"`
}

// callHelper sends one request, with fd when it is >= 0.
func callHelper(socket string, req helperRequest, fd int) (helperReply, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return helperReply{}, fmt.Errorf("the USB helper is not running (%s): %w", setupHint, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	uc := c.(*net.UnixConn)
	b, _ := json.Marshal(req)
	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	if _, _, err := uc.WriteMsgUnix(b, oob, nil); err != nil {
		return helperReply{}, err
	}
	var r helperReply
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		return helperReply{}, fmt.Errorf("the USB helper did not answer: %w", err)
	}
	if r.Error != "" {
		return r, errors.New(r.Error)
	}
	return r, nil
}

// RunHelper serves uid on socket until the listener fails. It must run as root.
func RunHelper(uid int, socket string) error {
	if os.Geteuid() != 0 {
		return errors.New("mobd usb-helper must run as root; it is started by `sudo mobd usb setup`")
	}
	v := usbip.VHCI{}
	if !v.Available() {
		return errors.New("vhci_hcd is not loaded (modprobe vhci-hcd)")
	}
	_ = os.MkdirAll(filepath.Dir(socket), 0o755)
	_ = os.Remove(socket)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer ln.Close()
	// Only the user may connect; the peer check below is the authority, this keeps others out early.
	if err := os.Chown(socket, uid, -1); err != nil {
		return err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	log.Printf("usb helper: attaching for uid %d on %s", uid, socket)
	h := &helper{uid: uid, v: v, owned: map[int]bool{}}
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return err
		}
		go h.serve(c)
	}
}

type helper struct {
	uid   int
	v     usbip.VHCI
	mu    sync.Mutex
	owned map[int]bool // ports this helper attached
}

func (h *helper) serve(c *net.UnixConn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	reply := func(r helperReply) { _ = json.NewEncoder(c).Encode(r) }
	if uid, err := peerUID(c); err != nil || (uid != h.uid && uid != 0) {
		log.Printf("usb helper: refused a connection from uid %d", uid)
		reply(helperReply{Error: "not allowed"})
		return
	}
	buf := make([]byte, 1024)
	oob := make([]byte, unix.CmsgSpace(4*4))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}
	var fds []int
	if msgs, err := unix.ParseSocketControlMessage(oob[:oobn]); err == nil {
		for _, m := range msgs {
			if got, err := unix.ParseUnixRights(&m); err == nil {
				fds = append(fds, got...)
			}
		}
	}
	defer func() {
		for _, fd := range fds {
			unix.Close(fd) // the kernel keeps its own reference after attach
		}
	}()
	var req helperRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		reply(helperReply{Error: "bad request"})
		return
	}
	switch req.Op {
	case "attach":
		if len(fds) != 1 {
			reply(helperReply{Error: "attach needs exactly one socket"})
			return
		}
		if err := checkSocket(fds[0]); err != nil {
			reply(helperReply{Error: err.Error()})
			return
		}
		switch req.Speed {
		case usbip.SpeedLow, usbip.SpeedFull, usbip.SpeedHigh, usbip.SpeedSuper:
		default:
			reply(helperReply{Error: "unsupported speed"})
			return
		}
		h.mu.Lock()
		port, err := h.v.Attach(fds[0], usbip.Device{BusNum: req.DevID >> 16, DevNum: req.DevID & 0xffff, Speed: req.Speed})
		if err == nil {
			h.owned[port] = true
		}
		h.mu.Unlock()
		if err != nil {
			reply(helperReply{Error: err.Error()})
			return
		}
		log.Printf("usb helper: attached port %d for uid %d", port, h.uid)
		reply(helperReply{Port: port})
	case "detach":
		h.mu.Lock()
		owned := h.owned[req.Port]
		delete(h.owned, req.Port)
		h.mu.Unlock()
		if !owned {
			reply(helperReply{Error: fmt.Sprintf("port %d was not attached by mobd", req.Port)})
			return
		}
		if err := h.v.Detach(req.Port); err != nil {
			reply(helperReply{Error: err.Error()})
			return
		}
		log.Printf("usb helper: detached port %d", req.Port)
		reply(helperReply{})
	default:
		reply(helperReply{Error: "unknown operation"})
	}
}

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) { cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return -1, err
	}
	if serr != nil {
		return -1, serr
	}
	return int(cred.Uid), nil
}

// checkSocket accepts only a stream socket: vhci_hcd needs one, and anything else is a mistake.
func checkSocket(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return errors.New("attach needs a socket")
	}
	typ, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || typ != unix.SOCK_STREAM {
		return errors.New("attach needs a stream socket")
	}
	return nil
}
