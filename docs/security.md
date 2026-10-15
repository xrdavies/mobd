# Security notes

Access controls and deployment requirements for mobd. See [VNC setup](vnc-svnc-setup.md)
for the live-view connection.

## Deployment posture

- Prefer **office intranet** or **VPN**. Do not expose raw VNC to the public internet.
- For home-to-office access, follow the [remote-access guide](remote-access.md):
  prefer **Tailscale Serve** with restricted tailnet access and a loopback mobd
  listener, or use a trusted **SSH** tunnel. A public HTTPS/WSS proxy also needs
  authentication on every route; TLS alone is not access control.
- Phone access without a VPN requires a public gateway or Tailscale Funnel plus
  authentication on every route; see the [proposed architecture](remote-access.md).
  Until that authentication is implemented, do not publish mobd through Funnel
  or an unauthenticated public tunnel. Funnel provides transport, not application login.
- The planned managed relay must authorize each user/device mapping and isolate
  customers' office machines. A gateway that terminates TLS can read proxied
  data; this topology does not provide end-to-end encryption from client to mobd.
- `mobd init` writes a loopback listen address. Without a config override, the runtime default is `:8080`; choose the bind address explicitly for deployment.

## View vs actuation

- **Humans:** the mob desktop client disables input by default and offers an explicit Control mode. A standalone svnc monitor can use **`viewOnly: true`**. This is client behavior; mobd relays RFB bytes and does not enforce view-only access.
- **Agent:** desktop tools run through **local MCP** (`mobd mcp`) on the office Mac, independently of the human VNC connection.

Keeping these channels apart limits accidental remote control and makes auditing clearer.

## Secrets and approvals

- Do not ship credentials in the client. Office-side runner and MCP hold tool credentials as needed.
- `ask_user` and `request_approval` are available through `mobd mcp`. The question/answer API uses a per-task token for internal calls; pending approvals are denied when unanswered, expired or cancelled.
- Codex and Claude permissions default to `bypass`. Set `codexPermissions` / `claudePermissions` to `approve` for the provider gates described in [runners](runners.md). Codex exec has only the instruction-based MCP gate; app-server supports provider approval requests. These controls do not make arbitrary desktop actions a sandbox.

## Task API auth

`sharedSecret` / `MOBD_SHARED_SECRET` is implemented and optional (empty by default).
When set, creating/cancelling/steering tasks, changing sessions, uploading images
and answering questions require `Authorization: Bearer <token>` or `X-Mob-Token`.
Read routes, SSE, screenshots, artifacts and `/vnc` are **not** covered by this
token check. VNC uses the upstream Screen Sharing authentication; browser origins
are checked separately. Keep the entire daemon within the intended trusted network.
There is no multi-user login system or dedicated security audit log.

## Distributed binaries

The [installer](https://mob-releases.0xd.app/mobd/install.sh) downloads over HTTPS,
checks the archive SHA-256, exact regular-file entries, Developer ID / Team ID,
program identifier and binary version before replacing the executable. CI signs
and notarizes it. Installation runs as the logged-in user without sudo, preserves
existing configuration and data, and refuses to replace a running daemon.
See [releases](releases.md) for the workflow and remaining manual checks.

## Client trust

Only connect trusted clients to the office daemon. The desktop client's Control
mode can send keyboard/mouse input, while agent work uses local MCP. The current
queue serializes agent tasks by default; it does not pause an agent automatically
when a human enables Control mode.
