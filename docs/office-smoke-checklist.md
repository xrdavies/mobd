# Office Mac manual smoke checklist

Do this **on the office Mac** (and home viewer for step 3) **before** `mobd` exists. Goal: prove Codex + in-repo **`mob-mcp`** can actuate the real desktop, and that humans can watch over VNC via [svnc](https://github.com/xrdavies/svnc) in **viewOnly**.

See [research-computer-use.md](research-computer-use.md) and [ADR 0007](adr/0007-actuation-mob-mcp.md). Codex on the office machine can perform parts of this checklist (install commands, config edits, self-checks). Prefer intranet or VPN; do not expose raw VNC to the public internet ([security](security.md)).

**Out of scope here:** shipping third-party desktop MCPs as product defaults, human remote keyboard/mouse, and `mobd` / the MOB task API.

**Do not** install [nix18/ControlMCP](https://github.com/nix18/ControlMCP) (GPL-3.0; abandoned for MOB’s purposes — [ADR 0006](adr/0006-actuation-controlmcp-first.md) superseded). Default actuation is the built-in **`mobd mcp`** server ([ADR 0008](adr/0008-actuation-in-mobd.md)).

## 1. mob-mcp — install, permissions, verify actuation

1. On the office Mac, check out this repo and build `mobd` (Go + Xcode command line tools; see [mobd README](../README.md)):

   ```bash
   go build -o bin/mobd ./cmd/mobd
   ./bin/mobd mcp   # stdio MCP server; Ctrl+C after a quick sanity start, or leave for Codex
   ```

2. System Settings → Privacy & Security:
   - Grant **Accessibility** to the host that launches `mobd` (Terminal / iTerm, or `mobd` itself under launchd).
   - Grant **Screen Recording** (needed for `screenshot`).
3. Run a smoke check via Codex (next section) or by calling tools: `screenshot`, then a harmless `open_app` / click. Confirm the **real** desktop UI changes—not only a text reply.

If Accessibility or Screen Recording is missing, stop and fix permissions before continuing.

Temporary MIT third-party spikes (e.g. [CursorTouch/MacOS-MCP](https://github.com/CursorTouch/MacOS-MCP)) are **optional** only if you need to validate TCC prompts while debugging — **not** a product dependency and **not** the documented default.

## 2. Codex CLI — auth and wire mob-mcp

1. Install the Codex CLI on the office Mac and authenticate (login / API credentials as required by current Codex docs).
2. Add `mob-mcp` to Codex’s MCP configuration. Example (absolute paths):

   ```json
   {
     "mcpServers": {
       "mob-mcp": {
         "command": "/absolute/path/to/mobd/bin/mobd",
         "args": ["mcp"]
       }
     }
   }
   ```

   Restart Codex or reload MCP so the server is listed as available.
3. **Smoke prompt (must drive real UI):** ask Codex to do something observable on the desktop via tools, for example open Safari (or another installed app) and navigate to a known URL, or open TextEdit and type a short unique string.

**Pass:** you see the office UI change accordingly (app opens, URL loads, text appears).
**Fail:** Codex only describes steps in chat without MCP tool use, or tools error on permissions—fix MCP config and macOS privacy grants, then retry.

`mobd` is **not** required for this step; spawn Codex manually.

## 3. VNC + websockify/WSS + home svnc (viewOnly)

With mobd running, no bridge is needed: turn on Screen Sharing and the App connects through `ws://<office>:<mobd-port>/vnc` ([ADR 0010](adr/0010-vnc-relay-in-mobd.md)). The websockify steps below apply to plain svnc. Step-by-step (Screen Sharing, websockify examples, WSS, home `viewOnly` snippet): **[vnc-svnc-setup.md](vnc-svnc-setup.md)**.

1. On the office Mac, enable **System Settings → General → Sharing → Screen Sharing** (or another VNC server). Note host, port, and credentials — ports are site-specific; see setup doc for examples only.
2. Bridge TCP VNC → WebSocket with **websockify** (or equivalent). Prefer **intranet/VPN**; use **WSS** (or SSH/VPN) when the path leaves a trusted LAN ([svnc](https://github.com/xrdavies/svnc) peripheral security).
3. From the **home** Mac (or VPN), connect with [svnc](https://github.com/xrdavies/svnc) using **`viewOnly: true`**.

**Pass:** the office desktop is visible in near real time.
**Pass (input):** mouse and keyboard from the viewer do **not** control the office Mac.

## 4. Acceptance

| Check | Criterion |
| --- | --- |
| **A — Actuation** | Codex + **mob-mcp** changes the office Mac UI (step 2). |
| **B — View-only monitor** | Home svnc shows that desktop; human input is disabled (step 3). |

Both A and B must pass. After that, Phase 1 coding can attach `mobd` to the same `mob-mcp` + VNC path ([implementation-phase1.md](implementation-phase1.md)).

## Notes

- Run install and actuation steps on the **office Mac**; use home only for the svnc viewer (and later task dispatch).
- Prefer **intranet / VPN**; see [security.md](security.md) for exposure rules.
- Third-party MCPs (including ControlMCP / GhostDesk) are **not** required; see research + ADR 0007.
- **`mobd` is not required yet**—this checklist is the manual vertical slice before the Go control plane.

## References

- [VNC + svnc viewOnly setup](vnc-svnc-setup.md)
- [mobd README (actuation)](../README.md#desktop-actuation-mobd-mcp)
- [Computer-use research](research-computer-use.md)
- [Phase 1 implementation plan](implementation-phase1.md)
- [Architecture](architecture.md)
- [ADR 0001 — Screen view: VNC + svnc](adr/0001-screen-view-vnc-svnc.md)
- [ADR 0004 — Runner: Codex first](adr/0004-runner-codex-first.md)
- [ADR 0007 — Actuation: in-repo mob-mcp](adr/0007-actuation-mob-mcp.md)
- [svnc](https://github.com/xrdavies/svnc)
