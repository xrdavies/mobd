package desktop

import "context"

// Checker is implemented by a Provider or Backend that reports its own readiness items for
// /health (for example Linux: xvfb, windowManager, encoder).
type Checker interface {
	Checks(ctx context.Context) map[string]Check
}

// Check is one readiness item; Unknown means it cannot be told yet.
type Check struct {
	OK      bool
	Unknown bool
	Detail  string
}

// ComputerName is how texts for people and agents name the office computer of a backend.
func ComputerName(backend string) string {
	switch backend {
	case "macos":
		return "Mac"
	case "linux":
		return "Linux computer"
	case "windows":
		return "Windows PC"
	}
	return "computer"
}

// OSName is the operating system of a backend, for messages ("not needed on Linux").
func OSName(backend string) string {
	switch backend {
	case "macos":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	}
	return backend
}
