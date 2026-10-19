package service

// New returns the LaunchAgent manager.
func New() (Manager, error) { return newLaunchd() }
