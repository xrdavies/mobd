package control

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/xrdavies/mobd/internal/pairing"
)

func TestPrivateManagementLifecycle(t *testing.T) {
	// macOS Unix socket paths must fit in sockaddr_un.
	dir, err := os.MkdirTemp("/tmp", "mobd-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	ln, closeSocket, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSocket()
	if _, release, err := Listen(path); err == nil {
		release()
		t.Fatal("duplicate socket accepted")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("socket is not private")
	}
	auth, err := pairing.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	srv := &http.Server{Handler: Handler(auth)}
	go srv.Serve(ln)
	defer srv.Close()
	var health map[string]any
	if err := Call(path, "GET", "/health", nil, &health); err != nil || health["service"] != "mobd" {
		t.Fatalf("health: %v %v", health, err)
	}
	var invite pairing.Invitation
	if err := Call(path, "POST", "/invitations", map[string]string{"url": "https://office.example"}, &invite); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(invite.URL)
	fragment, _ := url.ParseQuery(u.Fragment)
	c, err := auth.Redeem(fragment.Get("invite"), "phone")
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish, err := auth.Begin(context.Background(), c.Token, "GET", "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	var clients struct {
		Clients     []pairing.Client     `json:"clients"`
		Connections []pairing.Connection `json:"connections"`
	}
	if err := Call(path, "GET", "/clients", nil, &clients); err != nil {
		t.Fatal(err)
	}
	if len(clients.Clients) != 1 || len(clients.Connections) != 1 || clients.Connections[0].ClientID != c.ClientID {
		t.Fatal("missing client or connection")
	}
	if err := Call(path, "POST", "/clients/"+c.ClientID+"/revoke", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("management revoke left connection open")
	}
	if err := Call(path, "DELETE", "/clients/"+c.ClientID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := Call(path, "DELETE", "/clients/"+c.ClientID, nil, nil); err == nil {
		t.Fatal("unknown delete succeeded")
	}
	if err := Call(path, "DELETE", "/invitations", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := Call(path, "POST", "/reset", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRejectPublicOrOccupiedSocketDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, release, err := Listen(filepath.Join(dir, "sock")); err == nil {
		release()
		t.Fatal("public directory accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, release, err := Listen(path); err == nil {
		release()
		t.Fatal("non-socket overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "keep" {
		t.Fatal("existing file modified")
	}
}
