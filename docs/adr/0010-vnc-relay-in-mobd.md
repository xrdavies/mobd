# ADR 0010: VNC WebSocket relay in mobd; macOS Screen Sharing stays the VNC server

## Status

Accepted and implemented (2026-10-13): `GET /vnc` in mobd, App default `<mobd URL>/vnc`.

## Context

Browsers cannot open TCP sockets, so the live view needs a WebSocket ↔ TCP bridge in front of the office Mac's VNC server. Until now that was a separate **websockify** process (Python) started by `scripts/vnc.sh` on its own port (`:6080`), and every App machine needed two URLs: mobd and the VNC WebSocket.

- Two office processes, two ports to open on the LAN/VPN, a Python dependency, and a second URL for users to configure.
- `:6080` sat outside mobd's CORS rules and, later, its auth.
- mobd could not see live-view sessions.

The bridge only copies bytes; it never interprets RFB. The VNC server itself is macOS Screen Sharing, which is built in, maintained by Apple, and supports the macOS user sign-in (Apple Remote Desktop, security type 30) that lands in the user's session (svnc 0.2.0).

## Decision

- mobd serves a **WebSocket relay at `GET /vnc`** on its existing port, connecting to `MOBD_VNC_ADDR` (default `127.0.0.1:5900`). It copies bytes both ways without inspecting them, so every RFB security type passes through.
- Browser origins are checked against `MOBD_CORS_ORIGINS` (or same-origin); non-browser clients are allowed. If the VNC server is unreachable, the request fails with HTTP 502 and a hint to turn on Screen Sharing. Relays close when mobd shuts down.
- `/health` advertises the `vncProxy` feature, always checks the VNC server, and reports `vncSessions`.
- The App derives the VNC URL from the mobd URL (`http→ws`, `https→wss`, `+ /vnc`). The per-machine VNC URL remains as an optional override for custom setups.
- **macOS Screen Sharing remains the VNC server.** We do not implement our own VNC server; a self-hosted streaming path (WebRTC + ScreenCaptureKit behind `DesktopView`) stays a Phase 3 option for latency, not a VNC rewrite.
- **svnc stays a standalone library.** Users of svnc without mob bring their own websockify; mob users only paste the mobd URL.

## Consequences

- One office process and one port; no Python or websockify for mob. The websockify scripts were removed; service management is now built into the `mobd` CLI. Plain svnc users run websockify themselves.
- Screen Sharing still has to be turned on once in System Settings (mobd cannot do it without admin rights); `/health` reports when it is off.
- Live-view traffic shares mobd's port. Current shared-secret checks cover mutating HTTP routes, not `/vnc`; Screen Sharing handles VNC authentication. Transport encryption and network access remain a reverse proxy / VPN concern ([security.md](../security.md)).
