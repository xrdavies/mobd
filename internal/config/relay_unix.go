//go:build unix

package config

import (
	"errors"
	"os"
	"syscall"
)

func checkSecretFile(st os.FileInfo) error {
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) || (st.Mode().Perm() != 0400 && st.Mode().Perm() != 0600) {
		return errors.New("relay.turn.secretFile must belong to the user running mobd with mode 0600 or 0400")
	}
	return nil
}
