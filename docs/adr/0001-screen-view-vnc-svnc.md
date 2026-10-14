# ADR 0001: Screen view via VNC and svnc

## Status

Accepted

## Context

Operators need to watch the office Mac desktop while an agent works. Cloud desktop products (Manus, Grok Bot) embed that experience but are not suitable for private office intranet deployment. Existing remote-agent tools (e.g. Paseo, Orca) can start agents remotely but do not show the live desktop inside the product.

Sibling project [svnc](https://github.com/xrdavies/svnc) already provides a browser VNC client and related APIs (MIT, npm `@xrdavies/svnc`), and can be modified as a first-party dependency.

## Decision

- Use the **VNC protocol** for Phase 1–2 screen viewing.
- Use **svnc** as the human-facing viewer, with connections in **`viewOnly`** mode.
- Do **not** adopt RustDesk for viewing at this time.
- Introduce a **`DesktopView`** abstraction in MOB clients so a later backend (e.g. WebRTC via ScreenCaptureKit) can replace or sit beside VNC without rewriting task dispatch.

## Consequences

- Viewing depends on a VNC server on the office Mac and on svnc embedding or linking.
- Transport hardening follows intranet / VPN plus WSS or SSH (see `docs/security.md`), not VeNCrypt-first.
- Product can iterate on the viewer UX in-repo via svnc changes.
- Switching view protocols later is intentional and expected; VNC is the default, not a forever lock-in.
