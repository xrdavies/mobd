# ADR 0008: Actuation — merge mob-mcp into the `mobd` binary (Go)

## Status

Accepted and implemented (2026-10-13): `internal/actuate`, `internal/mcpserver`, `mobd mcp`; `services/mob-mcp` removed. Supersedes the **Python implementation** choice in [ADR 0007](0007-actuation-mob-mcp.md). The rest of 0007 still applies: actuation stays in-repo, MIT, a local MCP, and the documented default.

## Context

ADR 0007 chose an in-repo stdio MCP server, `services/mob-mcp`, written in Python for MVP speed. After Phase 1 office smoke testing:

- **macOS permissions (TCC) are fragile.** Accessibility and Screen Recording are granted to a concrete executable or its responsible host. With Python, that is a pyenv/venv interpreter path or the terminal app, which changes when Python, the venv, or the launcher changes. Commit `72a53d7` had to add a guard because Quartz input silently failed without Accessibility.
- **Two runtimes to deploy.** An office Mac needs Go (`mobd`) plus Python 3.10+, a venv, pyobjc and Pillow. Scripts and the smoke checklist carry the venv discovery logic.
- **mobd cannot see actuation.** Codex starts mob-mcp itself, so mobd only learns about tool calls through Codex's JSONL. Product features need actuation and the control plane in one place: a single shared desktop lock, `/screen` snapshots, readiness checks for permissions, click markers in the UI, and later pause or takeover.
- **The surface is small.** About 700 lines and 6 tools (`screenshot`, `click`, `type_text`, `key`, `list_windows`, `open_app`). The native APIs used (CGEvent, CGWindowList, NSPasteboard, NSScreen) can be reached from Go through cgo just as Python reaches them through pyobjc.

## Decision

- Port actuation to Go as `internal/actuate`, built with cgo on darwin and stubbed (returning a clear "macOS only" error) elsewhere so `go test ./...` passes on any OS.
- Expose it as a **stdio MCP server subcommand of the same binary**: `mobd mcp`, using the official Go SDK `github.com/modelcontextprotocol/go-sdk`.
- Keep the **tool names, arguments and result shapes identical** to the Python server (screenshot returns a PNG image block plus the JSON metadata text block; coordinates are in points). Prompts and Codex behaviour must not change.
- By default the Codex runner injects `mcp_servers.mob-mcp.command = <path of the running mobd>` and `args = ["mcp"]`. `MOBD_MOB_MCP_COMMAND` / `MOBD_MOB_MCP_ARGS` still override this to point at an external MCP.
- mobd reuses `internal/actuate` directly for control-plane features (`GET /screen`, permission checks in `/health`).
- Remove `services/mob-mcp` after the Go server matches it. Port its unit tests (key parsing, permission guard, tool registration).
- **Merge the binary, not the process.** Codex keeps starting the MCP server as a child process over stdio. Serving MCP in-process over streamable HTTP, so Codex connects to the running mobd by URL, is a later option (Phase B) that would give mobd the authoritative view of every action.

## Consequences

- One binary to build, sign and grant permissions to. Setup becomes: build `mobd`, grant Accessibility and Screen Recording to the launching host, run it.
- Building requires macOS with Xcode command line tools (cgo). Non-darwin builds still compile with stub actuation for tests and CI.
- The macOS permission identity is the *responsible process* that launched the chain (Terminal/iTerm when started from a shell, or `mobd` itself under launchd). `/health` reports `AXIsProcessTrusted` and `CGPreflightScreenCaptureAccess` as seen by mobd, which the `mobd mcp` children inherit.
- Python-only extras (OCR, image diffing) are no longer free. If needed later, add them in Go or as an optional, separate MCP. An Objective-C/Swift shim via cgo stays possible for the Accessibility element tree and ScreenCaptureKit.
- `scripts/` no longer looks for `services/mob-mcp/.venv`.
