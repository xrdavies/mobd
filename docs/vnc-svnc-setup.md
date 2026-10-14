# Office VNC + home svnc (viewOnly)

Practical setup for **acceptance B** of the [office smoke checklist](office-smoke-checklist.md): humans at home watch the office Mac desktop over VNC via [svnc](https://github.com/xrdavies/svnc) with **`viewOnly: true`**, while the agent actuates through **`mob-mcp`** on the office machine (not through this viewer path).

Related: [ADR 0001](adr/0001-screen-view-vnc-svnc.md), [security.md](security.md), svnc [peripheral security](https://github.com/xrdavies/svnc/blob/main/docs/peripheral-security.md) and [websockify decision](https://github.com/xrdavies/svnc/blob/main/docs/websockify-decision.md).

> **mob users (2026-10-13):** mobd now relays the live view itself on `GET /vnc` ([ADR 0010](adr/0010-vnc-relay-in-mobd.md)). Turn on Screen Sharing (section 1) and paste only the mobd URL into the App; the websockify steps below are for plain svnc or custom bridges.

**Ports below are examples only** — pick free ports and firewall rules that fit your LAN. Nothing here mandates a specific port number.

## Topology (Phase 1)

```text
Home browser (svnc, viewOnly: true)
        │  WebSocket (prefer WSS off-LAN)
        ▼
[optional] reverse proxy / VPN / SSH tunnel
        │
        ▼
Office: websockify (or equiv)  ──TCP──►  macOS Screen Sharing / VNC
                                              (office Mac desktop)
```

- **Human path:** viewOnly WebSocket → bridge → VNC framebuffer. No intentional key/mouse from the monitor.
- **Agent path:** Codex + `mob-mcp` on the office Mac (separate from this VNC session).

Prefer **office intranet or VPN**. Do not expose raw VNC (`5900`-style TCP) to the public internet.

## 1. Office Mac — enable Screen Sharing (VNC)

1. **System Settings → General → Sharing → Screen Sharing** — turn on.
2. Choose who can access (specific users or all users with a VNC password, depending on your macOS version / policy).
3. Note how you will reach the machine from the bridge host:
   - Same Mac as Screen Sharing: often `127.0.0.1` and the local VNC port Screen Sharing uses (commonly illustrated as `5900` — **verify on your Mac**; it can differ with multi-display / custom setups).
   - Another host on the LAN: use the office Mac’s LAN IP or hostname instead of inventing a fixed port in docs or scripts.
4. Confirm from another Mac on the same trusted network with the built-in Screen Sharing client (or any VNC client) that the desktop appears **before** adding websockify.

Optional: another VNC server (e.g. TigerVNC) is fine if you already run one; MOB only needs a classic RFB TCP endpoint the bridge can target.

## 2. Office — bridge TCP VNC → WebSocket

Browsers cannot open raw TCP VNC. [svnc](https://github.com/xrdavies/svnc) expects a **WebSocket** that carries RFB bytes. Use **[websockify](https://github.com/novnc/websockify)** (or an equivalent WS↔TCP bridge). svnc itself does not ship a production bridge.

### With mob: no bridge needed

`mobd` relays VNC itself on `GET /vnc`. Start it with the [mobd CLI](../README.md) and point the App at the mobd URL only:

```bash
make init   # edit ~/.mobd/config.json (listen, vncAddr)
make start
make status
```

The rest of this section is for plain svnc without mob.

### Plain svnc: websockify one-liner

```bash
# Example only: listen on WS port 6080, forward to local Screen Sharing on 5900
websockify 6080 127.0.0.1:5900
```

Notes:

- Bind the bridge to **loopback** or a private interface when possible; put VPN or a reverse proxy in front for remote viewers.
- Pin the TCP target (do not run an open relay that can connect anywhere).
- Alternatives: a native WebSocket VNC server (no bridge), or a thin lab bridge such as svnc’s non-production [`examples/ws-tcp-bridge/`](https://github.com/xrdavies/svnc/tree/main/examples/ws-tcp-bridge) for local experiments only.
- A future **`mobvnc`** package may replace these scripts; the viewer contract remains a WebSocket RFB URL.

Leave mobd running while you test (the CLI manages a user LaunchAgent). Restart with `make restart`. For plain svnc without mob, run websockify yourself (e.g. `websockify 6080 127.0.0.1:5900`).

## 3. TLS / WSS (when the path leaves a trusted LAN)

Align with svnc **D7**: encrypt at the **edge**, not via RFB VeNCrypt-first.

| Situation | Suggestion |
| --- | --- |
| Viewer and office on same trusted LAN / VPN | `ws://` to the bridge may be enough |
| Over the public internet or untrusted segments | Terminate **WSS** at nginx / Caddy / Traefik (or websockify `--cert`), or use **SSH** / WireGuard / Tailscale and keep WS inside the tunnel |

Example mental model (ports/hostnames are yours to choose):

```text
Browser ──WSS──► reverse proxy ──WS──► websockify ──TCP──► VNC on office Mac
```

Details: [svnc peripheral-security.md](https://github.com/xrdavies/svnc/blob/main/docs/peripheral-security.md).

## 4. Home — svnc with `viewOnly: true`

On the **home** Mac (or any machine that can reach the WebSocket URL over intranet/VPN/WSS):

```bash
npm install svnc
```

Minimal connect (replace URL, password, and mount element; **keep `viewOnly: true` for MOB humans**):

```ts
import { SVNCClient } from '@xrdavies/svnc';

const mount = document.getElementById('screen');

const client = await SVNCClient.connect(mount, {
  // Example URL — use your bridge host/port or WSS path
  url: 'ws://office-mac.local:6080/',
  credentials: { password: 'your-vnc-password' }, // if VNC Auth is enabled
  viewOnly: true, // required for acceptance B — no human input path
});

// Optional: observe frames; do not call click / typeText on this session
client.on('frame', () => {});
```

You can also clone [svnc](https://github.com/xrdavies/svnc), `npm install && npm run build`, and adapt [`examples/lite/`](https://github.com/xrdavies/svnc/tree/main/examples/lite) so connect options use `viewOnly: true`.

Quick start reference: [svnc quick-start](https://github.com/xrdavies/svnc/blob/main/docs/quick-start.md) (that doc’s Agent examples use `viewOnly: false` — **do not** copy that for MOB’s human monitor).

## 5. Verify acceptance B

| Check | How |
| --- | --- |
| Picture | Office desktop updates in the home svnc canvas (open an app on the office Mac or let Codex + `mob-mcp` change the UI). |
| No human input | With `viewOnly: true`, clicks and keypresses in the viewer must **not** move the office cursor or type into office apps. |
| Separation | Agent work still goes through `mob-mcp` / Codex on the office Mac, not through this viewOnly session. |

Checklist row: [office-smoke-checklist.md](office-smoke-checklist.md) §3 / acceptance **B**. Record outcome in [office-smoke-test-report.md](office-smoke-test-report.md) when you run it.

## Troubleshooting

- **Connect fails:** Confirm Screen Sharing works with a native VNC client first; then confirm websockify targets that same host:port; then open the WS URL from home (VPN up if required).
- **Black / stuck frame:** Check encodings/auth (None or VNC Auth — what svnc supports today); wrong password or blocked Screen Recording on the office Mac can also break capture for other tools.
- **Viewer still controls the Mac:** Session is not viewOnly — reconnect with `viewOnly: true` and avoid Automation `click` / `typeText` on the human monitor path.
- **Internet exposure:** Shut public VNC ports; put VPN or WSS+auth in front ([security.md](security.md)).

## References

- [svnc](https://github.com/xrdavies/svnc) — MIT npm package `@xrdavies/svnc`
- [websockify](https://github.com/novnc/websockify)
- [mobd CLI](../README.md) — office service and configuration
- [Office smoke checklist](office-smoke-checklist.md)
- [Office smoke test report](office-smoke-test-report.md)
- [Architecture](architecture.md)
