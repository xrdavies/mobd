package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/xrdavies/mobd/internal/config"
)

func TestCLIRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{{"wat"}, {"mcp", "extra"}, {"status", "--listen", ":9000"}, {"serve", "--unknown"}, {"serve", "unexpected"}, {"link", "--close", "--url", "https://office.example"}, {"link", "--lan", "--url", "https://office.example"}, {"remote", "bogus"}, {"remote", "setup", "--tailscale", "--cloudflare"}, {"remote", "--yes"}, {"clients"}, {"clients", "revoke"}, {"clients", "delete"}, {"clients", "unknown"}, {"auth", "reset"}, {"auth", "reset", "--yes", "extra"}, {"connections", "extra"}} {
		if err := run(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--help"}); err != nil {
		t.Fatal(err)
	}
}

func TestOccupiedPortDoesNotTouchState(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	original := []byte("existing history")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := serve(config.Config{ListenAddr: listener.Addr().String(), StatePath: path}); err == nil {
		t.Fatal("occupied port accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatal("state changed")
	}
}
