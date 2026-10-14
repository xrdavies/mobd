# Office Mac smoke test report

Date: 2026-10-12
Repository: `mob`
Code baseline: `d048da1` (`docs: VNC and svnc viewOnly setup`)

## Scope

This report covers the in-repo `services/mob-mcp` setup and its local desktop smoke checks. It does not claim completion of the VNC/view-only monitor check; that requires a separately reachable home viewer and an office VNC/WebSocket endpoint. For configuring acceptance **B**, follow **[vnc-svnc-setup.md](vnc-svnc-setup.md)** and re-run checklist §3.

## Environment

- macOS 26.5.2, Apple Silicon (`darwin/arm64`)
- Python 3.12.13 in a uv-managed virtual environment
- `mob-mcp` 0.1.0
- MCP Python SDK 1.30.0
- Codex CLI 0.160.1
- Codex MCP entry: `/Users/m4002/Projects/moe/mob/services/mob-mcp/.venv/bin/mob-mcp`

Installation used:

```bash
cd services/mob-mcp
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -e '.[dev]'
```

The MCP server was registered with Codex as the global stdio server `mob-mcp`. No third-party ControlMCP server was added.

## Results

### Automated tests

```text
22 collected
16 passed
6 skipped
```

The six skipped cases are the parameterized tests in `tests/test_tools_off_macos.py`. They verify that each actuation tool rejects non-macOS hosts, so pytest skips them on this macOS machine with the reason `non-macOS only`.

### MCP protocol smoke

Passed:

1. Started `mob-mcp` over stdio without writing non-protocol data to stdout.
2. Completed MCP `initialize` successfully.
3. Listed the six expected tools: `click`, `key`, `list_windows`, `open_app`, `screenshot`, and `type_text`.
4. Called `list_windows` and received the real desktop window list.
5. Called `screenshot` and received a real PNG image plus metadata.
6. Called `open_app("Safari")`; the command reported Safari opened and the subsequent window list contained Safari.

### VNC connectivity

Passed: a second machine on the office LAN connected to the office Mac's native Screen Sharing VNC endpoint at `192.168.50.86:5900`. The office Mac's Screen Sharing service was running and the endpoint returned an RFB greeting.

This verifies native LAN VNC reachability only. It does not by itself verify the browser WebSocket bridge or svnc's `viewOnly` input blocking.

### Fixes made during verification

The initial implementation had two runtime issues that the unit tests did not cover:

- The server imported the screenshot function under a module alias that shadowed the registered `screenshot` tool. The import now uses `capture_screenshot`.
- FastMCP 1.30 attempted structured-output validation on the image result. The screenshot tool now explicitly uses `structured_output=False`, allowing the `Image` and metadata text content to serialize correctly.

A regression test now checks that the screenshot tool returns MCP `image` and `text` content.

## Acceptance status

| Check | Status | Evidence |
| --- | --- | --- |
| A — local actuation path | **Passed for direct MCP smoke** | `list_windows`, `screenshot`, and `open_app("Safari")` succeeded through `mob-mcp`; Safari appeared in the real window list. |
| A — Codex prompt acceptance | **Pending** | Codex is configured, but a separate interactive Codex prompt driving a harmless UI change was not run as part of this report. |
| B1 — native LAN VNC reachability | **Passed** | A second LAN machine connected to `192.168.50.86:5900` through native Screen Sharing. |
| B2 — svnc view-only monitor | **Pending** | websockify/WSS, svnc rendering, and viewer input-blocking verification remain. Procedure: [vnc-svnc-setup.md](vnc-svnc-setup.md). |

Before declaring the full checklist complete, run a Codex prompt that opens a harmless app or types a unique marker, then validate the office desktop from a home/VPN svnc session with `viewOnly: true` and confirm viewer input cannot control the office Mac. Use [vnc-svnc-setup.md](vnc-svnc-setup.md) for Screen Sharing, websockify/WSS, and the home `viewOnly` connect example; update this report when B passes.
