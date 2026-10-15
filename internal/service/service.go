// Package service manages the current user's macOS LaunchAgent.
package service

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
)

const label = "com.xrdavies.mobd"

type Manager struct {
	Dir, Plist, Target string
	run                func(...string) ([]byte, error)
}

func New() (*Manager, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("background service management requires macOS; use mobd serve")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Manager{
		Dir:    filepath.Join(home, ".mobd"),
		Plist:  filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		Target: "gui/" + strconv.Itoa(os.Getuid()) + "/" + label,
		run:    func(args ...string) ([]byte, error) { return exec.Command("/bin/launchctl", args...).CombinedOutput() },
	}, nil
}

func (m *Manager) loaded() bool { _, err := m.run("print", m.Target); return err == nil }

func (m *Manager) Start(cfg config.Config, args []string, login bool) error {
	if m.loaded() {
		return errors.New("mobd service is already loaded; use mobd status or mobd restart")
	}
	if err := os.MkdirAll(m.Dir, 0700); err != nil {
		return err
	}
	// Reject an occupied endpoint before claiming another process is our service.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	if err := listener.Close(); err != nil {
		return err
	}
	// Do not mistake a foreground daemon's management socket for our new service.
	_, release, err := control.Listen(cfg.ControlSocket)
	if err != nil {
		return err
	}
	release()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Resolve the CLI in the invoking shell; launchd does not load shell profiles.
	for _, bin := range []*string{&cfg.CodexBin, &cfg.ClaudeBin} {
		if found, err := exec.LookPath(*bin); err == nil {
			*bin, _ = filepath.Abs(found)
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	snapshot := filepath.Join(m.Dir, "service.json")
	if err := writePrivate(snapshot, data); err != nil {
		return err
	}
	logPath := filepath.Join(m.Dir, "mobd.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	plist := filepath.Join(m.Dir, "mobd.plist")
	if login {
		plist = m.Plist
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
		return err
	}
	args = append([]string(nil), args...)
	for i := 0; i < len(args); i++ {
		name, value, equal := strings.Cut(args[i], "=")
		if name != "--config" && name != "--workdir" && name != "--state-path" && name != "--control-socket" {
			continue
		}
		if !equal {
			i++
			if i >= len(args) {
				return errors.New("missing path argument")
			}
			value = args[i]
		}
		value, err = filepath.Abs(value)
		if err != nil {
			return err
		}
		if equal {
			args[i] = name + "=" + value
		} else {
			args[i] = value
		}
	}
	argsData, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(m.Dir, "service-args.json"), argsData); err != nil {
		return err
	}
	data = agentPlist(exe, snapshot, cfg.CodexWorkDir, logPath, os.Getenv("PATH"))
	if err := writePrivate(plist, data); err != nil {
		return err
	}
	domain := m.Target[:strings.LastIndex(m.Target, "/")]
	if out, err := m.run("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := Health(cfg.ControlSocket); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mobd did not become healthy; inspect %s (service remains loaded; use mobd stop)", logPath)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func writePrivate(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".mobd-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func agentPlist(exe, cfg, workdir, logs, path string) []byte {
	escape := func(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>serve</string><string>--config</string><string>%s</string></array>
<key>WorkingDirectory</key><string>%s</string>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ExitTimeOut</key><integer>30</integer>
</dict></plist>
`, label, escape(exe), escape(cfg), escape(workdir), escape(path), escape(logs), escape(logs)))
}

// SavedArgs keeps an explicitly selected config/flags across a plain restart.
func (m *Manager) SavedArgs() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(m.Dir, "service-args.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var args []string
	err = json.Unmarshal(data, &args)
	return args, err
}

func (m *Manager) LoginEnabled() bool { _, err := os.Stat(m.Plist); return err == nil }

func (m *Manager) Stop() error {
	if m.loaded() {
		if out, err := m.run("bootout", m.Target); err != nil {
			return fmt.Errorf("launchctl bootout: %w: %s", err, out)
		}
	}
	for _, path := range []string{m.Plist, filepath.Join(m.Dir, "mobd.plist")} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (m *Manager) Status() error {
	if !m.loaded() {
		return errors.New("mobd: stopped")
	}
	data, err := os.ReadFile(filepath.Join(m.Dir, "service.json"))
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	fmt.Printf("mobd: loaded; listen=%s; health=%s/health; login=%t\n", cfg.ListenAddr, LocalURL(cfg.ListenAddr), m.LoginEnabled())
	return Health(cfg.ControlSocket)
}

func (m *Manager) Logs() error {
	cmd := exec.Command("/usr/bin/tail", "-n", "100", "-F", filepath.Join(m.Dir, "mobd.log"))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func LocalURL(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func Health(socket string) error {
	var health struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
	}
	if err := control.Call(socket, "GET", "/health", nil, &health); err != nil {
		return err
	}
	if !health.OK || health.Service != "mobd" {
		return errors.New("mobd health check failed")
	}
	return nil
}
