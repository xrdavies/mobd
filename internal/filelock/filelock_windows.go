//go:build windows

package filelock

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// TryLock takes an exclusive lock on f without waiting (LockFileEx over the whole file).
func TryLock(f *os.File) error {
	var ol windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, math.MaxUint32, math.MaxUint32, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrLocked
	}
	return err
}

// Unlock releases a lock taken by TryLock.
func Unlock(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, math.MaxUint32, math.MaxUint32, &ol)
}
