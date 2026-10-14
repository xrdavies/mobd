# ADR 0002: Thin custom orchestration in MOB

## Status

Accepted

## Context

Task dispatch and session orchestration could be:

1. Wired directly into Paseo / Orca as plugins
2. Forked from Paseo / Orca
3. Implemented as a thin stack owned by MOB, borrowing UX ideas only

MOB needs one product surface that combines chat/dispatch with an svnc-based monitor, deployable on an office intranet. Owning the task API and client shell matters more than reusing an external agent launcher’s full codebase.

## Decision

- MOB owns a **thin custom** control plane: task API, office runner wiring, and client shell that embeds svnc.
- **Learn** from Paseo / Orca (remote start of Claude/Codex-class tools, task/session status concepts).
- **Do not fork** Paseo or Orca.

## Consequences

- More upfront design for the task API, but clear product boundaries and private deployment.
- Integration with existing launchers remains possible at the edges later; it is not the core architecture.
- Client and monitor UX can evolve without waiting on upstream forks.
