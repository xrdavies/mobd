package service

// New returns the systemd user unit manager.
func New() (Manager, error) { return newSystemd() }
