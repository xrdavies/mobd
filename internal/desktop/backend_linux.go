package desktop

// Linux has no single desktop: each conversation gets its own virtual X display (Xvfb, openbox,
// XShm/XDamage capture, XTest input) from the desktop manager; see docs/linux.md. Until that
// lands, the shared backend is a placeholder and Linux runs turns without a desktop.
func newBackend() Backend { return unsupported{name: "linux"} }
