//go:build linux

package proc

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestWatch(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited, err := Watch(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		t.Fatal("closed while running")
	case <-time.After(100 * time.Millisecond):
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("exit not noticed")
	}
	gone, err := Watch(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-gone:
	case <-time.After(time.Second):
		t.Error("a gone pid is not closed at once")
	}
}
