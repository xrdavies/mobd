# Security notes

Access controls and deployment requirements for mobd. See [VNC setup](vnc-svnc-setup.md)
for the live-view connection.

## Deployment posture

- Prefer **office intranet** or **VPN**. Do not expose raw VNC to the public internet.
- HTTP pairing links are allowed with a CLI warning, including LAN and Tailscale
  addresses. Without an encrypted tunnel, observers can capture invitations,
  long-lived credentials and business data. Authentication does not encrypt HTTP;
  the CLI does not infer transport security from an IP range or hostname.
- For home-to-office access, follow the [remote-access guide](remote-access.md):
  prefer **Tailscale Serve** with restricted tailnet access and a loopback mobd
  listener, or use a trusted **SSH** tunnel. A public HTTPS/WSS proxy also needs
  authentication on every route; TLS alone is not access control.
- Phone access without a VPN requires a public gateway or Tailscale Funnel plus
  paired credentials on every route; see the [architecture](remote-access.md).
  Desktop/mobile client integration is pending. Funnel provides transport, not application login.
- The planned managed relay must authorize each user/device mapping and isolate
  customers' office machines. A gateway that terminates TLS can read proxied
  data; this topology does not provide end-to-end encryption from client to mobd.
- `mobd init` writes `0.0.0.0:8080` (all IPv4 interfaces). Without a config override, the runtime default is `:8080`; use `127.0.0.1:8080` when access should only go through a local proxy or tunnel.

## View vs actuation

- **Humans:** the mob desktop client disables input by default and offers an explicit Control mode. A standalone svnc monitor can use **`viewOnly: true`**. This is client behavior; mobd relays RFB bytes and does not enforce view-only access.
- **Agent:** desktop tools run through **local MCP** (`mobd mcp`) on the office Mac, independently of the human VNC connection.

Keeping these channels apart limits accidental remote control and makes auditing clearer.

## Secrets and approvals

- Do not ship credentials in the client. Office-side runner and MCP hold tool credentials as needed.
- `ask_user` and `request_approval` are available through `mobd mcp`. The question/answer API uses a per-task token for internal calls; pending approvals are denied when unanswered, expired or cancelled.
- Codex and Claude permissions default to `bypass`. Set `codexPermissions` / `claudePermissions` to `approve` for the provider gates described in [runners](runners.md). Codex exec has only the instruction-based MCP gate; app-server supports provider approval requests. These controls do not make arbitrary desktop actions a sandbox.

## Mandatory device authentication

Every business route requires `Authorization: Bearer <device credential>`, including
health, reads, writes, SSE, images and VNC. Credentials are independent per client
and remain valid until revoked, deleted or authentication is reset. The
[pairing protocol](pairing.md) uses ten-minute, single-use invitations generated
through a user-private Unix socket. Loopback and proxy headers do not bypass auth.
Because browser WebSockets cannot send headers, `GET /vnc` alone also accepts a
`?ticket=` from `POST /vnc/ticket`: 30 seconds, single use, bound to the issuing
client and invalidated with it. The long-lived credential never appears in a URL.
Revocation disconnects the client's active SSE/VNC streams. Screen Sharing still
performs its own upstream VNC authentication; browser origins are checked separately.

Authentication state is private, atomically persisted and locked to one daemon.
Only credential hashes are stored. Corrupt state fails startup; missing state
creates an empty identity, never anonymous access. Local management stays on the
Unix socket; agent/MCP calls retain per-task tokens. All paired clients have full
access to the same office data; there is no multi-user isolation or security audit log.

Remove nonempty `sharedSecret` / `MOBD_SHARED_SECRET` before upgrading and pair each
client. The old global secret and `X-Mob-Token` are no longer accepted. Business data
is preserved. The mob desktop/mobile implementations must be updated separately,
including secure storage and credentials on streaming/image/WebSocket requests.

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
