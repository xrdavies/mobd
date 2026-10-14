# Security notes

Short guidance aligned with the product design. Details of VNC transport should follow [svnc](https://github.com/xrdavies/svnc) peripheral / deployment docs.

## Deployment posture

- Prefer **office intranet** or **VPN**. Do not expose raw VNC to the public internet.
- For remote access, terminate TLS at a **WSS reverse proxy** or use **SSH** tunneling for VNC. Avoid relying on VeNCrypt as the primary story (same stance as svnc guidance).
- `mobd init` writes a loopback listen address. Without a config override, the runtime default is `:8080`; choose the bind address explicitly for deployment.

## View vs actuation

- **Humans:** the mob desktop client disables input by default and offers an explicit Control mode. A standalone svnc monitor can use **`viewOnly: true`**. This is client behavior; mobd relays RFB bytes and does not enforce view-only access.
- **Agent:** Prefer **local MCP** on the office Mac. If svnc Automation is used as fallback, it must be a **separate** connection/path from the human viewOnly session.

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
