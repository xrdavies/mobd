# mobd API v3 — features and reference

**Status:** implemented, including step 7 ([ADR 0008](adr/0008-actuation-in-mobd.md)). `GET /health` reports `apiVersion: 3`.
**Scope:** the office control plane (`cmd/mobd`, `internal/...`). API version `3` is independent of binary release versions such as `0.0.2`. See [releases](releases.md) for installation and [security](security.md) for the implemented authentication limits.
**Related:** [ADR 0008](adr/0008-actuation-in-mobd.md) (actuation merged into mobd), [ADR 0009](https://github.com/xrdavies/mob/blob/main/docs/adr/0009-app-frontend-react.md) and [apps-mob-ui.md](https://github.com/xrdavies/mob/blob/main/docs/ui.md) (the client that uses these APIs).

## Goals

1. **Show what the agent does**, not just its final reply. Tool calls, shell commands, reasoning summaries and plans become typed, persisted *activities* the App renders as timeline cards.
2. **One desktop, one driver.** A global FIFO queue makes sure only one task actuates the office Mac at a time.
3. **Manage conversations**: rename, pin, archive, delete.
4. **Live multi-client state**: a global event stream keeps the sidebar, queue and status chips current without polling.
5. **Richer turns**: per-task model and reasoning effort, plus image attachments.
6. **Desktop snapshots without VNC**: `GET /screen` for thumbnails (iPhone, session previews) and readiness checks.
7. **Readiness**: `/health` explains what is missing on the office Mac (Codex, actuation, macOS permissions, VNC).

## Current scope

Codex and Claude Code runners, questions/approvals and Codex app-server steering
are implemented. Shared-secret authentication is optional on mutating routes;
there is no multi-user login or dedicated security audit log. Storage remains an
atomic JSON snapshot with separate image files, not a database. Per-agent desktops
and automatic pause for human takeover remain planned work.

## Codex exec events → MOB model

The following is the exec fallback mapping, verified with codex-cli 0.162.1.
The default is now app-server; [runners](runners.md) covers both Codex modes and Claude.
`codex exec --json` emits:

```jsonl
{"type":"thread.started","thread_id":"…"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"…"}}
{"type":"item.started","item":{"id":"item_3","type":"command_execution","command":"/bin/zsh -lc 'echo hi'","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"…","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}
{"type":"item.started","item":{"id":"item_4","type":"mcp_tool_call","server":"mob-mcp","tool":"list_windows","arguments":{},"result":null,"error":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_4","type":"mcp_tool_call","server":"mob-mcp","tool":"list_windows","arguments":{…},"result":{"content":[{"type":"text","text":"…"}]},"error":null,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"…"}}
{"type":"turn.completed","usage":{"input_tokens":30877,"cached_input_tokens":0,"output_tokens":108,"reasoning_output_tokens":51}}
```

| Codex item / event | MOB result |
| --- | --- |
| `thread.started` | `resume` event → session `resumeId` (was `threadId`; see [runners.md](runners.md)) |
| `agent_message` | assistant **message** (unchanged) |
| `mcp_tool_call` | activity `kind: "tool"` with `server`, `tool`, `arguments`, text `output`, `error`; image content blocks are stored as **artifacts** and referenced by ID |
| `command_execution` | activity `kind: "command"` with `command`, `output` (truncated), `exitCode` |
| `reasoning` | activity `kind: "reasoning"` with summary `text` |
| `todo_list` | activity `kind: "plan"` with `items: [{text, completed}]` |
| `file_change` | activity `kind: "file_change"` with `changes: [{path, kind}]` |
| `web_search` | activity `kind: "web_search"` with `query` |
| item `error` | activity `kind: "notice"`, status `failed` (Codex warnings, e.g. config notices) |
| `turn.completed.usage` | task `usage` |
| anything else | debug `log` event (memory-only, unchanged) |

Activities are **upserted by ID** (`<taskId>:<itemId>`): `item.started` creates a `running` activity, and `item.updated` / `item.completed` replace it. Long text (`output`, reasoning) is capped at 16 KiB per activity, with `truncated: true`.

## Data model changes

```ts
type Task = {
  // existing: id, sessionId, resumeId (was threadId), eventId, prompt, status, runner, createdAt, updatedAt, error, vncViewUrl
  model?: string;
  effort?: string;                // allowed values come from the selected runner in /health
  attachments?: string[];          // upload artifact IDs
  usage?: { inputTokens: number; cachedInputTokens: number; outputTokens: number; reasoningOutputTokens: number; costUsd?: number };
  waiting?: boolean;              // awaiting a user answer or approval
  queuePosition?: number;          // 1-based while queued, computed on read
  startedAt?: string;
  finishedAt?: string;
};

type Session = {
  // existing: id, title, runner, resumeId (was threadId), activeTaskId, status, createdAt, updatedAt, messages
  pinned: boolean;
  archived: boolean;
  waiting?: boolean;
};

type Activity = {
  id: string;                      // "<taskId>:<codexItemId>"
  taskId: string;
  kind: "tool" | "command" | "reasoning" | "plan" | "file_change" | "web_search" | "notice";
  status: "running" | "completed" | "failed";
  title: string;                   // short human summary, e.g. "click (412, 230)"
  server?: string; tool?: string; arguments?: unknown;
  command?: string; exitCode?: number;
  text?: string;                   // tool text output, command output, reasoning summary
  truncated?: boolean;
  error?: string;
  images?: string[];               // artifact IDs (screenshots)
  items?: { text: string; completed: boolean }[];   // plan
  changes?: { path: string; kind: string }[];       // file_change
  startedAt: string; updatedAt: string;
};
```

The App builds the conversation timeline by merging `messages` and `activities` by time (`createdAt` / `startedAt`). Both come from one snapshot in `GET /sessions/{id}`.

## Desktop queue

- `MOBD_MAX_RUNNING` (default **1**) limits how many tasks run at once, across all sessions.
- `POST /tasks` always returns `queued`. A dispatcher starts queued tasks in FIFO order as slots free up.
- A session still allows only one non-terminal task. A follow-up while queued or running returns **409** as before.
- Cancelling an undispatched queued task makes it `cancelled` straight away without starting a runner. A dispatched task can briefly still report `queued`; cancellation then waits for its worker to stop.
- `queuePosition` is set on queued tasks in every response and in `task` global events.
- Dispatch is asynchronous. Two immediate submissions can initially have positions 1 and 2; once the first becomes `running`, the second becomes queued position 1.

## Endpoints

Existing v2 endpoints keep their shape. New fields are additive.

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/health` | Adds `apiVersion: 3`, `features`, `checks`, `runners`, default runner's `models`/`efforts`, `maxRunning` |
| GET | `/sessions` | `?archived=false` (default) \| `true` \| `all`. Sorted pinned first, then `updatedAt` descending |
| GET | `/sessions/{id}` | `{session, tasks, activities, questions}` |
| PATCH | `/sessions/{id}` | Body `{title?, pinned?, archived?}` → updated session |
| DELETE | `/sessions/{id}` | **409** while a turn is active; removes its tasks, activities and artifacts → **204** |
| POST | `/tasks` | `{prompt, sessionId?, runner?, model?, effort?, attachments?}`; attachments are upload IDs; runner is pinned per session |
| GET | `/tasks` | `?sessionId=`, `?status=`, `?limit=` (default 100, max 500) |
| GET | `/tasks/{id}/events` | Adds `activity` and `usage` events |
| POST | `/tasks/{id}/cancel` | Also cancels queued tasks |
| GET | `/queue` | `{running: Task[], queued: Task[]}` |
| GET | `/events` | Global SSE (see below) |
| GET | `/screen` | Current desktop snapshot. `?format=png\|jpeg` (default jpeg), `?maxWidth=` (default full size), `?quality=1..100` (default 80) |
| POST | `/uploads` | `multipart/form-data` field `file`; PNG/JPEG/GIF/WebP up to 20 MiB → `{id, name, mime, size, url}` |
| GET | `/artifacts/{id}` | Stored image (screenshots from tool results, uploads). Immutable, `Cache-Control: max-age=31536000` |
| POST | `/tasks/{id}/questions/{qid}/answer` | Answer a pending question: `{answer}` (questions) or `{approved, answer?}` (approvals) → question; 409 if already settled ([runners.md](runners.md#asking-the-user-ask_user-request_approval)) |
| POST | `/tasks/{id}/steer` | `{prompt}`: add a message to the running turn (runners with `steer`; Codex app-server). 409 when the turn cannot take input |
| POST | `/internal/tasks/{id}/ask` | Used by `mobd mcp` only (per-turn token); blocks until the question is settled |
| GET | `/vnc` | WebSocket ↔ TCP relay to `MOBD_VNC_ADDR` (Screen Sharing); replaces websockify ([ADR 0010](adr/0010-vnc-relay-in-mobd.md)). 403 for unknown origins, 502 if the VNC server is down |

### Health (v3)

```json
{
  "ok": true, "service": "mobd", "apiVersion": 3,
  "runner": "codex", "maxRunning": 1,
  "vncViewUrl": "…", "mobMcp": "/path/to/mobd",
  "features": ["queue", "activities", "sessionAdmin", "events", "screen", "uploads", "taskOptions", "vncProxy", "runners", "questions", "steer"],
  "models": ["gpt-5.5-codex", "…"],
  "efforts": ["minimal", "low", "medium", "high", "xhigh"],
  "checks": {
    "codex":     { "ok": true,  "detail": "/Users/me/.local/bin/codex (codex-cli 0.162.1)" },
    "actuation": { "ok": true,  "detail": "mobd mcp" },
    "accessibility":   { "ok": false, "detail": "grant Accessibility to the app that launches mobd" },
    "screenRecording": { "ok": true },
    "vnc":       { "ok": true,  "detail": "127.0.0.1:5900 reachable" }
  }
}
```

`ok` stays true when mobd is serving. `checks` explain readiness without blocking anything and are cached for 10 s. Each check is `{ok, unknown?, detail?}`; permission checks come from `CGPreflightPostEventAccess` / `CGPreflightScreenCaptureAccess` in mobd ([ADR 0008](adr/0008-actuation-in-mobd.md)) and are `unknown` off macOS. `runners` lists each enabled runner's capabilities and readiness ([runners.md](runners.md)); top-level `models`/`efforts` describe the default runner. Codex models come from `MOBD_CODEX_MODELS`; an empty list means "the agent's default only". The `vnc` check dials `MOBD_VNC_ADDR` (default `127.0.0.1:5900`); `vncSessions` counts open `/vnc` relays.

### Global events (`GET /events`)

SSE with monotonic `id`s from a 1024-entry in-memory ring. `Last-Event-ID` / `?after=` resume. If the cursor is older than the ring, the server first sends `event: reset`, and the client should refetch snapshots.

| `event` | `data` |
| --- | --- |
| `session` | session summary (no messages) after create/update/status change |
| `session.deleted` | `{id}` |
| `task` | task snapshot (with `queuePosition`) on every status change |
| `queue` | same shape as `GET /queue` whenever it changes |
| `reset` | `{}` |

Per-task timelines stay on `/tasks/{id}/events`, including `question` events for
question/approval changes. The global stream carries session/task summaries with
their `waiting` flags; question snapshots are also available in `GET /sessions/{id}`.

## Configuration additions

| Variable | Default | Meaning |
| --- | --- | --- |
| `MOBD_MAX_RUNNING` | `1` | Tasks allowed to run at once (one desktop → 1) |
| `MOBD_DATA_DIR` | directory of `MOBD_STATE_PATH` | Root for `artifacts/` (screenshots, uploads) |
| `MOBD_CODEX_MODELS` | empty | Codex model IDs offered to clients (`MOBD_MODELS` is still read for backward compatibility) |
| `MOBD_VNC_ADDR` | `127.0.0.1:5900` | VNC server relayed by `GET /vnc` and checked by `/health` |
| `MOBD_QUESTION_TIMEOUT` | `30m` | How long a turn waits for an answer to `ask_user` / `request_approval` |

## Internal structure

```text
cmd/mobd            CLI: init, serve, start, stop, restart, status, logs, mcp, --version
internal/api        HTTP handlers, SSE, global event hub
internal/task       Store (snapshot persistence), queue ordering, activities
internal/runner     Registry, Codex app-server/exec and Claude adapters, shared CLI lifecycle
internal/actuate    Desktop capture and cgo input on macOS; stubs elsewhere
internal/mcpserver  mobd mcp: desktop tools, questions and approvals over stdio
internal/artifact   Content-addressed file store for screenshots and uploads
internal/config     JSON config, environment overrides and CLI flags
internal/service    Per-user macOS LaunchAgent and saved service configuration
```

- `runner.Runner` interface: `Start(ctx, *task.Task, Options, EmitFunc) error`. Codex and Claude implement it; optional `runner.Steerer` enables mid-turn input.
- In-memory debug logs are capped per task (oldest log lines dropped first). Persisted messages and activities are never dropped from replay.
- Every state change still goes through the atomic snapshot commit. Image bytes never enter the snapshot.

## Original delivery order (completed)

1. Runner interface + activity mapping + usage + log cap
2. Desktop queue + `/queue` + queued cancel
3. Session admin (`PATCH`/`DELETE`, pinned/archived, filters) + `/tasks` filters
4. Global `/events` hub
5. Artifacts + `/screen` + `/uploads` + task options (model/effort/attachments)
6. Health v3 checks
7. ADR 0008: `internal/actuate` cgo input + `mobd mcp`, remove `services/mob-mcp`
