//go:build !linux

package proc

import (
	"errors"
	"os"
	"os/exec"
)

// IntoCgroup fails: cgroups are Linux only, and only the Linux desktop manager asks for one.
func IntoCgroup(*exec.Cmd, string) (*os.File, error) {
	return nil, errors.New("cgroups are only available on Linux")
}

// SetOOMScoreAdj does nothing outside Linux.
func SetOOMScoreAdj(int, int) error { return nil }
