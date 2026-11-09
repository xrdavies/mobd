package desktop

import "context"

// Grant states. A grant is asked for once, on the office computer, when mobd is first started:
// later nobody is at the computer to answer a prompt.
const (
	GrantGranted = "granted"
	GrantMissing = "missing"
	// GrantWaiting: the OS is showing a prompt that nobody has answered yet.
	GrantWaiting = "waiting"
	// GrantRestart: granted, but this process only sees it after mobd restarts.
	GrantRestart = "restart"
	// GrantUnknown: mobd cannot tell (for example, no local network to try).
	GrantUnknown = "unknown"
)

// Grant is one OS permission mobd and the agents it runs need, as this process sees it.
type Grant struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	State string `json:"state"`
	// Required grants stop mobd from working; the others stop some agent work.
	Required bool   `json:"required"`
	Detail   string `json:"detail,omitempty"`
	// Settings opens the System Settings pane where the grant is turned on.
	Settings string `json:"settings,omitempty"`
}

// GrantSource is a backend whose OS permissions are granted interactively (macOS TCC).
type GrantSource interface {
	// Grants checks every permission without showing a prompt that was answered before.
	Grants(ctx context.Context) []Grant
	// RequestGrant shows the OS prompt for one permission; it does not wait for the answer.
	RequestGrant(name string) error
}

// Grants are this OS's permissions, nil where there is nothing to grant.
func Grants(ctx context.Context) []Grant {
	if g, ok := current.(GrantSource); ok {
		return g.Grants(ctx)
	}
	return nil
}

// RequestGrant shows the OS prompt for one of Grants.
func RequestGrant(name string) error {
	if g, ok := current.(GrantSource); ok {
		return g.RequestGrant(name)
	}
	return ErrUnsupported
}
