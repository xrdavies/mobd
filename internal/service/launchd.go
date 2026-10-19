package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xrdavies/mobd/internal/config"
)

const label = "com.xrdavies.mobd"

// launchd manages the current user's macOS LaunchAgent.
type launchd struct {
	dir, plist, target string
	run                func(...string) ([]byte, error)
}

func newLaunchd() (*launchd, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return &launchd{
		dir:    dir,
		plist:  filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		target: "gui/" + strconv.Itoa(os.Getuid()) + "/" + label,
		run:    func(args ...string) ([]byte, error) { return exec.Command("/bin/launchctl", args...).CombinedOutput() },
	}, nil
}

func (m *launchd) Dir() string     { return m.dir }
func (m *launchd) LogPath() string { return filepath.Join(m.dir, "mobd.log") }

func (m *launchd) loaded() bool { _, err := m.run("print", m.target); return err == nil }

func (m *launchd) Start(cfg config.Config, args []string, login bool) error {
	if m.loaded() {
		return errors.New("mobd service is already loaded; use mobd status or mobd restart")
	}
	p, err := prepare(m.dir, cfg, args)
	if err != nil {
		return err
	}
	plist := filepath.Join(m.dir, "mobd.plist")
	if login {
		plist = m.plist
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
		return err
	}
	if err := writePrivate(plist, agentPlist(p.exe, p.snapshot, cfg.CodexWorkDir, p.logPath, os.Getenv("PATH"))); err != nil {
		return err
	}
	domain := m.target[:strings.LastIndex(m.target, "/")]
	if out, err := m.run("bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	return waitHealthy(cfg.ControlSocket, p.logPath)
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

func (m *launchd) SavedArgs() ([]string, error) { return savedArgs(m.dir) }

func (m *launchd) LoginEnabled() bool { _, err := os.Stat(m.plist); return err == nil }

func (m *launchd) Stop() error {
	if m.loaded() {
		if out, err := m.run("bootout", m.target); err != nil {
			return fmt.Errorf("launchctl bootout: %w: %s", err, out)
		}
	}
	for _, path := range []string{m.plist, filepath.Join(m.dir, "mobd.plist")} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (m *launchd) Status() error {
	if !m.loaded() {
		return errors.New("mobd: stopped")
	}
	return printStatus(m.dir, m.LoginEnabled())
}

func (m *launchd) Logs() error { return follow(m.LogPath(), 100, os.Stdout) }
