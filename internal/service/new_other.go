//go:build !darwin && !linux

package service

import "errors"

// New: background service management is not available here yet (Windows: a logon task, planned).
func New() (Manager, error) {
	return nil, errors.New("background service management is not supported on this system yet; use mobd serve")
}
