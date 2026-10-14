# ADR 0003: Desktop actuation via local MCP

## Status

Accepted

## Context

The office Mac must open applications and operate GUI elements under agent control. Options include:

- Local computer-use MCP servers on the office Mac (MOB default: in-repo `mob-mcp`; see ADR 0007)
- Driving the UI only through VNC input (svnc Automation)
- Cloud-hosted desktop computer-use (rejected: not intranet-deployable as desired)

Humans watching over VNC must remain view-only in the common case; actuation should not imply that the monitor connection can type and click.

## Decision

- **Prefer local computer-use MCP** on the office Mac for actuation (Accessibility permissions as required by the OS).
- Treat **svnc Automation** (click/type over VNC) as a **fallback**, on a path **separate** from the human `viewOnly` session.
- Keep actuation APIs out of the `DesktopView` monitor interface.

## Consequences

- Agent effectiveness depends on office-side MCP install and macOS permissions.
- Security story is clearer: human view channel ≠ agent input channel.
- Fallback via VNC remains available if MCP tooling is incomplete for a workflow, without changing the default human monitor mode.
