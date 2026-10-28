package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
)

// remoteState is how devices reach this mobd from outside the office, saved by `mobd remote
// setup` in ~/.mobd/remote.json; `mobd link` pairs with its URL by default.
type remoteState struct {
	Method string `json:"method"` // tailscale or cloudflare
	URL    string `json:"url"`
}

// shell runs the programs remote access needs; tests replace it.
type shell interface {
	LookPath(name string) (string, error)
	// Output runs a command quietly and returns its output.
	Output(name string, args ...string) ([]byte, error)
	// Run runs a command on the terminal (sudo may ask for a password).
	Run(name string, args ...string) error
}

type osShell struct{}

func (osShell) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (osShell) Output(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
func (osShell) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// remoteCLI holds what `mobd remote` and `mobd link` need, so tests can replace each part.
type remoteCLI struct {
	out       io.Writer
	in        *bufio.Reader
	sh        shell
	statePath string
	// listen is mobd's HTTP listen address (from the management socket).
	listen string
	// probe checks that base reaches this mobd.
	probe func(base string) error
	// yes answers every question with yes (--yes).
	yes bool
	// secret reads a value without echoing it.
	secret func(prompt string) (string, error)
	wait   time.Duration
}

func newRemoteCLI(socket string) (*remoteCLI, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	var health struct {
		Listen string `json:"listen"`
	}
	if err := control.Call(socket, "GET", "/health", nil, &health); err != nil {
		return nil, fmt.Errorf("mobd is not running (%w); start it with mobd start", err)
	}
	c := &remoteCLI{out: os.Stdout, in: bufio.NewReader(os.Stdin), sh: osShell{}, statePath: filepath.Join(dir, "remote.json"),
		listen: health.Listen, wait: 3 * time.Second}
	c.probe = func(base string) error { return probeURL(socket, base) }
	c.secret = func(prompt string) (string, error) {
		fmt.Fprint(c.out, prompt)
		if term.IsTerminal(int(os.Stdin.Fd())) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(c.out)
			return strings.TrimSpace(string(b)), err
		}
		line, err := c.in.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	return c, nil
}

// probeURL asks the management socket for a probe code and requests it at base: only this
// mobd answers it with 204.
func probeURL(socket, base string) error {
	var probe struct {
		Code string `json:"code"`
	}
	if err := control.Call(socket, "POST", "/probes", nil, &probe); err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(strings.TrimRight(base, "/") + "/probe/" + probe.Code)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", base, err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s answers HTTP %d, but not as this mobd", base, res.StatusCode)
	}
	return nil
}

func (c *remoteCLI) port() string {
	if _, port, err := net.SplitHostPort(c.listen); err == nil && port != "" {
		return port
	}
	return "8080"
}

