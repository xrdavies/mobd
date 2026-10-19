//go:build unix

package proc

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A child started by the command dies with the group, as when a turn is cancelled.
func TestKillReachesGrandchildren(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	g := NewGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := g.Started(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	child, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("child pid %q: %v", buf[:n], err)
	}
	if err := g.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("grandchild survived")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
