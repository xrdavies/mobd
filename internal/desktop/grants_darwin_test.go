package desktop

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func swapFolders(t *testing.T, dirs ...string) {
	t.Helper()
	probe, list, read, wait := fullDiskProbe, protectedFolders, folderRead, folderWait
	t.Cleanup(func() { fullDiskProbe, protectedFolders, folderRead, folderWait = probe, list, read, wait })
	fullDiskProbe = filepath.Join(t.TempDir(), "no-full-disk-access")
	protectedFolders = func() []string { return dirs }
	folderWait = 50 * time.Millisecond
}

// The folders grant: a denied folder is missing, an unanswered prompt is waiting, and a read that
// outlives the check answers the next one.
func TestFilesGrant(t *testing.T) {
	root := t.TempDir()
	open, denied := filepath.Join(root, "Desktop"), filepath.Join(root, "Documents")
	for _, dir := range []string{open, denied} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(denied, 0700) })
	swapFolders(t, open, filepath.Join(root, "Downloads")) // Downloads does not exist: nothing to protect
	if g := filesGrant(); g.State != GrantGranted || g.Required {
		t.Fatalf("readable: %+v", g)
	}
	swapFolders(t, open, denied)
	if g := filesGrant(); g.State != GrantMissing || !strings.Contains(g.Detail, "Documents") || strings.Contains(g.Detail, "Desktop") {
		t.Fatalf("denied: %+v", g)
	}

	swapFolders(t, open)
	answer := make(chan error)
	folderRead = func(string) error { return <-answer }
	if g := filesGrant(); g.State != GrantWaiting || !strings.Contains(g.Detail, "Desktop") {
		t.Fatalf("prompt open: %+v", g)
	}
	if g := filesGrant(); g.State != GrantWaiting {
		t.Fatalf("still open: %+v", g) // the second check waits on the same read
	}
	answer <- nil
	if g := filesGrant(); g.State != GrantGranted {
		t.Fatalf("answered: %+v", g)
	}

	// Full Disk Access covers every folder without reading them.
	swapFolders(t, denied)
	fullDiskProbe = filepath.Join(root, "probe")
	os.WriteFile(fullDiskProbe, nil, 0600)
	if g := filesGrant(); g.State != GrantGranted || g.Detail != "Full Disk Access" {
		t.Fatalf("full disk access: %+v", g)
	}
}

func TestLocalNetworkGrant(t *testing.T) {
	gateway, send := defaultGateway, sendLocal
	t.Cleanup(func() { defaultGateway, sendLocal = gateway, send })
	for _, tc := range []struct {
		name    string
		gateway string
		err     error
		state   string
	}{
		{"allowed", "192.168.1.1", nil, GrantGranted},
		{"blocked", "10.0.0.1", syscall.EHOSTUNREACH, GrantMissing},
		{"other error", "192.168.1.1", syscall.ENETDOWN, GrantUnknown},
		{"no local router", "100.64.0.1", nil, GrantUnknown},
		{"no route", "", nil, GrantUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := false
			defaultGateway = func() (net.IP, error) {
				if tc.gateway == "" {
					return nil, errors.New("no gateway")
				}
				return net.ParseIP(tc.gateway), nil
			}
			sendLocal = func(net.IP) error { sent = true; return tc.err }
			if g := localNetworkGrant(); g.State != tc.state || g.Detail == "" {
				t.Fatalf("%+v", g)
			}
			if sent != (tc.state != GrantUnknown || tc.err != nil) {
				t.Fatalf("sent %v", sent)
			}
		})
	}
}

// The real router lookup on this Mac, where there is one.
func TestDefaultGateway(t *testing.T) {
	ip, err := defaultGateway()
	if err != nil {
		t.Skip("no default route:", err)
	}
	if ip == nil {
		t.Fatal("nil gateway")
	}
}

func TestRequestGrantNames(t *testing.T) {
	swapFolders(t)
	if err := Current().(GrantSource).RequestGrant("files"); err != nil {
		t.Fatal(err)
	}
	if err := Current().(GrantSource).RequestGrant("camera"); err == nil {
		t.Fatal("unknown permission accepted")
	}
}
