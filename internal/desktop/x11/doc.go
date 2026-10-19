// Package x11 drives one X display (a Linux conversation desktop) as a desktop.Backend: capture,
// screen-change tracking, and later input and the clipboard (docs/linux-design.md, section 2.6).
// It builds on Linux only; this file keeps the package visible to tools elsewhere.
package x11
