package usb

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	unitPath    = "/etc/systemd/system/mobd-usb-helper@.service"
	modulesPath = "/etc/modules-load.d/mobd-usb.conf"
)

// helperUnit runs one helper per user: mobd-usb-helper@<uid>.service.
func helperUnit(exe string) string {
	return `[Unit]
Description=mobd USB helper for uid %i (attaches shared USB devices)
Documentation=https://github.com/xrdavies/mobd/blob/main/docs/usb-redirection.md

[Service]
ExecStartPre=-/sbin/modprobe vhci-hcd
ExecStart=` + exe + ` usb-helper --uid %i
RuntimeDirectory=mobd-usb
RuntimeDirectoryPreserve=yes
Restart=on-failure
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=read-only
ProtectSystem=full

[Install]
WantedBy=multi-user.target
`
}

// SetupUser is the user `sudo mobd usb setup` sets up: the one who ran sudo.
func SetupUser() (int, error) {
	if os.Geteuid() != 0 {
		return 0, errors.New("run it with sudo: sudo mobd usb setup")
	}
	s := os.Getenv("SUDO_UID")
	uid, err := strconv.Atoi(s)
	if err != nil || uid == 0 {
		return 0, errors.New("run it with sudo as the user that runs mobd, not as root itself")
	}
	return uid, nil
}

// Setup installs and starts the helper for uid, and loads vhci-hcd now and at boot.
func Setup(uid int, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if out, err := exec.Command("modprobe", "vhci-hcd").CombinedOutput(); err != nil {
		return fmt.Errorf("modprobe vhci-hcd: %s: this kernel has no USB/IP importer (CONFIG_USBIP_VHCI_HCD)", strings.TrimSpace(string(out)))
	}
	if err := os.WriteFile(modulesPath, []byte("# mobd: import shared USB devices (docs/usb-redirection.md)\nvhci-hcd\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(helperUnit(exe)), 0o644); err != nil {
		return err
	}
	unit := fmt.Sprintf("mobd-usb-helper@%d.service", uid)
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", unit}, {"restart", unit}} {
		if b, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(b)))
		}
	}
	fmt.Fprintf(out, "USB import is set up for uid %d (%s, using %s).\nUpdating mobd to another path? Run `sudo mobd usb setup` again.\n", uid, unit, exe)
	return nil
}

// Remove stops and removes the helper for uid; the module and the unit stay if others use them.
func Remove(uid int, out io.Writer) error {
	unit := fmt.Sprintf("mobd-usb-helper@%d.service", uid)
	if b, err := exec.Command("systemctl", "disable", "--now", unit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl disable %s: %s", unit, strings.TrimSpace(string(b)))
	}
	matches, _ := filepath.Glob("/etc/systemd/system/multi-user.target.wants/mobd-usb-helper@*.service")
	if len(matches) == 0 {
		_ = os.Remove(unitPath)
		_ = os.Remove(modulesPath)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	}
	fmt.Fprintf(out, "USB import is off for uid %d.\n", uid)
	return nil
}
