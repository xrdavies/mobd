package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeShell records commands and answers them from a table.
type fakeShell struct {
	paths   map[string]string
	outputs map[string]string
	fails   map[string]bool
	ran     []string
}

func (f *fakeShell) LookPath(name string) (string, error) {
	if p, ok := f.paths[name]; ok {
		return p, nil
	}
	return "", errors.New("not found")
}

func (f *fakeShell) Output(name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.ran = append(f.ran, line)
	if f.fails[line] {
		return []byte("permission denied"), errors.New("exit status 1")
	}
	return []byte(f.outputs[line]), nil
}

func (f *fakeShell) Run(name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	f.ran = append(f.ran, "run: "+line)
	if f.fails[line] {
		return errors.New("exit status 1")
	}
	return nil
}

func testRemote(t *testing.T, sh *fakeShell, input string) (*remoteCLI, *strings.Builder, *[]string) {
	t.Helper()
	out := &strings.Builder{}
	var probed []string
	c := &remoteCLI{out: out, in: bufio.NewReader(strings.NewReader(input)), sh: sh,
		statePath: filepath.Join(t.TempDir(), "remote.json"), listen: "0.0.0.0:8080"}
	c.probe = func(base string) error {
		probed = append(probed, base)
		return nil
	}
	c.secret = func(string) (string, error) { return "", errors.New("no token") }
	return c, out, &probed
}

func TestLinkUsesTheSavedRemoteAddressElseTheLAN(t *testing.T) {
	sh := &fakeShell{outputs: map[string]string{"scutil --get LocalHostName": "office-mac\n"}}
	c, _, _ := testRemote(t, sh, "")
	addr, kind, err := c.linkAddress(false)
	if err != nil || !strings.HasPrefix(addr, "http://") || !strings.HasSuffix(addr, ":8080") || !strings.HasPrefix(kind, "LAN") {
		t.Fatalf("no remote: %q %q %v", addr, kind, err)
	}
	if runtime.GOOS == "darwin" && addr != "http://office-mac.local:8080" {
		t.Fatalf("macOS uses the .local name: %q", addr)
	}
	c.save(remoteState{Method: "tailscale", URL: "https://office-mac.tail.ts.net"})
	if addr, kind, _ := c.linkAddress(false); addr != "https://office-mac.tail.ts.net" || kind != "remote, Tailscale Serve" {
		t.Fatalf("saved: %q %q", addr, kind)
	}
	if addr, _, _ := c.linkAddress(true); !strings.HasPrefix(addr, "http://") {
		t.Fatalf("--lan: %q", addr)
	}
	// A mobd that only listens on loopback cannot be paired over the LAN: say so.
	c.listen = "127.0.0.1:8080"
	if _, _, err := c.linkAddress(true); err == nil || !strings.Contains(err.Error(), `"0.0.0.0:8080"`) {
		t.Fatalf("loopback: %v", err)
	}
}

const tsRunning = `{"BackendState":"Running","CertDomains":["office-mac.tail49e461.ts.net"],"Self":{"DNSName":"office-mac.tail49e461.ts.net."}}`

