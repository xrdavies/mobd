//go:build windows

package proc

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Group is a job object holding a command and every process it starts. Closing the last handle
// kills them (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE), so a crashed mobd leaves nothing behind.
type Group struct {
	cmd *exec.Cmd
	job windows.Handle
}

// NewGroup prepares cmd; call it before cmd.Start.
func NewGroup(cmd *exec.Cmd) *Group {
	return &Group{cmd: cmd}
}

// Started puts the started process into a new job object. A child the process creates before
// this returns escapes the job (exec has no suspended start); runners start no children that fast.
func (g *Group) Started() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(g.cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return err
	}
	g.job = job
	return nil
}

// Kill terminates every process in the job.
func (g *Group) Kill() error {
	if g.job != 0 {
		return windows.TerminateJobObject(g.job, 1)
	}
	if g.cmd.Process != nil {
		return g.cmd.Process.Kill()
	}
	return nil
}

// Close releases the job handle, which also kills anything still in it.
func (g *Group) Close() error {
	if g.job == 0 {
		return nil
	}
	err := windows.CloseHandle(g.job)
	g.job = 0
	return err
}
