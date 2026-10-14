# ADR 0005: Control plane and office runner in Go

## Status

Accepted

Implementation update (2026-10-15): the standalone daemon includes the service
CLI, native actuation, MCP and VNC relay. Go/Xcode are needed to build from
source; users can install a signed macOS Apple Silicon binary without those
toolchains ([installation](../releases.md#4-安装和升级)).

## Context

Phase 1 needs a long-running office process that exposes a thin task HTTP API (including SSE) and spawns/cancels a Codex CLI session with local MCP. Candidate stacks discussed: Go, Rust, Node, and Bun.

Requirements that matter for Phase 1:

- Stable resident process on an office Mac
- Straightforward subprocess management for Codex
- Simple HTTP + SSE without a heavy framework
- Single deployable binary preferred for intranet ops
- TypeScript remains appropriate for the svnc viewer and any later web shell; that does not force the control plane language

Rust can integrate with Codex/MCP via subprocess or an MCP client, but costs more iteration time for Phase 1. Node/Bun align with svnc’s stack but are a weaker fit for a small always-on host binary compared to Go.

## Decision

- Implement the MOB **control plane and office runner host in Go** (`cmd/mobd` and `internal/...`).
- Keep **svnc** (TypeScript) as the view client; do not rewrite svnc in Go.
- Defer any native/home client UI stack choice to Phase 2; an optional static dispatch page may still be served or opened separately in Phase 1.

## Consequences

- Phase 1 implementation docs and code layout assume Go modules and a `cmd/mobd` entrypoint.
- MCP and Codex integration go through process spawn and config files/CLI flags inside the Go runner adapter, not through embedding those runtimes in-process.
- If extreme memory-safety constraints appear later, hot paths could be reconsidered; Go is the Phase 1 and default choice for the host.
