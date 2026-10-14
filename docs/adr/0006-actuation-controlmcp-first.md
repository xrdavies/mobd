# ADR 0006: Actuation — ControlMCP first

## Status

**Superseded** by [ADR 0007](0007-actuation-mob-mcp.md) (2026-10-12).

Do not use [nix18/ControlMCP](https://github.com/nix18/ControlMCP) as MOB’s default or bundled actuation. See [research-computer-use.md](../research-computer-use.md) for the evaluation (maintenance signals, **GPL-3.0** vs MOB MIT, alternatives).

## Context (historical)

[ADR 0003](0003-actuation-local-mcp.md) prefers local computer-use MCP on the office Mac over driving the UI only through the human VNC session. Concrete MCP options under discussion included ControlMCP and GhostDesk.

MOB needs the agent to operate **real applications on the office Mac desktop** (not a cloud VM). Phase 1 originally picked one default to reduce install and runner-config surface area.

- **ControlMCP** focused on controlling the current machine without requiring a Docker virtual-desktop path.
- **GhostDesk** offered local binary control and also a Docker/virtual-desktop style path.

## Decision (historical — no longer in force)

- Use **ControlMCP** as the **Phase 1 default** local actuation MCP on the office Mac.
- Keep GhostDesk (and other MCP servers) as **optional, config-selected** alternatives later — runner config should not hard-code forever to a single binary name.
- Do not change ADR 0003: human viewOnly VNC stays separate; svnc Automation remains fallback, not the Phase 1 default.

## Consequences (historical)

Replaced by ADR 0007 consequences: in-repo **`mob-mcp`**, MIT, Codex stdio; ControlMCP removed from install defaults.
