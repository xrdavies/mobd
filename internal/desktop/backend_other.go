//go:build !darwin && !linux

package desktop

import "runtime"

// Other systems (Windows is planned) have no backend yet.
func newBackend() Backend { return unsupported{name: runtime.GOOS} }
