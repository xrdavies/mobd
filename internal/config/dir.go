package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Dir holds mobd's own files: config.json, service.json, mobd.log and run/.
// ~/.mobd on macOS and Linux, %LOCALAPPDATA%\mobd on Windows (a machine-bound daemon, not roaming).
func Dir() (string, error) {
	if runtime.GOOS == "windows" {
		local, err := os.UserCacheDir() // %LOCALAPPDATA%
		return filepath.Join(local, "mobd"), err
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".mobd"), err
}

// ControlSocketDefault is the private management socket's default path.
func ControlSocketDefault() (string, error) {
	dir, err := Dir()
	return filepath.Join(dir, "run", "control.sock"), err
}

// dataHome is the default directory of state.json, and so of the data dir (artifacts, auth,
// desktops). macOS and Windows keep the user config directory, where existing history lives;
// Linux uses $XDG_DATA_HOME, since profiles, caches and downloads are data, not configuration.
func dataHome() (string, error) {
	if runtime.GOOS == "linux" {
		if xdg := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
			return filepath.Join(xdg, "mob"), nil
		}
		home, err := os.UserHomeDir()
		return filepath.Join(home, ".local", "share", "mob"), err
	}
	dir, err := os.UserConfigDir()
	return filepath.Join(dir, "mob"), err
}
