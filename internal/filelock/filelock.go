// Package filelock holds an exclusive, non-blocking lock on an open file, so only one mobd
// uses a state file or management socket at a time. The lock ends with the process.
package filelock

import "errors"

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("file is locked by another process")
