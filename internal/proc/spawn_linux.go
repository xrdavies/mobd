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
		if err := os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte(strconv.Itoa(s.OOMScoreAdj)), 0); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return 0, nil, fmt.Errorf("oom_score_adj: %w", err)
		}
	}
	return pid, cmd.Wait, nil
}
