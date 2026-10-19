//go:build linux

package proc

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// Watch returns a channel closed when pid exits. It works for processes mobd did not start, as
// after a restart (pidfd_open and poll, Linux ≥ 5.3). A pid already gone is closed at once;
// when ctx ends first the channel stays open.
func Watch(ctx context.Context, pid int) (<-chan struct{}, error) {
	exited := make(chan struct{})
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		close(exited)
		return exited, nil
	}
	if err != nil {
		return nil, err
	}
	go func() {
		defer unix.Close(fd)
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, int((500 * time.Millisecond).Milliseconds()))
			if n > 0 || (err != nil && !errors.Is(err, unix.EINTR)) {
				close(exited)
				return
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return exited, nil
}
