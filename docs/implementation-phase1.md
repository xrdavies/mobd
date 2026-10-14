# Phase 1 implementation plan

> Archived Phase 1 plan. As of 2026-10-15, mobd is a standalone repository with
> a signed macOS installer, API v3, built-in MCP, Codex/Claude runners and a VNC relay.
> The monorepo layout, Python installation steps and remaining-work lists below
> describe the original plan, not current setup. Use [installation](releases.md#4-安装和升级),
> the [office checklist](office-smoke-checklist.md) and [current roadmap](roadmap.md).

> **Note (2026-10-13):** actuation has since moved into the `mobd` binary as `mobd mcp` ([ADR 0008](adr/0008-actuation-in-mobd.md)); `services/mob-mcp` (Python) references below are historical. Current API: [mobd-v3.md](mobd-v3.md).

Prove the dispatch → office Codex + **mob-mcp** → view-only VNC loop. Language and tooling choices below match ADRs 0001–0005 and **0007** (0006 superseded). See [research-computer-use.md](research-computer-use.md).

## Goals and exit criteria

From [roadmap Phase 1](roadmap.md):

- Office Mac runs a **Codex** runner with in-repo **`mob-mcp`** (local computer-use MCP).
- Office Mac exposes the desktop over **VNC**.
- Humans watch with **[svnc](https://github.com/xrdavies/svnc)** in **viewOnly** (separate browser/window is fine).
- A thin MOB **Go** control plane starts work on the office machine.

**Exit criteria:** From home, dispatch a Codex task that drives the office Mac UI, and watch the desktop live over VNC via svnc **without** sending human input.

## Monorepo layout (Go)

Monorepo tree (Go `mobd` scaffold lives under `cmd/` and `internal/`):

```text
mob/
  cmd/
    mobd/          # main: HTTP task API + runner orchestration
  internal/
    api/                  # REST handlers + SSE event stream
    runner/
      codex/              # spawn/attach Codex; inject mob-mcp MCP config
    task/                 # task store, state machine, IDs
    config/               # listen addr, MCP command/args, VNC view URL
  web/                    # optional: minimal static dispatch page (Phase 1 optional)
  # or apps/web-console/  # same optional role; pick one path when coding starts
  services/
    mob-mcp/             # Python stdio MCP: screenshot/click/type/key/windows
  docs/                   # architecture, ADRs, this plan
```

| Package / binary | Responsibility |
| --- | --- |
| `cmd/mobd` | Single office process: serve API, own runner lifecycle, health |
| `internal/api` | `POST/GET` tasks, cancel, SSE progress |
| `internal/runner/codex` | Build Codex + `mob-mcp` MCP config; start/cancel process; parse progress |
| `services/mob-mcp` | In-repo actuation MCP (MIT); Codex stdio target |
| `internal/task` | Persistent conversations, transcripts, thread IDs and turn states; memory-only debug replay |
| `web/` (optional) | Minimal HTML/JS to create a task and show status + link to svnc |

Phase 1 may co-locate the control plane on the office Mac (`mobd`). Splitting API to another host is a later ops choice, not required for the exit criteria.

## Task API sketch

Base URL example: `http://mobd:8080` (config-driven).

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/sessions` | List conversations newest-updated first |
| `GET` | `/sessions/:id` | Full transcript and ordered task/turn snapshots |
| `POST` | `/tasks` | Create a turn; `prompt` and optional `sessionId` (omitted = new conversation); 409 when busy |
| `GET` | `/tasks` | List recent tasks |
| `GET` | `/tasks/:id` | Task snapshot (status, timestamps, runner, error if any) |
| `GET` | `/tasks/:id/events` | **SSE** stream of progress / log / status changes |
| `POST` | `/tasks/:id/cancel` | Request cancellation of the runner process |

Suggested task shape (illustrative):

```json
{
  "id": "…",
  "sessionId": "…",
  "threadId": "…",
  "eventId": 12,
  "prompt": "…",
  "status": "queued|running|succeeded|failed|cancelled",
  "runner": "codex",
  "createdAt": "…",
  "updatedAt": "…",
  "error": null
}
```

Auth for Phase 1: **deferred**. Leave `MOBD_SHARED_SECRET` unset; bind to intranet/VPN only. Intranet auth and audit land with the home App in Phase 2 — see [client-phase2-lite.md](client-phase2-lite.md) and [roadmap.md](roadmap.md).

## Office Mac install checklist

For the full manual smoke procedure (mob-mcp → Codex → VNC/svnc viewOnly, **before** `mobd`), follow **[office-smoke-checklist.md](office-smoke-checklist.md)**. Codex on the office Mac can execute parts of that checklist.

Order below is practical, not a product installer yet.

1. **mob-mcp** — build/run in-repo `services/mob-mcp` ([ADR 0007](adr/0007-actuation-mob-mcp.md)). Grant **Accessibility** and **Screen Recording** in System Settings.
2. **Codex CLI** — install and authenticate so `mobd` can spawn Codex with an MCP config that points at `mob-mcp`.
3. **VNC / Screen Sharing** — enable a VNC-capable desktop server on the office Mac (macOS Screen Sharing or another VNC server). Prefer intranet / VPN; do not expose raw VNC to the public internet ([security](security.md)).
4. **Websockify (or equivalent) + WSS** — bridge VNC to a browser-reachable WebSocket, preferably behind TLS (WSS), matching [svnc](https://github.com/xrdavies/svnc) deployment expectations.
5. **svnc (home side)** — open the viewer with `viewOnly: true` against the office WSS/VNC endpoint.
6. **MOB `mobd`** — when implemented: listen on the configured task API address; point config at mob-mcp command/args and at the human VNC view URL for clients.

Smoke check before coding clients: complete [office-smoke-checklist.md](office-smoke-checklist.md) (Codex + mob-mcp actuation, then svnc viewOnly).

## Configuration (mobd)

Minimum knobs (names illustrative):

| Setting | Role |
| --- | --- |
| Listen address | HTTP bind for the task API (e.g. intranet interface + port) |
| mob-mcp command / args | How `runner/codex` injects MCP into Codex (path to binary, flags) |
| Codex executable / working dir | Spawn parameters for the Codex CLI |
| `MOBD_STATE_PATH` | Durable conversation snapshot (defaults to user config directory/mob/state.json) |
| VNC view URL | URL (or host/port/path) clients use with svnc viewOnly — returned or documented for the dispatch UI |
| Optional shared secret | **Deferred** until Phase 2 App; leave unset in Phase 1 |

Other MCP servers remain **config-swappable later** after license review; Phase 1 defaults to in-repo `mob-mcp` only ([ADR 0007](adr/0007-actuation-mob-mcp.md)).

## Actuation vs view (reminder)

- **Human:** svnc **viewOnly** only. No Phase 1 human mouse/keyboard path.
- **Agent:** mob-mcp on the office Mac. Do not reuse the human viewOnly session for clicks/typing.
- svnc Automation remains a documented fallback ([ADR 0003](adr/0003-actuation-local-mcp.md)), **out of Phase 1** unless mob-mcp cannot cover a required smoke test.

## Out of scope for Phase 1

- Native home Mac / iPhone apps; embedded svnc inside a MOB shell (Phase 2)
- Claude / Grok runners (Codex only)
- Third-party desktop MCP as default (optional later via config + license review only)
- WebRTC / ScreenCaptureKit view backend
- Human remote input channel
- Approval gates for payments/credentials (design later; not required for exit criteria)
- Intranet/production auth (deferred to Phase 2 App), multi-tenant task DB, HA control plane
- Forking or embedding Paseo / Orca

## Remaining Phase 1 work (auth deferred)

Scaffold exists under `cmd/mobd` and `internal/`. Finish these **without** auth:

1. **Codex path hardening** — office `POST /tasks` → real Codex + mob-mcp UI drive; clearer errors; optional single-flight concurrency.
2. **Health** — readiness hints on `/health` (Codex / mob-mcp present) without refusing to listen.
3. **Config / CORS** — exact-origin CORS is implemented for home browser / `apps/mob` HTTP and SSE calls; configure `MOBD_CORS_ORIGINS` and `MOBD_VNC_VIEW_URL` ([env reference](../README.md)).
4. **Optional** `web/` static dispatch page — skip if [`apps/mob`](client-phase2-lite.md) starts immediately.

Do **not** treat `MOBD_SHARED_SECRET` as a Phase 1 exit gate.

## Suggested implementation order

0. **`services/mob-mcp`** Phase 1 MVP (this package) — office smoke with Codex ([office-smoke-checklist.md](office-smoke-checklist.md))
1. `internal/task` + in-memory store and status transitions
2. `internal/api` REST + SSE (can stub runner with a no-op process first)
3. `internal/runner/codex` + mob-mcp config injection; real spawn/cancel
4. Wire `cmd/mobd`
5. Manual verification against the install checklist + svnc viewOnly
6. Optional `web/` static dispatch page

## References

- [mob-mcp README](adr/0008-actuation-in-mobd.md)
- [Office Mac smoke checklist](office-smoke-checklist.md)
- [Computer-use research](research-computer-use.md)
- [Architecture](architecture.md)
- [Roadmap](roadmap.md)
- [ADR 0005 — Backend: Go](adr/0005-backend-go.md)
- [ADR 0006 — Actuation: ControlMCP first](adr/0006-actuation-controlmcp-first.md) (**superseded**)
- [ADR 0007 — Actuation: in-repo mob-mcp](adr/0007-actuation-mob-mcp.md)
- [ADR 0004 — Runner: Codex first](adr/0004-runner-codex-first.md)
- [ADR 0001 — Screen view: VNC + svnc](adr/0001-screen-view-vnc-svnc.md)
