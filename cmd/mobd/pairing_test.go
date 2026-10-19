package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/pairing"
)

func TestLinkWarnsForHTTPAndStillIssuesUsableInvitation(t *testing.T) {
	// Keep the Unix socket path within macOS sockaddr_un limits.
	dir, err := os.MkdirTemp("/tmp", "mobd-link-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	auth, err := pairing.Open(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	socket := filepath.Join(dir, "control.sock")
	listener, release, err := control.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	server := &http.Server{Handler: control.Handler(control.Deps{Pairing: auth})}
	go server.Serve(listener)
	defer server.Close()
	for _, address := range []string{"http://192.168.1.20:8080", "http://100.101.102.103:8080", "http://office-mac:8080", "http://[fd00::20]:8080", "http://127.0.0.1:8080", "https://office-mac.example.ts.net"} {
		t.Run(address, func(t *testing.T) {
			stdout, err := os.CreateTemp(dir, "stdout-")
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			stderr, err := os.CreateTemp(dir, "stderr-")
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			originalOut, originalErr := os.Stdout, os.Stderr
			defer func() { os.Stdout, os.Stderr = originalOut, originalErr }()
			os.Stdout, os.Stderr = stdout, stderr
			err = run([]string{"link", "--url", address, "--socket", socket})
			os.Stdout, os.Stderr = originalOut, originalErr
			if err != nil {
				t.Fatal(err)
			}
			output, err := os.ReadFile(stdout.Name())
			if err != nil {
				t.Fatal(err)
			}
			warning, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(address, "http://") {
				if !strings.Contains(string(warning), "Warning: HTTP does not encrypt") || !strings.Contains(string(warning), "device credentials") {
					t.Fatalf("missing HTTP warning: %s", warning)
				}
			} else if len(warning) != 0 {
				t.Fatalf("unexpected HTTPS warning: %s", warning)
			}
			if strings.Contains(string(output), "Warning:") || !strings.Contains(string(output), "Expires:") || !strings.ContainsAny(string(output), "█▀▄") {
				t.Fatal("missing QR/expiry or warning mixed into stdout")
			}
			var link string
			for _, line := range strings.Split(string(output), "\n") {
				if strings.HasPrefix(line, address+"/pair#") {
					link = line
					break
				}
			}
			if link == "" {
				t.Fatal("invitation did not preserve origin")
			}
			u, err := url.Parse(link)
			if err != nil {
				t.Fatal(err)
			}
			fragment, err := url.ParseQuery(u.Fragment)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := auth.Redeem(fragment.Get("invite"), "phone"); err != nil {
				t.Fatalf("unusable invitation: %v", err)
			}
		})
	}
}
