# Phase 2 lite — home Mac App `apps/mob` (embed svnc)

> Archived client plan. As of 2026-10-15, the desktop app lives at the root of
> [xrdavies/mob](https://github.com/xrdavies/mob), uses React/Tauri and connects to
> mobd's API and `/vnc` relay. The old `apps/mob` layout and MVP exclusions below
> are historical. Current daemon installation is in the [README](../README.md);
> implemented server capabilities are listed in the [roadmap](roadmap.md).

**Status:** scaffold landed in [`apps/mob`](https://github.com/xrdavies/mob) (Tauri 2 + Vite/TS MVP). Auth still deferred.
**Decision (user):** intranet auth is **deferred** until this App work; Phase 1 `MOBD_SHARED_SECRET` stays optional and unset for now.

## Goals

Home Mac first: one small surface that is **remote control + monitor**.

- **Dispatch:** call office `mobd` task API (`POST /tasks`, list/get, SSE, cancel).
- **Monitor:** embed **[svnc](https://github.com/xrdavies/svnc)** with **`viewOnly: true`** (no human mouse/keyboard).
- **Not in MVP:** iPhone, auth/login, human remote input, WebRTC, multi-runner.

## Phase 1 remaining on `mobd` (besides auth)

Skim of current `cmd/mobd` + `internal/` vs [implementation-phase1.md](implementation-phase1.md): scaffold already has task CRUD, cancel, SSE, `/health`, env config, Codex spawn + mob-mcp `-c` injection, and safe fail if Codex is missing.

**Still to finish before calling Phase 1 “done” (auth explicitly out):**

| Area | What’s left |
| --- | --- |
| **Codex path hardening** | Office end-to-end: `POST /tasks` → real `codex exec` + `mob-mcp` drives UI; verify MCP `-c` / args on real Codex CLI; clearer failure messages; optional concurrency limit (one running task); tune sandbox / workdir docs from office findings |
| **Health** | Keep `/health`; extend readiness hints (Codex on `PATH`, `mob-mcp` binary exists) without blocking listen |
| **Config** | CORS is implemented via `MOBD_CORS_ORIGINS` (Tauri defaults); mobd reads `~/.mobd/config.json` and exported environment variables. Ensure `MOBD_VNC_VIEW_URL` is set for clients |
| **Optional static dispatch page** | `web/` or served by `mobd`: prompt + status + link/embed placeholder — useful before the native shell; skip if `apps/mob` lands first |
| **Auth** | **Deferred** — do not block on `MOBD_SHARED_SECRET`; add with App (Phase 2) |

Exit criteria for Phase 1 remain: from home, dispatch a Codex UI task and watch viewOnly VNC (svnc or Screen Sharing for smoke).

## Stack options (home Mac)

| Option | Pros | Cons |
| --- | --- | --- |
| **Tauri 2 + Vite/TypeScript** | Real Mac app; small footprint; webview runs `@xrdavies/svnc` npm package as-is; same UI can later wrap iOS or stay web | Rust toolchain on build machine |
| Electron + Vite/TS | Familiar Node packaging | Heavy binary; more update/security surface |
| Swift + WKWebView | Native feel | Awkward npm/`@xrdavies/svnc` bundling; two stacks for web+API glue |
| Browser-only Vite shell | Fastest prototype | Not an “App”; windowing/deeplinks weaker |

### Recommendation: **Tauri 2 + Vite / TypeScript**

**One stack for the home App (`apps/mob`).** Rationale:

1. **svnc is already a browser library** — the MVP screen is HTML/Canvas + `SVNCClient.connect(..., { viewOnly: true })`; Tauri’s webview is the natural host.
2. **Task API is plain HTTP/SSE** — fetch from the frontend to `MOBD_URL`; no need for Electron’s Node in the renderer.
3. **Mac-first App** without Electron weight; iPhone can reuse the web UI later (Tauri mobile or a thin WKWebView shell) without rewriting monitor logic.
4. Electron only if we hit a hard Tauri/webview gap; Swift-only wastes the svnc investment.

Prototype shortcut (optional, same frontend): run the Vite app in Chrome against LAN `mobd` before wrapping Tauri.

## MVP screens

Grok-like three-column shell (see [apps-mob-ui.md](https://github.com/xrdavies/mob/blob/main/docs/ui.md)):

1. **Left — Sessions** — remote machines (localStorage) with real conversations from `GET /sessions`; restore the last selection per machine.
2. **Center — Chat** — full transcript from `GET /sessions/:id`; prompt composer → `POST ${MOBD_URL}/tasks` with `sessionId`; structured message/status SSE (`GET .../tasks/:id/events`); Cancel → `POST .../cancel`.
3. **Right — Preview** — svnc thumbnail with **`viewOnly: true`** on `VNC_WS_URL`; double-click expands to App-fullscreen; Esc returns. Never enable human input.
4. **Settings** — add/edit machines (`MOBD_URL`, `VNC_WS_URL`, optional password, display name). Connection fields are not a permanent main-chrome strip.

Conversation history is now persisted by the office daemon in `MOBD_STATE_PATH`; the client stores connection settings and selected IDs locally. No database dependency is required. Update the daemon together with the client.

## Environment variables (client)

| Variable | Meaning | Example |
| --- | --- | --- |
| `MOBD_URL` | Base URL of office `mobd` task API | `http://192.168.50.86:8080` |
| `VNC_WS_URL` | WebSocket URL for svnc (after websockify / WSS proxy). Since [ADR 0010](adr/0010-vnc-relay-in-mobd.md) optional: default is mobd's `/vnc` relay | `ws://192.168.50.86:6080/` or `wss://…` |

Notes:

- Office side may also set `MOBD_VNC_VIEW_URL` (human-facing page URL). The mob App should prefer explicit `VNC_WS_URL` for the embedded RFB connection.
- Client stores machines (name + URLs + optional password) in **localStorage** via Settings; env vars only prefill a default machine. VNC password never in the repo; OS keychain later.
- Until App auth lands, bind `mobd` to intranet/VPN only ([security.md](security.md)).

## Proposed repo layout (next coding turn)

```text
apps/
  mob/                 # Tauri 2 + Vite/TS MVP (home Mac; product name: mob)
    src/               # UI: sessions | chat | thumbnail preview + Settings
    src-tauri/         # Tauri shell
    README.md
```

Scaffold is under `apps/mob` — see that README for Mac run steps.

## Out of scope for mob App MVP

- Intranet auth / tokens (follow-on in Phase 2 with App)
- iPhone client
- Human remote keyboard/mouse
- Shipping websockify inside the App (office still runs VNC + bridge)
- Replacing VNC with WebRTC (`DesktopView` later)

## UI plan

Living wireframes / MVP checklist: [apps-mob-ui.md](https://github.com/xrdavies/mob/blob/main/docs/ui.md).

## References

- [svnc](https://github.com/xrdavies/svnc) — `SVNCClient`, `viewOnly`, examples/lite
- [vnc-svnc-setup.md](vnc-svnc-setup.md)
- [mobd README](../README.md)
- [roadmap.md](roadmap.md)
- [implementation-phase1.md](implementation-phase1.md)
