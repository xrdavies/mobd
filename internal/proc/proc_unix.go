//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// Group is a command's process group.
type Group struct{ cmd *exec.Cmd }

// NewGroup makes cmd start in a new process group; call it before cmd.Start.
func NewGroup(cmd *exec.Cmd) *Group {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return &Group{cmd: cmd}
}

// Started is called after cmd.Start; nothing to do on Unix.
func (g *Group) Started() error { return nil }

// Kill kills every process in the group.
func (g *Group) Kill() error {
	if g.cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
}

// Close releases the group's resources; nothing to do on Unix.
func (g *Group) Close() error { return nil }

// KillGroup signals every process in the process group pgid.
func KillGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pgid, sig)
}
