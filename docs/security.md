# Security notes

Short guidance aligned with the product design. Details of VNC transport should follow [svnc](https://github.com/xrdavies/svnc) peripheral / deployment docs.

## Deployment posture

- Prefer **office intranet** or **VPN**. Do not expose raw VNC to the public internet.
- For remote access, terminate TLS at a **WSS reverse proxy** or use **SSH** tunneling for VNC. Avoid relying on VeNCrypt as the primary story (same stance as svnc guidance).

## View vs actuation

- **Humans:** VNC sessions via svnc with **`viewOnly: true`**. No mouse/keyboard from the monitor path by default.
- **Agent:** Prefer **local MCP** on the office Mac. If svnc Automation is used as fallback, it must be a **separate** connection/path from the human viewOnly session.

Keeping these channels apart limits accidental remote control and makes auditing clearer.

## Secrets and approvals

- Do not ship credentials in the client. Office-side runner and MCP hold tool credentials as needed.
- Later: require human approval in the control plane before high-risk actions (payments, password managers, destructive system changes).

## Task API auth (deferred)

Phase 1 leaves `MOBD_SHARED_SECRET` **unset**. Rely on intranet/VPN and network posture, not application auth. Shared-secret / session auth and basic audit land with the **home App** in Phase 2 ([client-phase2-lite.md](client-phase2-lite.md), [roadmap.md](roadmap.md)).

## Client trust

Home Mac and iPhone clients are untrusted relative to the office machine: they may dispatch tasks and watch video/framebuffer, but should not become a general remote-desktop console unless an explicit Phase 3 input channel is enabled.
