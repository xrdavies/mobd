package desktop

// Linux is a placeholder. The planned backend (docs/desktop-streaming.md): xdg-desktop-portal
// ScreenCast + PipeWire for capture and RemoteDesktop + libei for input on Wayland, X11
// XShm/XTest as a fallback, VAAPI encoding. The portal asks once; keep its restore token.
func newBackend() Backend { return unsupported{name: "linux"} }
