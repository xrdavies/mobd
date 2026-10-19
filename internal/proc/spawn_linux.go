//go:build linux

package proc

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Exec is the real Spawner.
type Exec struct{}

func (Exec) Start(s Spec) (int, func() error, error) {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Env, cmd.Dir = s.Env, s.Dir
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: s.NewSession}
	if s.CgroupDir != nil {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(s.CgroupDir.Fd())
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	pid := cmd.Process.Pid
	if s.OOMScoreAdj != 0 {
		if err := SetOOMScoreAdj(pid, s.OOMScoreAdj); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return 0, nil, err
		}
	}
	return pid, cmd.Wait, nil
}

// IntoCgroup makes cmd start inside the cgroup directory dir (CLONE_INTO_CGROUP). The caller
// closes the returned directory once cmd has started.
func IntoCgroup(cmd *exec.Cmd, dir string) (*os.File, error) {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("cgroup: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(f.Fd())
	return f, nil
}

// SetOOMScoreAdj sets how readily the kernel's OOM killer picks pid (and what it starts later).
func SetOOMScoreAdj(pid, adj int) error {
	if err := os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte(strconv.Itoa(adj)), 0); err != nil {
		return fmt.Errorf("oom_score_adj: %w", err)
	}
	return nil
}
