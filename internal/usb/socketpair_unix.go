//go:build unix

package usb

import (
	"os"

	"golang.org/x/sys/unix"
)

// socketPair connects the OS importer's end to the tunnel's, with no listening port another local
// process could take. Both ends use Go's poller, so deadlines and Close interrupt them.
func socketPair() (kernel, bridge *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd) // SOCK_CLOEXEC is not on macOS
		if err := unix.SetNonblock(fd, true); err != nil {
			unix.Close(fds[0])
			unix.Close(fds[1])
			return nil, nil, err
		}
	}
	return os.NewFile(uintptr(fds[0]), "usbip-importer"), os.NewFile(uintptr(fds[1]), "usbip-tunnel"), nil
}
