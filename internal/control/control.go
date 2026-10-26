// Package control provides the user-private management socket; it is never proxied over TCP.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/xrdavies/mobd/internal/desktop"
	"github.com/xrdavies/mobd/internal/filelock"
	"github.com/xrdavies/mobd/internal/pairing"
)

// Listen holds a lock while replacing stale sockets after a crash.
func Listen(path string) (net.Listener, func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err := filelock.TryLock(lock); err != nil {
		lock.Close()
		return nil, nil, errors.New("mobd management socket is already in use")
	}
	unlock := func() { _ = filelock.Unlock(lock); _ = lock.Close() }
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			unlock()
			return nil, nil, errors.New("control socket path is not a socket")
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			conn.Close()
			unlock()
			return nil, nil, errors.New("control socket is active")
		}
		if err := os.Remove(path); err != nil {
			unlock()
			return nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		unlock()
		return nil, nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	if err := privateSocket(path); err != nil {
		ln.Close()
		unlock()
		return nil, nil, err
	}
	return ln, func() { ln.Close(); unlock() }, nil
}

// ErrNotRunning: nothing answers on the control socket.
var ErrNotRunning = errors.New("contact running mobd")

// Desktops is the running server's desktop administration (api.Server).
type Desktops interface {
	PerSessionDesktops() bool
	DesktopsDir() string
	ListDesktops() ([]desktop.Info, desktop.Resources)
	ResolveSession(prefix string) (string, error)
	OpenDesktop(ctx context.Context, id string) (desktop.Info, error)
	CloseDesktop(ctx context.Context, id string, reason desktop.CloseReason) error
	CloseAllDesktops(ctx context.Context, reason desktop.CloseReason) (int, error)
	DesktopDoctor(ctx context.Context) []desktop.DoctorCheck
	SeedDesktop(id string) error
}

// Deps are what the management routes act on; Desktops may be nil (tests).
type Deps struct {
	Pairing  *pairing.Store
	Desktops Desktops
	// Listen is mobd's HTTP listen address, so `mobd link` and `mobd remote` know the port.
	Listen string
}

// DesktopList is GET /desktops on the control socket.
type DesktopList struct {
	PerSession bool               `json:"perSession"`
	Dir        string             `json:"dir"`
	Desktops   []desktop.Info     `json:"desktops"`
	Memory     desktop.MemoryInfo `json:"memory"`
	Disk       desktop.DiskInfo   `json:"disk"`
}

func Handler(deps Deps) http.Handler {
	store := deps.Pairing
	mux := http.NewServeMux()
	if deps.Desktops != nil {
		desktopRoutes(mux, deps.Desktops)
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ok": true, "service": "mobd", "listen": deps.Listen})
	})
	mux.HandleFunc("POST /probes", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 201, map[string]string{"code": store.NewProbe()})
	})
	mux.HandleFunc("POST /invitations", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			reply(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		invite, err := store.Invite(req.URL)
		if err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 201, invite)
	})
	mux.HandleFunc("DELETE /invitations", func(w http.ResponseWriter, r *http.Request) { store.CloseInvitations(); w.WriteHeader(204) })
	mux.HandleFunc("GET /clients", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"clients": store.Clients(), "connections": store.Connections()})
	})
	mux.HandleFunc("GET /connections", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"connections": store.Connections()})
	})
	change := func(remove bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			err := store.Revoke(r.PathValue("id"), remove)
			if errors.Is(err, pairing.ErrNotFound) {
				reply(w, 404, map[string]string{"error": err.Error()})
				return
			}
			if err != nil {
				reply(w, 500, map[string]string{"error": err.Error()})
				return
			}
			w.WriteHeader(204)
		}
	}
	mux.HandleFunc("POST /clients/{id}/revoke", change(false))
	mux.HandleFunc("DELETE /clients/{id}", change(true))
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Reset(); err != nil {
			reply(w, 500, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(204)
	})
	return mux
}

// closeReasons may be given to DELETE /desktops on the control socket.
var closeReasons = map[string]desktop.CloseReason{
	"": desktop.CloseUser, "user": desktop.CloseUser, "stop": desktop.CloseStop, "uninstall": desktop.CloseUninstall,
}

func desktopRoutes(mux *http.ServeMux, d Desktops) {
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusInternalServerError
		var coded interface{ HTTPStatus() int }
		if errors.As(err, &coded) {
			status = coded.HTTPStatus()
		}
		reply(w, status, map[string]string{"error": err.Error()})
	}
	resolve := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		id, err := d.ResolveSession(r.PathValue("id"))
		if err != nil {
			fail(w, err)
		}
		return id, err == nil
	}
	mux.HandleFunc("GET /desktops", func(w http.ResponseWriter, r *http.Request) {
		list, res := d.ListDesktops()
		reply(w, 200, DesktopList{PerSession: d.PerSessionDesktops(), Dir: d.DesktopsDir(), Desktops: list, Memory: res.Memory, Disk: res.Disk})
	})
	mux.HandleFunc("POST /desktops/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := resolve(w, r)
		if !ok {
			return
		}
		info, err := d.OpenDesktop(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, info)
	})
	mux.HandleFunc("DELETE /desktops/{id}", func(w http.ResponseWriter, r *http.Request) {
		reason, ok := closeReasons[r.URL.Query().Get("reason")]
		if !ok {
			reply(w, 400, map[string]string{"error": "reason must be user, stop or uninstall"})
			return
		}
		id, ok := resolve(w, r)
		if !ok {
			return
		}
		if err := d.CloseDesktop(r.Context(), id, reason); err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]string{"sessionId": id})
	})
	mux.HandleFunc("DELETE /desktops", func(w http.ResponseWriter, r *http.Request) {
		reason, ok := closeReasons[r.URL.Query().Get("reason")]
		if !ok || r.URL.Query().Get("all") != "1" {
			reply(w, 400, map[string]string{"error": "use ?all=1 and a reason of user, stop or uninstall"})
			return
		}
		n, err := d.CloseAllDesktops(r.Context(), reason)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]int{"closed": n})
	})
	mux.HandleFunc("POST /desktops/{id}/seed", func(w http.ResponseWriter, r *http.Request) {
		id, ok := resolve(w, r)
		if !ok {
			return
		}
		if err := d.SeedDesktop(id); err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]string{"sessionId": id})
	})
	mux.HandleFunc("GET /doctor", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"checks": d.DesktopDoctor(r.Context())})
	})
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func Call(socket, method, path string, body any, result any) error {
	return CallTimeout(socket, 5*time.Second, method, path, body, result)
}

// CallTimeout is Call for requests that may take longer (starting or closing desktops).
func CallTimeout(socket string, timeout time.Duration, method, path string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(method, "http://mobd"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w via %s: %w", ErrNotRunning, socket, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&problem)
		return fmt.Errorf("management request: %s: %s", res.Status, problem.Error)
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(result)
	}
	return nil
}