func TestTailscaleSetupServesChecksAndSaves(t *testing.T) {
	sh := &fakeShell{paths: map[string]string{"tailscale": "/usr/local/bin/tailscale"},
		outputs: map[string]string{"/usr/local/bin/tailscale status --json": tsRunning},
		// The Homebrew daemon's CLI needs root: the plain run fails, then sudo is offered.
		fails: map[string]bool{"/usr/local/bin/tailscale serve --bg --https=443 http://127.0.0.1:8080": true}}
	c, out, probed := testRemote(t, sh, "1\ny\ny\n")
	if err := c.setup("", "", ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := sh.ran[len(sh.ran)-1]; got != "run: sudo /usr/local/bin/tailscale serve --bg --https=443 http://127.0.0.1:8080" {
		t.Fatalf("ran %q", sh.ran)
	}
	if len(*probed) != 1 || (*probed)[0] != "https://office-mac.tail49e461.ts.net" {
		t.Fatalf("probed %v", *probed)
	}
	st, _ := c.load()
	if st == nil || st.Method != "tailscale" || st.URL != "https://office-mac.tail49e461.ts.net" || !strings.Contains(out.String(), "Next: run mobd link") {
		t.Fatalf("saved %+v\n%s", st, out)
	}
	info, _ := os.Stat(c.statePath)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestTailscaleSetupExplainsWhatIsMissing(t *testing.T) {
	c, _, _ := testRemote(t, &fakeShell{}, "")
	if err := c.setup("tailscale", "", ""); err == nil || !strings.Contains(err.Error(), "tailscale.com/download") {
		t.Fatalf("not installed: %v", err)
	}
	sh := &fakeShell{paths: map[string]string{"tailscale": "ts"}, outputs: map[string]string{"ts status --json": `{"BackendState":"NeedsLogin"}`}}
	c, _, _ = testRemote(t, sh, "")
	if err := c.setup("tailscale", "", ""); err == nil || !strings.Contains(err.Error(), "NeedsLogin") {
		t.Fatalf("signed out: %v", err)
	}
	sh.outputs["ts status --json"] = `{"BackendState":"Running"}`
	c, out, _ := testRemote(t, sh, "")
	c.yes = true
	if err := c.setup("tailscale", "", ""); err == nil || !strings.Contains(out.String(), "login.tailscale.com/admin/dns") {
		t.Fatalf("no HTTPS: %v\n%s", err, out)
	}
	if _, err := os.Stat(c.statePath); err == nil {
		t.Fatal("saved without a working address")
	}
}

func TestCloudflareSetupInstallsTheTunnelAndChecksTheHostname(t *testing.T) {
	sh := &fakeShell{paths: map[string]string{"brew": "/opt/homebrew/bin/brew"}}
	c, out, probed := testRemote(t, sh, "https://Mob.Example.com/\ny\ny\n")
	token := "eyJhIjoiMTIzIiwidCI6IjQ1NiIsInMiOiI3ODkifQ"
	c.secret = func(string) (string, error) { return token, nil }
	// cloudflared is not installed; it appears once brew has installed it.
	c.sh = &installingShell{fakeShell: sh}
	if err := c.setup("cloudflare", "", ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{"run: /opt/homebrew/bin/brew install cloudflared", "run: sudo /opt/homebrew/bin/cloudflared service install " + token}
	if strings.Join(sh.ran, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %q", sh.ran)
	}
	if strings.Contains(out.String(), token) {
		t.Fatal("the token was printed")
	}
	if st, _ := c.load(); st == nil || st.URL != "https://mob.example.com" || (*probed)[0] != "https://mob.example.com" {
		t.Fatalf("saved %+v probed %v", st, *probed)
	}
	for _, bad := range []string{"", "localhost", "mob.example.com/path"} {
		if _, err := cloudflareHostname(bad); err == nil {
			t.Fatalf("accepted hostname %q", bad)
		}
	}
}

// installingShell makes cloudflared exist once brew installed it.
type installingShell struct{ *fakeShell }

func (s *installingShell) Run(name string, args ...string) error {
	if name == "/opt/homebrew/bin/brew" {
		s.paths["cloudflared"] = "/opt/homebrew/bin/cloudflared"
	}
	return s.fakeShell.Run(name, args...)
}

func TestStatusChecksTheAddressAndOffRemovesIt(t *testing.T) {
	sh := &fakeShell{paths: map[string]string{"cloudflared": "cf"}}
	c, out, _ := testRemote(t, sh, "y\n")
	if err := c.status(); err != nil || !strings.Contains(out.String(), "not set up") {
		t.Fatalf("nothing: %v %s", err, out)
	}
	c.save(remoteState{Method: "cloudflare", URL: "https://mob.example.com"})
	c.probe = func(string) error {
		return errors.New("https://mob.example.com answers HTTP 530, but not as this mobd")
	}
	out.Reset()
	if err := c.status(); err == nil || !strings.Contains(out.String(), "1033") {
		t.Fatalf("down: %v %s", err, out)
	}
	if err := c.off(); err != nil || sh.ran[len(sh.ran)-1] != "run: sudo cf service uninstall" {
		t.Fatalf("off: %v %q", err, sh.ran)
	}
	if st, _ := c.load(); st != nil {
		t.Fatal("still saved")
	}
}
