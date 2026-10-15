# Office VNC + home svnc (viewOnly)

mobd requires a paired `Authorization: Bearer <token>` on the `/vnc` handshake,
in addition to Screen Sharing credentials. The client must support authenticated
WebSockets (for example through a native transport); raw browser WebSocket/svnc
examples cannot attach this header automatically. Client integration is follow-up
work. Never put the long-lived token in the WebSocket URL. See [pairing](pairing.md).


Practical setup for the live-view checks in the [office smoke checklist](office-smoke-checklist.md).
The mob client disables human input by default and offers explicit Control mode;
a standalone [svnc](https://github.com/xrdavies/svnc) monitor can use `viewOnly: true`.
The agent actuates through `mobd mcp` on the office machine.

Related: [architecture](architecture.md), [security](security.md) and
svnc [peripheral security](https://github.com/xrdavies/svnc/blob/main/docs/peripheral-security.md).

> **mob setup:** install [mobd](releases.md#4-安装和升级), enable Screen Sharing,
> and enter the office Mac's mobd URL in the App. mobd relays the live view on
> `GET /vnc`. websockify is only needed
> for standalone svnc setups that do not use this relay.

**Ports below are examples only** — pick free ports and firewall rules that fit your LAN. Nothing here mandates a specific port number.

## Topology (mob)

```text
Home mob client (svnc, human input disabled by default)
        │  WebSocket (prefer WSS off-LAN)
        ▼
[optional] reverse proxy / VPN / SSH tunnel
        │
        ▼
Office: mobd /vnc relay       ──TCP──►  macOS Screen Sharing / VNC
                                              (office Mac desktop)
```

- **Human path:** WebSocket → mobd relay → VNC framebuffer. Input is disabled by default in the client.
- **Agent path:** Codex or Claude + `mobd mcp` on the office Mac (separate from this VNC session).

For separate home/office networks, follow the [remote-access guide](remote-access.md)
to carry the API and VNC relay through one private endpoint.
Prefer **office intranet or VPN**. Do not expose raw VNC (`5900`-style TCP) to the public internet.

## 1. Office Mac — enable Screen Sharing (VNC)

1. **System Settings → General → Sharing → Screen Sharing** — turn on.
2. Choose who can access (specific users or all users with a VNC password, depending on your macOS version / policy).
3. Note how you will reach the machine from the bridge host:
   - Same Mac as Screen Sharing: often `127.0.0.1` and the local VNC port Screen Sharing uses (commonly illustrated as `5900` — **verify on your Mac**; it can differ with multi-display / custom setups).
   - Another host on the LAN: use the office Mac’s LAN IP or hostname instead of inventing a fixed port in docs or scripts.
4. Confirm from another Mac on the same trusted network with the built-in Screen Sharing client (or any VNC client) that the desktop appears before connecting through the WebSocket relay.

Optional: another VNC server (e.g. TigerVNC) is fine if you already run one; MOB only needs a classic RFB TCP endpoint the bridge can target.

## 2. Office — bridge TCP VNC → WebSocket

Browsers cannot open raw TCP VNC. [svnc](https://github.com/xrdavies/svnc) expects a
**WebSocket** that carries RFB bytes. mobd provides this relay at `/vnc`;
standalone svnc can use **[websockify](https://github.com/novnc/websockify)**.

### With mob: built-in relay

`mobd` relays VNC itself on `GET /vnc`. Start it with the [mobd CLI](../README.md) and point the App at the mobd URL only:

```bash
mobd init   # first install; edit ~/.mobd/config.json (listen, vncAddr)
mobd start
mobd status
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

Leave mobd running while you test (the CLI manages a user LaunchAgent). Restart with `mobd restart`. For plain svnc without mob, run websockify yourself (e.g. `websockify 6080 127.0.0.1:5900`).

## 3. TLS / WSS (when the path leaves a trusted LAN)

Carry the connection through a VPN/SSH tunnel, or terminate TLS at a reverse
proxy that authenticates every route. TLS alone does not restrict access.

| Situation | Suggestion |
| --- | --- |
| Viewer and office on same trusted LAN / VPN | `ws://` to the bridge may be enough |
| Over the public internet or untrusted segments | Use **SSH** / WireGuard / Tailscale, or an authenticated **WSS** reverse proxy; protect the whole mobd API as well as VNC |

Example mental model (ports/hostnames are yours to choose):

```text
Browser ──WSS──► reverse proxy ──WS──► mobd /vnc ──TCP──► VNC on office Mac
```

Details: [svnc peripheral-security.md](https://github.com/xrdavies/svnc/blob/main/docs/peripheral-security.md).

## 4. Home — svnc with `viewOnly: true`

On the **home** Mac (or any machine that can reach the WebSocket URL over intranet/VPN/WSS):

```bash
npm install @xrdavies/svnc
```

Minimal connect (replace URL, password, and mount element; **keep `viewOnly: true` for MOB humans**):

```ts
import { SVNCClient } from '@xrdavies/svnc';

const mount = document.getElementById('screen');

const client = await SVNCClient.connect(mount, {
  // Example URL — use your bridge host/port or WSS path
  url: 'ws://office-mac.local:6080/',
  credentials: { password: 'your-vnc-password' }, // if VNC Auth is enabled
  viewOnly: true, // standalone monitor: no human input path
});

// Optional: observe frames; do not call click / typeText on this session
client.on('frame', () => {});
```

You can also clone [svnc](https://github.com/xrdavies/svnc), `npm install && npm run build`, and adapt [`examples/lite/`](https://github.com/xrdavies/svnc/tree/main/examples/lite) so connect options use `viewOnly: true`.

Quick start reference: [svnc quick-start](https://github.com/xrdavies/svnc/blob/main/docs/quick-start.md) (that doc’s Agent examples use `viewOnly: false` — **do not** copy that for MOB’s human monitor).

## 5. Verify the view-only monitor

| Check | How |
| --- | --- |
| Picture | Office desktop updates in the home svnc canvas (open an app on the office Mac or let Codex + `mob-mcp` change the UI). |
| No human input | With `viewOnly: true`, clicks and keypresses in the viewer must **not** move the office cursor or type into office apps. |
| Separation | Agent work still goes through `mob-mcp` / Codex on the office Mac, not through this viewOnly session. |

Use the [office smoke checklist](office-smoke-checklist.md#4-verify-the-home-view)
to verify the full task and live-view flow.

## Troubleshooting

- **Connect fails:** Confirm Screen Sharing works with a native VNC client first, then check mobd's `vncAddr` and `/health`, or the target of your standalone websockify bridge. Verify the WS URL from home with VPN connected when required.
- **Black / stuck frame:** Check VNC credentials and the client's supported authentication modes. mobd passes RFB through unchanged, including Apple Remote Desktop sign-in; capture permission for `/screen` and MCP tools is a separate macOS grant.
- **Viewer still controls the Mac:** Session is not viewOnly — reconnect with `viewOnly: true` and avoid Automation `click` / `typeText` on the human monitor path.
- **Internet exposure:** Shut public VNC ports; put VPN or WSS+auth in front ([security.md](security.md)).

## References

- [svnc](https://github.com/xrdavies/svnc) — MIT npm package `@xrdavies/svnc`
- [websockify](https://github.com/novnc/websockify)
- [mobd CLI](../README.md) — office service and configuration
- [Office smoke checklist](office-smoke-checklist.md)
- [Architecture](architecture.md)
