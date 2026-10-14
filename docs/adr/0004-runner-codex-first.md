# ADR 0004: Office runner — Codex first

## Status

Accepted

Implementation update (2026-10-15): Codex and Claude Code are both implemented.
Codex defaults to app-server mode with exec as a fallback. The original
Codex-first ordering below is historical; see [runners](../runners.md) for the
current registry, session pinning, approvals and steering behavior.

## Context

The office runner starts an AI coding/agent session with access to actuation tools. Multiple backends are desirable long-term (Codex, Claude, Grok, others). Shipping all at once delays proving the dispatch + view loop.

## Decision

- Implement the office runner for **Codex first**.
- Design the runner boundary so **Claude, Grok, and others** can be added as pluggable adapters later without changing the client’s task model.

## Consequences

- Phase 1 validation targets Codex + local MCP + VNC/svnc only.
- Control plane task records should store a runner type / config so additional runners can be selected when implemented.
- Provider-specific auth and CLI/SDK details stay inside each runner adapter.
