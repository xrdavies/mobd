//go:build unix

package control

import (
	"errors"
	"os"
	"syscall"
)

// privateDir checks that only this user can reach the socket's directory.
func privateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || int(owner.Uid) != os.Getuid() {
		return errors.New("control socket requires a directory owned by this user with mode 0700")
	}
	return nil
}

func privateSocket(path string) error { return os.Chmod(path, 0600) }
