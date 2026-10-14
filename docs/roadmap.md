# Roadmap and delivery status

Status checked against the mobd repository on 2026-10-15 (Asia/Tokyo).
The daemon is maintained in [xrdavies/mobd](https://github.com/xrdavies/mobd);
the desktop client is in [xrdavies/mob](https://github.com/xrdavies/mob).
Product versions are independent of the API version advertised by `/health`.

## Implemented

- Go task API v3: persistent conversations, runner resume IDs, messages and
  activities; session management; per-task and global SSE; screenshots and uploads.
- FIFO desktop queue, with one running agent task by default and cancellation.
- Codex app-server (default) and exec fallback, plus Claude Code runner. App-server
  supports streamed replies, steering and provider approval requests.
- `mobd mcp`: local desktop tools, `ask_user`, `request_approval` and Claude's
  permission prompt bridge. No separate Python MCP package is needed.
- Built-in `/vnc` WebSocket relay to macOS Screen Sharing. The desktop client
  embeds svnc with input disabled by default and an explicit Control mode.
- JSON configuration, environment/CLI overrides, and per-user LaunchAgent commands:
  `init`, `start`, `stop`, `restart`, `status`, `logs` and foreground `serve`.
- Independent release tagging, Developer ID signing and Apple notarization;
  GitHub Releases, R2 distribution, and the public install/upgrade script at
  `https://mob-releases.0xd.app/mobd/install.sh`.

[v0.0.2](https://github.com/xrdavies/mobd/releases/tag/v0.0.2) is published to GitHub
and R2 as of the date above. See [releases](releases.md) for release steps and the
[office smoke checklist](office-smoke-checklist.md) for manual acceptance.
CI success does not replace a real office/home desktop test.

## Current limits

- One shared desktop: the queue coordinates agents, but human Control mode does
  not automatically pause the active agent.
- Optional shared-secret checks cover mutating API routes, not read routes or
  `/vnc`. There is no multi-user login system or dedicated security audit log.
  Deployment remains LAN/VPN oriented ([security](security.md)).
- Runner permissions default to `bypass`; approval enforcement depends on the
  runner mode and configured policy ([runners](runners.md)).
- Official binary distribution targets macOS 13+ / Apple Silicon. State is an
  atomic JSON snapshot owned by a single daemon, with separate image artifacts.

## Planned, not implemented

- Pause/resume coordination for human takeover.
- Additional runner adapters (for example Gemini, opencode or pi).
- Multi-user authentication and broader security/audit controls.
- Separate desktops per agent through user sessions or external VM runtimes
  ([research](research-per-agent-desktop-vm.md)); no VM orchestration ships today.
- Alternative view transports such as WebRTC/ScreenCaptureKit when needed.

## Historical plans

[Phase 1](implementation-phase1.md), [Phase 2 lite](client-phase2-lite.md) and the
dated research/ADRs retain the original design decisions. Their old directory
layouts, pending feature lists and prototype installation commands are historical;
current installation starts in the [README](../README.md).
