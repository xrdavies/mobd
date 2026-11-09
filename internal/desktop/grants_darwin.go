package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// System Settings panes; the old preference URLs still open the right page.
const (
	settingsAccessibility = "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
	settingsScreen        = "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture"
	settingsFullDisk      = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"
	settingsLocalNetwork  = "x-apple.systempreferences:com.apple.preference.security?Privacy_LocalNetwork"
)

// fullDiskProbe is readable only with Full Disk Access, and reading it never shows a prompt.
var fullDiskProbe = "/Library/Application Support/com.apple.TCC/TCC.db"

// protectedFolders are the home folders macOS asks about one by one; swapped in tests.
var protectedFolders = func() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "Desktop"), filepath.Join(home, "Documents"), filepath.Join(home, "Downloads")}
}

// folderWait is how long a check waits for a folder before reporting the prompt as open.
var folderWait = 300 * time.Millisecond

// Grants checks what the agents mobd runs need besides input and capture: child processes share
// mobd's grants, so mobd asks for them while someone is at the computer.
func (macOS) Grants(ctx context.Context) []Grant {
	ax := Grant{Name: "accessibility", Label: "Accessibility", Required: true, Settings: settingsAccessibility, State: GrantGranted}
	if !postAccess() {
		ax.State, ax.Detail = GrantMissing, "lets mobd and the agent click and type"
	}
	sr := Grant{Name: "screenRecording", Label: "Screen & System Audio Recording", Required: true, Settings: settingsScreen, State: GrantGranted}
	if !screenAccess() {
		sr.State, sr.Detail = GrantMissing, "lets mobd show the desktop and play its sound"
	}
	return []Grant{ax, sr, filesGrant(), localNetworkGrant()}
}

// RequestGrant shows the prompt for one grant. Files and the local network have no request API:
// the first access shows the prompt, so this starts that access.
func (macOS) RequestGrant(name string) error {
	switch name {
	case "accessibility":
		requestPostAccess()
	case "screenRecording":
		requestScreenAccess()
	case "files":
		if !fullDiskAccess() {
			for _, dir := range protectedFolders() {
				probeFolder(dir)
			}
		}
	case "localNetwork":
		go localNetworkGrant()
	default:
		return fmt.Errorf("unknown permission %q", name)
	}
	return nil
}

func fullDiskAccess() bool {
	f, err := os.Open(fullDiskProbe)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func filesGrant() Grant {
	g := Grant{Name: "files", Label: "Desktop, Documents and Downloads folders", Settings: settingsFullDisk, State: GrantGranted}
	if fullDiskAccess() {
		g.Detail = "Full Disk Access"
		return g
	}
	dirs := protectedFolders()
	results := make([]error, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Go(func() { results[i] = folderAccess(dir, folderWait) })
	}
	wg.Wait()
	var denied, waiting []string
	for i, err := range results {
		switch {
		case errors.Is(err, errPromptOpen):
			waiting = append(waiting, filepath.Base(dirs[i]))
		case err != nil:
			denied = append(denied, filepath.Base(dirs[i]))
		}
	}
	switch {
	case len(denied) > 0:
		g.State = GrantMissing
		g.Detail = "no access to " + strings.Join(denied, ", ") + "; turn mobd on in Files and Folders, or give it Full Disk Access"
	case len(waiting) > 0:
		g.State = GrantWaiting
		g.Detail = "answer the prompt about " + strings.Join(waiting, ", ") + " on this computer"
	default:
		g.Detail = "lets the agent work with files there"
	}
	return g
}

// errPromptOpen: the folder is still waiting for an answer to its prompt.
var errPromptOpen = errors.New("waiting for the prompt")

// folders are the folder reads in flight: reading a folder nobody has answered about blocks
// until the prompt is answered, so a read outlives the check that started it.
var folders struct {
	sync.Mutex
	reads map[string]chan error
}

// probeFolder starts reading dir unless a read is already waiting.
func probeFolder(dir string) chan error {
	folders.Lock()
	defer folders.Unlock()
	if folders.reads == nil {
		folders.reads = map[string]chan error{}
	}
	read := folders.reads[dir]
	if read == nil {
		read = make(chan error, 1)
		folders.reads[dir] = read
		go func() { read <- folderRead(dir) }()
	}
	return read
}

// folderAccess reads dir, waiting up to wait for macOS to answer.
func folderAccess(dir string, wait time.Duration) error {
	read := probeFolder(dir)
	select {
	case err := <-read:
		folders.Lock()
		delete(folders.reads, dir)
		folders.Unlock()
		return err
	case <-time.After(wait):
		return errPromptOpen
	}
}

// folderRead lists dir; swapped in tests.
var folderRead = readFolder

func readFolder(dir string) error {
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing there to protect
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// defaultGateway is the router of the local network; swapped in tests.
var defaultGateway = func() (net.IP, error) {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "gateway:"); ok {
			if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errors.New("no gateway")
}

// sendLocal sends one datagram to the discard port of a local address; swapped in tests.
var sendLocal = func(ip net.IP) error {
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: 9})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte{0})
	return err
}

// localNetworkGrant sends to the router: macOS 15 and later show the Local Network prompt on the
// first try and fail sends with "no route to host" until it is allowed. The live view sends to
// phones and computers on the same network, and agents reach local services.
func localNetworkGrant() Grant {
	g := Grant{Name: "localNetwork", Label: "Local Network", Settings: settingsLocalNetwork, State: GrantGranted,
		Detail: "lets the live view and the agent reach devices on this network"}
	ip, err := defaultGateway()
	if err != nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		g.State, g.Detail = GrantUnknown, "no local network router to try; check again on the office network"
		return g
	}
	switch err := sendLocal(ip); {
	case errors.Is(err, syscall.EHOSTUNREACH):
		g.State, g.Detail = GrantMissing, "macOS blocks the local network; allow the prompt, or turn mobd on in Local Network"
	case err != nil:
		g.State, g.Detail = GrantUnknown, err.Error()
	}
	return g
}
