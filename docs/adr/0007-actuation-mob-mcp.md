# ADR 0007: Actuation — in-repo `mob-mcp` (supersedes 0006)

## Status

Accepted; **implementation language superseded** by [ADR 0008](0008-actuation-in-mobd.md) (Go, inside the `mobd` binary as `mobd mcp`). The in-repo, MIT, local-MCP-by-default decision stands.

As of 2026-10-15, `services/mob-mcp` no longer exists in this repository.
The Python decisions below are historical; the [published installer](../releases.md#4-安装和升级)
provides the daemon and MCP as one binary.

## Context

[ADR 0003](0003-actuation-local-mcp.md) requires local computer-use MCP on the office Mac. [ADR 0006](0006-actuation-controlmcp-first.md) chose [nix18/ControlMCP](https://github.com/nix18/ControlMCP) as the Phase 1 default.

Follow-up research ([research-computer-use.md](../research-computer-use.md), 2026-10-12) found ControlMCP unsuitable:

- **0** stars / **0** forks; no GitHub Releases; last push **2026-03-27** (stale relative to research date).
- License **GPL-3.0**, which conflicts with bundling into MIT MOB.
- GhostDesk is actively maintained but uses **FSL-1.1-ALv2** with a Competing Use clause that is a poor fit for shipping MOB as a productized computer-use stack.
- Stronger MIT options exist (e.g. CursorTouch/MacOS-MCP) but remain external single-project risks; MOB needs ownership for office private deployment, audit hooks, and Codex integration.

Anthropic computer-use APIs / Claude Code built-in computer-use are valuable **patterns**, not a Codex-first runtime.

## Decision

- Build and maintain an **in-repo** stdio MCP server: **`services/mob-mcp/`**, MIT, as the **default** office actuation path for Codex.
- Phase 1 MVP tools: `screenshot`, `click`, `type_text`, `key`, `list_windows`, `open_app` (see research doc).
- Implementation language for MVP: **Python**; optional later Swift helper for ScreenCaptureKit / Accessibility quality — keep the MCP schema stable.
- Third-party MCP servers (MacOS-MCP, computer-use-mcp, GhostDesk, etc.) may be used only as **temporary smoke aids** or **optional config alternatives** after license review — **not** as the documented product default.
- **Supersede ADR 0006.** Do not install ControlMCP as part of MOB docs or `mobd` defaults.

## Consequences

- Office smoke checklist and Phase 1 plan target `mob-mcp`.
- `mobd` injects MCP config pointing at the in-repo server (path/command configurable).
- Team owns macOS TCC (Accessibility, Screen Recording) behavior and coordinate scaling.
- Slightly higher Phase 1 eng cost than adopting an external MCP as-is; lower long-term license and abandonment risk.
