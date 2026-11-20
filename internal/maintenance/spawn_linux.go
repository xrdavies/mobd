package maintenance

import (
	"bytes"
	"fmt"
	"os/exec"
)

// Start runs the helper as a transient systemd user unit, outside mobd's own cgroup.
func (h Helper) Start(id string) error {
	args := append([]string{"--user", "--unit=mobd-maintain-" + id, "--collect", "--quiet",
		"--setenv=PATH=" + h.Path, "--working-directory=" + h.Cwd}, h.args(id)...)
	if out, err := exec.Command("systemd-run", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
