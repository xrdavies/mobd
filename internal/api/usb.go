package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/coder/websocket"

	"github.com/xrdavies/mobd/internal/pairing"
	"github.com/xrdavies/mobd/internal/usb"
	"github.com/xrdavies/mobd/internal/usbip"
)

// USB redirection (docs/usb-redirection.md): a paired client shares one of its USB devices with
// this computer. Every operation is scoped to the calling client: another client's session is
// "not found", never visible or releasable.

func (s *Server) handleUSB(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"importer": s.usb.Readiness(), "sessions": nonNil(s.usb.List(pairing.ClientID(r.Context())))})
}

func nonNil(v []usb.Session) []usb.Session {
	if v == nil {
		return []usb.Session{}
	}
	return v
}

func (s *Server) handleUSBCreate(w http.ResponseWriter, r *http.Request) {
	var d usb.Device
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sess, err := s.usb.Create(pairing.ClientID(r.Context()), d)
	if err != nil {
		writeUSBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleUSBGet(w http.ResponseWriter, r *http.Request) {
	sess, err := s.usb.Get(pairing.ClientID(r.Context()), r.PathValue("id"))
	if err != nil {
		writeUSBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// handleUSBRelease detaches the device here, then closes its tunnel. ?force=true detaches a
// device that is still in use (a mounted drive), which can lose writes.
func (s *Server) handleUSBRelease(w http.ResponseWriter, r *http.Request) {
	sess, err := s.usb.Release(pairing.ClientID(r.Context()), r.PathValue("id"), r.URL.Query().Get("force") == "true")
	if err != nil {
		writeUSBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// handleUSBTunnel carries the session's USB/IP stream: binary messages both ways, any split. mobd
// sends text messages {"t":"state", ...session} when the state changes. Native clients only: the
// credential is in the Authorization header.
func (s *Server) handleUSBTunnel(w http.ResponseWriter, r *http.Request) {
	client, id := pairing.ClientID(r.Context()), r.PathValue("id")
	if _, err := s.usb.Get(client, id); err != nil {
		writeUSBErr(w, err)
		return
	}
	// Ends when the client's pairing is revoked, as well as with the connection.
	ctx, done, err := s.pairing.Attach(r.Context(), client, http.MethodGet, "/usb/tunnel")
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	defer done()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(usbip.MaxTransfer + 64<<10)
	if err := s.usb.Serve(ctx, client, id, wsTunnel{c}); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

type wsTunnel struct{ c *websocket.Conn }

func (t wsTunnel) Read(ctx context.Context) ([]byte, error) {
	for {
		typ, b, err := t.c.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ == websocket.MessageBinary {
			return b, nil
		}
	}
}

func (t wsTunnel) Write(ctx context.Context, b []byte) error {
	return t.c.Write(ctx, websocket.MessageBinary, b)
}

func (t wsTunnel) Status(ctx context.Context, s usb.Session) error {
	b, _ := json.Marshal(struct {
		T string `json:"t"`
		usb.Session
	}{"state", s})
	return t.c.Write(ctx, websocket.MessageText, b)
}

func (t wsTunnel) Ping(ctx context.Context) error { return t.c.Ping(ctx) }

func writeUSBErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, usb.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, usb.ErrInvalidBus):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, usb.ErrNotReady):
		writeErrCode(w, http.StatusServiceUnavailable, "usb_not_ready", err.Error())
	case errors.Is(err, usb.ErrBusy):
		writeErrCode(w, http.StatusConflict, "usb_busy", err.Error())
	case errors.Is(err, usb.ErrDuplicate), errors.Is(err, usb.ErrTooMany), errors.Is(err, usb.ErrState):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}