func (c *remoteCLI) load() (*remoteState, error) {
	data, err := os.ReadFile(c.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st remoteState
	if err := json.Unmarshal(data, &st); err != nil || st.URL == "" {
		return nil, fmt.Errorf("%s is not valid; run mobd remote off, then mobd remote setup", c.statePath)
	}
	return &st, nil
}

func (c *remoteCLI) save(st remoteState) error {
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(filepath.Dir(c.statePath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.statePath, append(data, '\n'), 0o600)
}

func (c *remoteCLI) ask(prompt string) (string, error) {
	fmt.Fprint(c.out, prompt)
	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no answer (stdin closed); use the setup flags, see mobd remote --help")
	}
	return strings.TrimSpace(line), nil
}

// confirm shows a command and asks before running it.
func (c *remoteCLI) confirm(display string) (bool, error) {
	if c.yes {
		fmt.Fprintf(c.out, "Running: %s\n", display)
		return true, nil
	}
	answer, err := c.ask(fmt.Sprintf("Will run: %s  [Y/n] ", display))
	if err != nil {
		return false, err
	}
	return answer == "" || strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}

// waitFor retries the probe for up to limit: a new Serve or tunnel takes a moment.
func (c *remoteCLI) waitFor(base string, limit time.Duration) error {
	fmt.Fprintf(c.out, "Checking %s …", base)
	deadline := time.Now().Add(limit)
	for {
		err := c.probe(base)
		if err == nil {
			fmt.Fprintln(c.out, " reaches this mobd ✓")
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(c.out, " ✗")
			return err
		}
		fmt.Fprint(c.out, ".")
		time.Sleep(c.wait)
	}
}

// lanURL is this computer's address on the office network: its .local name on macOS (it
// survives a new IP from the router), else its first private IPv4 address.
func (c *remoteCLI) lanURL() (string, error) {
	host, _, _ := net.SplitHostPort(c.listen)
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		return "", fmt.Errorf(`mobd only listens on %s, so other devices cannot reach it: set "listen" to "0.0.0.0:%s" in config.json and run mobd restart, or set up access with mobd remote setup`, c.listen, c.port())
	}
	if runtime.GOOS == "darwin" {
		if out, err := c.sh.Output("scutil", "--get", "LocalHostName"); err == nil && strings.TrimSpace(string(out)) != "" {
			return "http://" + strings.TrimSpace(string(out)) + ".local:" + c.port(), nil
		}
	}
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range ifaces {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
			return "http://" + n.IP.String() + ":" + c.port(), nil
		}
	}
	return "", errors.New("no LAN address found; pass the address with mobd link --url")
}

// linkAddress is what `mobd link` pairs with when --url is not given: the saved remote
// address, or the LAN one (always with lan).
func (c *remoteCLI) linkAddress(lan bool) (address, kind string, err error) {
	if !lan {
		st, err := c.load()
		if err != nil {
			return "", "", err
		}
		if st != nil {
			return st.URL, "remote, " + methodLabel(st.Method), nil
		}
	}
	address, err = c.lanURL()
	return address, "LAN; office network only", err
}

func methodLabel(m string) string {
	switch m {
	case "tailscale":
		return "Tailscale Serve"
	case "cloudflare":
		return "Cloudflare Tunnel"
	}
	return m
}

// status prints the saved remote access and whether it reaches this mobd now.
func (c *remoteCLI) status() error {
	st, err := c.load()
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Fprintln(c.out, "Remote access: not set up. Devices pair over the office network (mobd link).")
		fmt.Fprintln(c.out, "To use Mob from anywhere: mobd remote setup")
		return nil
	}
	fmt.Fprintf(c.out, "Remote access: %s  %s\n", methodLabel(st.Method), st.URL)
	if err := c.probe(st.URL); err != nil {
		fmt.Fprintf(c.out, "Reaches mobd ✗: %v\n", err)
		fmt.Fprintln(c.out, c.hint(st.Method))
		return errors.New("remote access does not reach mobd")
	}
	fmt.Fprintln(c.out, "Reaches mobd ✓")
	return nil
}

func (c *remoteCLI) hint(method string) string {
	switch method {
	case "tailscale":
		if ts, err := c.tailscaleStatus(); err == nil && ts.BackendState != "Running" {
			return "Tailscale is " + ts.BackendState + ": run tailscale up (or open the Tailscale app) and sign in."
		}
		return "Check tailscale serve status shows this address forwarding to http://127.0.0.1:" + c.port() + "."
	case "cloudflare":
		return "Check the tunnel in the Cloudflare dashboard: error 1033 means cloudflared is not running or connected; 502 means its hostname does not point at http://127.0.0.1:" + c.port() + "."
	}
	return ""
}

// tailscaleCLI finds the tailscale command: on PATH, or inside the macOS app.
func (c *remoteCLI) tailscaleCLI() (string, error) {
	for _, name := range []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"} {
		if path, err := c.sh.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Tailscale is not installed: install it from https://tailscale.com/download, sign in, then run mobd remote setup again")
}

type tailscaleStatus struct {
	BackendState string   `json:"BackendState"`
	CertDomains  []string `json:"CertDomains"`
	Self         struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}

