package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/xrdavies/mobd/internal/config"
)

const unitName = "mobd.service"

// systemd manages mobd as a systemd user unit (Linux).
type systemd struct {
	dir, unit, user string
	run             func(name string, args ...string) ([]byte, error)
	stderr          io.Writer
}

func newSystemd() (*systemd, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	units, err := os.UserConfigDir() // $XDG_CONFIG_HOME or ~/.config
	if err != nil {
		return nil, err
	}
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	return &systemd{
		dir: dir, unit: filepath.Join(units, "systemd", "user", unitName), user: u.Username,
		run:    func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() },
		stderr: os.Stderr,
	}, nil
}

func (m *systemd) Dir() string     { return m.dir }
func (m *systemd) LogPath() string { return filepath.Join(m.dir, "mobd.log") }

func (m *systemd) systemctl(args ...string) ([]byte, error) {
	return m.run("systemctl", append([]string{"--user"}, args...)...)
}

func (m *systemd) active() bool {
	_, err := m.systemctl("is-active", "--quiet", unitName)
	return err == nil
}

func (m *systemd) Start(cfg config.Config, args []string, login bool) error {
	if m.active() {
		return errors.New("mobd service is already running; use mobd status or mobd restart")
	}
	p, err := prepare(m.dir, cfg, args)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.unit), 0755); err != nil {
		return err
	}
	unit := renderUnit(p.exe, p.snapshot, cfg.CodexWorkDir, p.logPath, os.Getenv("PATH"))
	if err := writePrivate(m.unit, unit); err != nil {
		return err
	}
	if err := os.Chmod(m.unit, 0644); err != nil {
		return err
	}
	if out, err := m.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, out)
	}
	m.linger()
	enable := "disable"
	if login {
		enable = "enable"
	}
	if out, err := m.systemctl(enable, unitName); err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", enable, err, out)
	}
	if out, err := m.systemctl("start", unitName); err != nil {
		return fmt.Errorf("systemctl --user start: %w: %s", err, out)
	}
	return waitHealthy(cfg.ControlSocket, p.logPath)
}

// linger keeps the user manager running without a login session, so mobd (and later its
// desktops) survive the last logout and start at boot. It is never turned off again.
func (m *systemd) linger() {
	if out, err := m.run("loginctl", "show-user", m.user, "--property=Linger", "--value"); err == nil && strings.TrimSpace(string(out)) == "yes" {
		return
	}
	if out, err := m.run("loginctl", "enable-linger", m.user); err != nil {
		fmt.Fprintf(m.stderr, "warning: could not enable linger (%s); mobd stops at logout until you run: sudo loginctl enable-linger %s\n",
			strings.TrimSpace(string(out)), m.user)
		return
	}
	fmt.Fprintln(m.stderr, "linger enabled: mobd keeps running without a login")
}

func (m *systemd) SavedArgs() ([]string, error) { return savedArgs(m.dir) }

func (m *systemd) LoginEnabled() bool {
	_, err := m.systemctl("is-enabled", "--quiet", unitName)
	return err == nil
}

// Stop stops and disables the unit; the unit file stays (removed by uninstall).
func (m *systemd) Stop() error {
	if _, err := os.Stat(m.unit); os.IsNotExist(err) {
		return nil
	}
	if out, err := m.systemctl("disable", "--now", unitName); err != nil {
		return fmt.Errorf("systemctl --user disable --now: %w: %s", err, out)
	}
	return nil
}

// Uninstall stops the unit and removes its file.
func (m *systemd) Uninstall() error {
	if err := m.Stop(); err != nil {
		return err
	}
	if err := os.Remove(m.unit); err != nil && !os.IsNotExist(err) {
		return err
	}
	if out, err := m.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, out)
	}
	return nil
}

func (m *systemd) Status() error {
	if !m.active() {
		return errors.New("mobd: stopped")
	}
	return printStatus(m.dir, m.LoginEnabled())
}

func (m *systemd) Logs() error { return Follow(m.LogPath(), 100, os.Stdout) }
