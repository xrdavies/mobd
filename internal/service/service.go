// Package service runs mobd in the background as the current user: a LaunchAgent on macOS, a
// systemd user unit on Linux. Both share the preflight and the files in config.Dir().
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrdavies/mobd/internal/config"
	"github.com/xrdavies/mobd/internal/control"
)

// Manager starts, stops and inspects the background service.
type Manager interface {
	Start(cfg config.Config, args []string, login bool) error
	Stop() error // stops the service and disables login startup
	// Uninstall stops the service and removes its definition; settings and data stay.
	Uninstall() error
	Status() error
	Logs() error // follows LogPath
	SavedArgs() ([]string, error)
	LoginEnabled() bool
	Dir() string
	LogPath() string
}

// prepared is what prepare leaves for the service definition.
type prepared struct {
	exe, snapshot, logPath string
}

// prepare checks that the endpoints are free, then writes service.json (the config with
// absolute runner paths: services do not load shell profiles), the log file and the saved args.
func prepare(dir string, cfg config.Config, args []string) (prepared, error) {
	var p prepared
	if err := os.MkdirAll(dir, 0700); err != nil {
		return p, err
	}
	// Reject an occupied endpoint before claiming another process is our service.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return p, fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}
	if err := listener.Close(); err != nil {
		return p, err
	}
	if cfg.StreamPort > 0 {
		udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.StreamPort})
		if err != nil {
			return p, fmt.Errorf("live desktop UDP port %d: %w", cfg.StreamPort, err)
		}
		if err := udp.Close(); err != nil {
			return p, err
		}
	}
	// Do not mistake a foreground daemon's management socket for our new service.
	_, release, err := control.Listen(cfg.ControlSocket)
	if err != nil {
		return p, err
	}
	release()
	if p.exe, err = os.Executable(); err != nil {
		return p, err
	}
	// Resolve the CLI in the invoking shell; the service manager does not load shell profiles.
	for _, bin := range []*string{&cfg.CodexBin, &cfg.ClaudeBin} {
		if found, err := exec.LookPath(*bin); err == nil {
			*bin, _ = filepath.Abs(found)
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return p, err
	}
	p.snapshot = filepath.Join(dir, "service.json")
	if err := writePrivate(p.snapshot, data); err != nil {
		return p, err
	}
	p.logPath = filepath.Join(dir, "mobd.log")
	file, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return p, err
	}
	if err := file.Close(); err != nil {
		return p, err
	}
	args, err = absArgs(args)
	if err != nil {
		return p, err
	}
	argsData, err := json.Marshal(args)
	if err != nil {
		return p, err
	}
	return p, writePrivate(filepath.Join(dir, "service-args.json"), argsData)
}

// absArgs makes path flags absolute, so a restart from another directory finds the same files.
func absArgs(args []string) ([]string, error) {
	args = append([]string(nil), args...)
	for i := 0; i < len(args); i++ {
		name, value, equal := strings.Cut(args[i], "=")
		if name != "--config" && name != "--workdir" && name != "--state-path" && name != "--control-socket" {
			continue
		}
		if !equal {
			i++
			if i >= len(args) {
				return nil, errors.New("missing path argument")
			}
			value = args[i]
		}
		value, err := filepath.Abs(value)
		if err != nil {
			return nil, err
		}
		if equal {
			args[i] = name + "=" + value
		} else {
			args[i] = value
		}
	}
	return args, nil
}

// waitHealthy waits for the started service to answer on its management socket.
func waitHealthy(socket, logPath string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := Health(socket); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mobd did not become healthy; inspect %s (service remains loaded; use mobd stop)", logPath)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// savedArgs keeps an explicitly selected config/flags across a plain restart.
func savedArgs(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "service-args.json"))
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

// printStatus prints the running service's endpoints from its snapshot and checks its health.
func printStatus(dir string, login bool) error {
	data, err := os.ReadFile(filepath.Join(dir, "service.json"))
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	fmt.Printf("mobd: loaded; listen=%s; health=%s/health; login=%t\n", cfg.ListenAddr, LocalURL(cfg.ListenAddr), login)
	return Health(cfg.ControlSocket)
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