func (c *remoteCLI) tailscaleStatus() (tailscaleStatus, error) {
	var st tailscaleStatus
	ts, err := c.tailscaleCLI()
	if err != nil {
		return st, err
	}
	out, err := c.sh.Output(ts, "status", "--json")
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		if err == nil {
			err = jerr
		}
		return st, fmt.Errorf("tailscale status: %v", err)
	}
	return st, nil
}

// runMaybeSudo runs a command, and with sudo when the plain run fails (the tailscale CLI of
// the Homebrew daemon needs root; the macOS app's does not).
func (c *remoteCLI) runMaybeSudo(name string, args ...string) error {
	display := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
	ok, err := c.confirm(display)
	if err != nil || !ok {
		return errors.Join(err, errors.New("cancelled"))
	}
	out, err := c.sh.Output(name, args...)
	if err == nil {
		return nil
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("%s: %v %s", display, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(c.out, "That needs administrator rights: %s\n", strings.TrimSpace(string(out)))
	if ok, err := c.confirm("sudo " + display); err != nil || !ok {
		return errors.Join(err, errors.New("cancelled"))
	}
	return c.sh.Run("sudo", append([]string{name}, args...)...)
}

func (c *remoteCLI) setupTailscale() error {
	ts, err := c.tailscaleCLI()
	if err != nil {
		return err
	}
	for {
		st, err := c.tailscaleStatus()
		if err != nil {
			return err
		}
		if st.BackendState != "Running" {
			return fmt.Errorf("Tailscale is %s: run tailscale up (or open the Tailscale app), sign in, then run mobd remote setup again", st.BackendState)
		}
		fmt.Fprintln(c.out, "Tailscale installed ✓  signed in ✓")
		if len(st.CertDomains) > 0 {
			base := "https://" + strings.TrimSuffix(st.CertDomains[0], ".")
			if err := c.runMaybeSudo(ts, "serve", "--bg", "--https=443", "http://127.0.0.1:"+c.port()); err != nil {
				return err
			}
			if err := c.waitFor(base, 30*time.Second); err != nil {
				return fmt.Errorf("%v\n%s", err, c.hint("tailscale"))
			}
			return c.finish(remoteState{Method: "tailscale", URL: base})
		}
		fmt.Fprintln(c.out, "HTTPS certificates: off ✗")
		fmt.Fprintln(c.out, "  → Turn on MagicDNS and HTTPS Certificates at https://login.tailscale.com/admin/dns")
		if c.yes {
			return errors.New("turn on HTTPS Certificates for your tailnet, then run this again")
		}
		if _, err := c.ask("    then press Enter to check again (Ctrl-C to stop) "); err != nil {
			return err
		}
	}
}

// cloudflareHostname turns what was typed or pasted into a bare hostname.
func cloudflareHostname(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || !strings.Contains(u.Hostname(), ".") || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%q is not a hostname like mob.example.com", strings.TrimSpace(raw))
	}
	return strings.ToLower(u.Hostname()), nil
}

func (c *remoteCLI) setupCloudflare(token, hostname string) error {
	if hostname == "" || token == "" {
		fmt.Fprintln(c.out, "In the Cloudflare dashboard (https://dash.cloudflare.com):")
		fmt.Fprintln(c.out, "  1. Zero Trust → Networks → Tunnels: create a Cloudflared tunnel, environment macOS.")
		fmt.Fprintf(c.out, "  2. Add a public hostname, e.g. mob.example.com, service HTTP, URL 127.0.0.1:%s.\n", c.port())
		fmt.Fprintln(c.out, "  3. Copy the token from the install command it shows.")
	}
	var err error
	if hostname == "" {
		if hostname, err = c.ask("Public hostname: "); err != nil {
			return err
		}
	}
	if hostname, err = cloudflareHostname(hostname); err != nil {
		return err
	}
	if token == "" {
		if token, err = c.secret("Tunnel token (not shown): "); err != nil {
			return err
		}
	}
	if token = strings.TrimSpace(token); len(token) < 20 || strings.ContainsAny(token, " \t") {
		return errors.New("that does not look like a tunnel token; copy it from the dashboard's install command")
	}
	cf, err := c.sh.LookPath("cloudflared")
	if err != nil {
		if err := c.installCloudflared(); err != nil {
			return err
		}
		if cf, err = c.sh.LookPath("cloudflared"); err != nil {
			return errors.New("cloudflared still not found after installing it")
		}
	} else {
		fmt.Fprintln(c.out, "cloudflared installed ✓")
	}
	if ok, err := c.confirm("sudo cloudflared service install ••••••"); err != nil || !ok {
		return errors.Join(err, errors.New("cancelled"))
	}
	if err := c.sh.Run("sudo", cf, "service", "install", token); err != nil {
		return fmt.Errorf("cloudflared service install: %w (if a tunnel service is already installed, run mobd remote off first)", err)
	}
	base := "https://" + hostname
	if err := c.waitFor(base, 90*time.Second); err != nil {
		return fmt.Errorf("%v\n%s", err, c.hint("cloudflare"))
	}
	return c.finish(remoteState{Method: "cloudflare", URL: base})
}

// cloudflaredApt adds Cloudflare's apt repository and installs cloudflared, as
// https://pkg.cloudflare.com documents it for Debian and Ubuntu.
const cloudflaredApt = `mkdir -p --mode=0755 /usr/share/keyrings && ` +
	`curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg && ` +
	`echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' ` +
	`> /etc/apt/sources.list.d/cloudflared.list && apt-get update && apt-get install -y cloudflared`

// installCloudflared installs cloudflared with Homebrew (macOS), else Cloudflare's apt repository
// (Debian, Ubuntu), asking first; other systems get the download page.
func (c *remoteCLI) installCloudflared() error {
	if brew, err := c.sh.LookPath("brew"); err == nil {
		if ok, err := c.confirm("brew install cloudflared"); err != nil || !ok {
			return errors.Join(err, errors.New("cancelled"))
		}
		if err := c.sh.Run(brew, "install", "cloudflared"); err != nil {
			return fmt.Errorf("brew install cloudflared: %w", err)
		}
		return nil
	}
	if _, err := c.sh.LookPath("apt-get"); err == nil {
		fmt.Fprintln(c.out, "cloudflared comes from Cloudflare's apt repository (pkg.cloudflare.com):")
		if ok, err := c.confirm("sudo sh -c \"" + cloudflaredApt + "\""); err != nil || !ok {
			return errors.Join(err, errors.New("cancelled"))
		}
		if err := c.sh.Run("sudo", "sh", "-c", cloudflaredApt); err != nil {
			return fmt.Errorf("installing cloudflared from Cloudflare's apt repository: %w", err)
		}
		return nil
	}
	return errors.New("cloudflared is not installed: install it (https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/), then run this again")
}

func (c *remoteCLI) finish(st remoteState) error {
	if err := c.save(st); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Saved: %s at %s\n", methodLabel(st.Method), st.URL)
	if st.Method == "tailscale" {
		fmt.Fprintln(c.out, "Next: run mobd link and scan it on a device with the Tailscale app on.")
	} else {
		fmt.Fprintln(c.out, "Next: run mobd link and scan it on your device.")
	}
	return nil
}

func (c *remoteCLI) setup(method, token, hostname string) error {
	if st, err := c.load(); err != nil {
		return err
	} else if st != nil {
		fmt.Fprintf(c.out, "Remote access is already set up: %s at %s (mobd remote off removes it).\n", methodLabel(st.Method), st.URL)
		if !c.yes {
			if answer, err := c.ask("Set it up again? [y/N] "); err != nil || !strings.EqualFold(answer, "y") {
				return errors.Join(err, errors.New("cancelled"))
			}
		}
	}
	if method == "" {
		fmt.Fprintln(c.out, "How should devices reach this computer from outside the office?")
		fmt.Fprintln(c.out, "  1) Tailscale Serve    private: only your devices, which need the Tailscale app")
		fmt.Fprintln(c.out, "  2) Cloudflare Tunnel  no app on the phone; needs a Cloudflare account and a domain")
		answer, err := c.ask("> ")
		if err != nil {
			return err
		}
		method = map[string]string{"1": "tailscale", "2": "cloudflare", "tailscale": "tailscale", "cloudflare": "cloudflare"}[strings.ToLower(answer)]
		if method == "" {
			return errors.New("choose 1 or 2")
		}
	}
	if method == "tailscale" {
		return c.setupTailscale()
	}
	return c.setupCloudflare(token, hostname)
}

// off turns the saved remote access off and forgets it; paired devices stay paired.
func (c *remoteCLI) off() error {
	st, err := c.load()
	if err != nil {
		os.Remove(c.statePath)
		return err
	}
	if st == nil {
		fmt.Fprintln(c.out, "Remote access is not set up.")
		return nil
	}
	switch st.Method {
	case "tailscale":
		ts, err := c.tailscaleCLI()
		if err == nil {
			err = c.runMaybeSudo(ts, "serve", "--https=443", "off")
		}
		if err != nil {
			return err
		}
	case "cloudflare":
		if ok, err := c.confirm("sudo cloudflared service uninstall"); err != nil || !ok {
			return errors.Join(err, errors.New("cancelled"))
		}
		cf, err := c.sh.LookPath("cloudflared")
		if err == nil {
			err = c.sh.Run("sudo", cf, "service", "uninstall")
		}
		if err != nil {
			return fmt.Errorf("cloudflared service uninstall: %w", err)
		}
		fmt.Fprintln(c.out, "The tunnel and hostname stay in your Cloudflare account; delete them there if you no longer need them.")
	}
	if err := os.Remove(c.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(c.out, "Remote access is off. Paired devices stay paired; mobd link now uses the office network address.")
	return nil
}

const remoteUsage = `usage:
  mobd remote                      show remote access and check it reaches mobd
  mobd remote setup                set it up (asks: Tailscale Serve or Cloudflare Tunnel)
  mobd remote setup --tailscale [--yes]
  mobd remote setup --cloudflare --hostname mob.example.com [--token TOKEN] [--yes]
  mobd remote off                  turn it off (paired devices stay paired)
Options: --socket PATH (management socket)`

func remoteCommand(args []string) error {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	if action != "" && action != "setup" && action != "off" {
		return errors.New(remoteUsage)
	}
	socket, err := config.ControlSocketDefault()
	if err != nil {
		return err
	}
	if v := os.Getenv("MOBD_CONTROL_SOCKET"); v != "" {
		socket = v
	}
	fs := flag.NewFlagSet("remote", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), remoteUsage) }
	fs.StringVar(&socket, "socket", socket, "Running mobd management socket")
	var ts, cf, yes bool
	var token, hostname string
	if action == "setup" {
		fs.BoolVar(&ts, "tailscale", false, "Use Tailscale Serve")
		fs.BoolVar(&cf, "cloudflare", false, "Use Cloudflare Tunnel")
		fs.StringVar(&token, "token", "", "Cloudflare tunnel token")
		fs.StringVar(&hostname, "hostname", "", "Cloudflare public hostname")
	}
	if action == "setup" || action == "off" {
		fs.BoolVar(&yes, "yes", false, "Run without asking")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || ts && cf {
		return errors.New(remoteUsage)
	}
	c, err := newRemoteCLI(socket)
	if err != nil {
		return err
	}
	c.yes = yes
	switch action {
	case "":
		return c.status()
	case "setup":
		method := ""
		if ts {
			method = "tailscale"
		} else if cf || token != "" || hostname != "" {
			method = "cloudflare"
		}
		return c.setup(method, token, hostname)
	case "off":
		return c.off()
	}
	return errors.New(remoteUsage)
}
