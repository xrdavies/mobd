# Roadmap

> Historical product plan from before the repository split. Current server setup is in
> [README](../README.md); desktop code and build commands are in
> [xrdavies/mob](https://github.com/xrdavies/mob).

Phased delivery. Do not skip the separation of **view** (human) and **actuation** (agent).

## Phase 1 — Prove the loop

- Office Mac: **Go** `mobd` control plane + **Codex** runner with **mob-mcp** (local computer-use MCP); see [implementation-phase1.md](implementation-phase1.md)
- Office Mac: VNC server for the desktop
- Human monitor: **svnc** in view-only mode (embedded in the App)
- Minimal task dispatch via the thin MOB Go API (`POST /tasks`, status, SSE) to start work on the office machine

**Auth:** Phase 1 optional shared secret (`MOBD_SHARED_SECRET`) stays **deferred / unset**. Do not block the loop on intranet auth; rely on intranet/VPN bind. Auth moves to Phase 2 with the App.

**Remaining (non-auth):** Codex path hardening on office Mac via `mobd`, richer `/health` readiness hints, optional static `web/` dispatch page. Home-client CORS is implemented via `MOBD_CORS_ORIGINS`. Detail: [client-phase2-lite.md](client-phase2-lite.md) (Phase 1 leftover table) and [implementation-phase1.md](implementation-phase1.md).

**Exit criteria:** From home, dispatch a Codex task that drives the office Mac UI, and watch the desktop live over VNC via svnc without sending human input. ✓ Delivered.

## Phase 2 — Productize the client

Order of work (2026-10-13): design docs ([mobd-v3](mobd-v3.md), [apps-mob-ui v2](https://github.com/xrdavies/mob/blob/main/docs/ui.md), ADR [0008](adr/0008-actuation-in-mobd.md) / [0009](https://github.com/xrdavies/mob/blob/main/docs/adr/0009-app-frontend-react.md)) → **mobd v3** APIs and features → **merge mob-mcp into mobd** → **App v2** (React port, then redesign). Security hardening is deferred while in development. Status: all four delivered on 2026-10-13 (mobd API v3, `mobd mcp`, `apps/mob` 0.2.0).

Agent runners (2026-10-13, [runners.md](runners.md)): pluggable runner contract and shared tests; `ask_user` / `request_approval` with question cards; Claude Code runner (with an optional hard approval gate); Codex app-server mode with hard approvals, interrupt, steering and streaming. Next candidates: pause the agent while the user takes control, more runners (Gemini, opencode, pi; possibly via ACP).


- Integrate the svnc viewer into MOB clients (home Mac first via **`apps/mob`**, then iPhone) — see **[client-phase2-lite.md](client-phase2-lite.md)** (recommended stack: **Tauri 2 + Vite/TS**)
- Unified UX: chat / task list + embedded live desktop (view-only by default, Control mode opt-in)
- **Intranet auth and basic audit log land with the App** (not as a Phase 1 gate). Wire tokens/session to `mobd` when the home client exists.

**Exit criteria:** One MOB client surface for dispatch + monitor on home Mac; iPhone usable for the same core flow.

## Phase 3 — View protocol flexibility (delivered: Control mode)

- Optional `DesktopView` implementation: WebRTC / ScreenCaptureKit (or similar) for lower-latency self-hosted streaming
- Human input channel for remote mouse/keyboard — **default off** (view-only); **Control** mode is an explicit opt-in toggle in the App
- Human approval gates for sensitive actions

**Exit criteria:** Clients can switch view backends without rewriting task/actuation paths; human control remains an explicit, rare escape hatch.

## Later — Per-agent desktops (planning)

Not an MVP gate. Target shape: shared filesystem, independent GUI desktop per agent (Grok Bot–like). Options and Tart/UTM orchestration notes: **[research-per-agent-desktop-vm.md](research-per-agent-desktop-vm.md)**. Until then, keep the **single desktop + global queue**.
