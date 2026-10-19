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

func Handler(store *pairing.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ok": true, "service": "mobd"})
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

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func Call(socket, method, path string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
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
		return fmt.Errorf("contact running mobd via %s: %w", socket, err)
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
