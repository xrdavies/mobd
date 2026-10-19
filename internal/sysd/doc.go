// Package sysd talks to the user's systemd manager over D-Bus for the Linux desktops: one
// transient scope per desktop, freezing, thawing and stopping it (docs/linux-design.md, section
// 2.4). It builds on Linux only; this file keeps the package visible to tools elsewhere.
package sysd
