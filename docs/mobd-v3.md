# mobd API reference

`GET /health` reports `apiVersion: 3`. The API version is independent of the
binary release version printed by `mobd --version`. Clients discover supported
features and runner capabilities through `/health`.

This reference covers the office control plane (`cmd/mobd`, `internal/...`).
See [architecture](architecture.md), [runners](runners.md),
[installation](releases.md) and [security](security.md).

[Agent concepts](agent-concepts.md) defines the vocabulary and compatibility names.
[Agent collaboration](agent-collaboration.md) is a separate draft; its proposed
routes and `agentCollaboration` feature are not implemented API capabilities.

## Capabilities

1. **Agent timeline:** tool calls, shell commands, reasoning summaries and plans become typed, persisted *activities* the App renders as timeline cards.
2. **Desktop queue:** a global FIFO queue runs one task at a time by default.
3. **Manage conversations**: rename, pin, archive, delete.
4. **Live multi-client state**: a global event stream keeps the sidebar, queue and status chips current without polling.
5. **Task options**: per-task model and reasoning effort, plus image attachments.
6. **Desktop snapshots**: `GET /screen` for thumbnails (iPhone, session previews) and readiness checks.
7. **Readiness**: `/health` explains what is missing on the office computer (Codex, actuation, macOS permissions, display state; on Linux the desktop programs, cgroup, encoder, memory and disk).
8. **Desktops per conversation** (Linux, feature `sessionDesktops`): each conversation has its own desktop, turns of different conversations run at once, idle desktops sleep; see [Conversation desktops](#conversation-desktops-linux).

## Current scope

Codex and Claude Code runners, questions/approvals and Codex app-server steering
are implemented. Every business route requires a paired device credential in
`Authorization: Bearer <token>`, including `/health` and the live desktop (`/stream`). See the
[pairing protocol](pairing.md) for invitations and CLI management.
The `pairing` feature is advertised after authentication. There is no multi-user
login or dedicated security audit log. Storage uses an
atomic JSON snapshot with separate image files. On macOS tasks share one desktop; on Linux each
conversation has its own. Human Control mode does not automatically pause an agent.

## Codex exec events → MOB model

The following is the exec mode mapping implemented by the Codex runner.
The default mode is app-server; [runners](runners.md) covers both Codex modes and Claude.
`codex exec --json` emits:

```jsonl
{"type":"thread.started","thread_id":"…"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"…"}}
{"type":"item.started","item":{"id":"item_3","type":"command_execution","command":"/bin/zsh -lc 'echo hi'","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"…","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}
{"type":"item.started","item":{"id":"item_4","type":"mcp_tool_call","server":"mob-mcp","tool":"list_windows","arguments":{},"result":null,"error":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_4","type":"mcp_tool_call","server":"mob-mcp","tool":"list_windows","arguments":{},"result":{"content":[{"type":"text","text":"…"}]},"error":null,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"…"}}
{"type":"turn.completed","usage":{"input_tokens":30877,"cached_input_tokens":0,"output_tokens":108,"reasoning_output_tokens":51}}
```

| Codex item / event | MOB result |
| --- | --- |
| `thread.started` | `resume` event → session `resumeId` |
| `agent_message` | assistant **message** |
| `mcp_tool_call` | activity `kind: "tool"` with `server`, `tool`, `arguments`, `text`, `error`; image content blocks are stored as **artifacts** and referenced by ID |
| `command_execution` | activity `kind: "command"` with `command`, `text` (truncated), `exitCode` |
| `reasoning` | activity `kind: "reasoning"` with summary `text` |
| `todo_list` | activity `kind: "plan"` with `items: [{text, completed}]` |
| `file_change` | activity `kind: "file_change"` with `changes: [{path, kind}]` |
| `web_search` | activity `kind: "web_search"` with the query in `title` |
| item `error` | activity `kind: "notice"`, status `failed` (Codex warnings, e.g. config notices) |
| `turn.completed.usage` | task `usage` |
| anything else | debug `log` event (memory-only) |

Activities are **upserted by ID** (`<taskId>:<itemId>`): `item.started` creates a `running` activity, and `item.updated` / `item.completed` replace it. Activity `text` (tool output, command output or reasoning) is capped at 16 KiB, with `truncated: true`.

## Data model

```ts
type Task = {
  id: string;
  sessionId: string;
  resumeId?: string;              // opaque runner conversation ID
  eventId: number;
  prompt: string;
  status: "queued" | "running" | "succeeded" | "failed" | "cancelled";
  runner: string;
  createdAt: string;
  updatedAt: string;
  error: string | null;
  model?: string;
  effort?: string;                // allowed values come from the selected runner in /health
  attachments?: string[];         // upload artifact IDs
  usage?: { inputTokens: number; cachedInputTokens: number; outputTokens: number; reasoningOutputTokens: number; costUsd?: number };
  waiting?: boolean;              // awaiting a user answer or approval
  queuePosition?: number;         // 1-based while queued, computed on read
  queueReason?: "slots" | "desktops" | "memory" | "disk" | "desktopStarting"; // why it still waits
  startedAt?: string;
  finishedAt?: string;
};

type Session = {
  id: string;
  title: string;
  runner?: string;
  resumeId?: string;
  activeTaskId?: string;
  status: Task["status"];
  createdAt: string;
  updatedAt: string;
  messages?: { id: string; taskId: string; role: string; content: string; createdAt: string }[];
  pinned: boolean;
  archived: boolean;
  waiting?: boolean;
};

type Activity = {
  id: string;                      // "<taskId>:<runnerItemId>"
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

**Incremental fetch.** Every conversation has a revision `rev` that goes up on any change in it (a
turn's status, steps, messages, questions or usage, or the title and flags); each turn carries the
`rev` of its last change. `GET /sessions/{id}` answers `{session, tasks, activities, questions, rev,
full}`. A client that already holds the conversation passes `?since=<rev>` and gets only the turns
changed after that revision, plus any still queued or running, each with all of its messages,
steps and questions; `session.messages` then holds only those turns' messages. With `full: false`
the client replaces those turns in what it holds and keeps the rest; with `full: true` (no `since`,
`since=0`, or a `since` ahead of mobd's revision, e.g. after restoring an older state) it replaces
everything. Turns are never deleted on their own; a deleted conversation answers 404.

JSON answers of 1 KB or more, and event streams (`/events`, `/tasks/{id}/events`), are
gzip-compressed for clients that send `Accept-Encoding: gzip`. A stream is compressed from its
first byte and sync-flushed after every event, so each event can be decoded at once; since a reply
streams as its whole text so far, the repeats shrink to little more than the new words while the
reply is under gzip's 32 KB window (longer replies still shrink about 4×). Images, video and
WebSocket upgrades are never compressed. A proxy in front of mobd must not buffer the stream.

## Desktop queue

- `MOBD_MAX_RUNNING` (default **1**) limits how many tasks run at once, across all sessions.
- `POST /tasks` always returns `queued`. A dispatcher starts queued tasks in FIFO order as slots free up.
- A session allows only one non-terminal task. A follow-up while queued or running returns **409**.
- Cancelling an undispatched queued task makes it `cancelled` straight away without starting a runner. A dispatched task can briefly still report `queued`; cancellation then waits for its worker to stop.
- `queuePosition` is set on queued tasks in every response and in `task` global events.
- `queueReason` says what a queued task waits for: a free slot (`maxRunning`), the desktop limit
  (`maxDesktops`), memory or disk below their floor (only new desktops wait), or its desktop
  starting. It is kept in memory, never persisted.
- Dispatch is asynchronous. Two immediate submissions can initially have positions 1 and 2; once the first becomes `running`, the second becomes queued position 1.

## Endpoints

Every business endpoint below requires a paired bearer credential. The internal
MCP endpoint instead checks its per-task token; see [security](security.md).
Invitation redemption and preview are documented in [pairing](pairing.md).

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/health`, `/healthz` | `apiVersion: 3`, `features`, `checks`, `runners` (each with `models`, `efforts`, `defaultModel`, `defaultEffort`, `permissions`), `maxRunning` |
| GET | `/sessions` | `?archived=false` (default) \| `true` \| `all`. Sorted pinned first, then `updatedAt` descending |
| GET | `/sessions/{id}` | `{session, tasks, activities, questions}` |
| PATCH | `/sessions/{id}` | Body `{title?, pinned?, archived?, model?, effort?}` → updated session. `model`/`effort` set the conversation's settings (`""` = the agent's default from settings.json); effort must be one of its runner's |
| DELETE | `/sessions/{id}` | **409** while a turn is active; removes its tasks, activities and artifacts → **204** |
| POST | `/tasks` | `{prompt, sessionId?, runner?, model?, effort?, attachments?}`; attachments are upload IDs; runner is pinned per session. `model`/`effort` only for a new conversation (they become its settings; omitted = the defaults in settings.json); a turn in an existing one uses the conversation's and **400** if it names any ([below](#settings)) |
| GET | `/settings` | settings.json and each agent's efforts and own defaults ([below](#settings)) |
| PUT | `/settings` | Replace settings.json: `{rev, defaultAgent, agents}` → same as GET; **409** `settings_changed` when `rev` is stale, **400** for invalid values |
| GET | `/tasks` | `?sessionId=`, `?status=`, `?limit=` (default 100, max 500) |
| GET | `/tasks/{id}` | Task snapshot |
| GET | `/tasks/{id}/events` | Per-task SSE: status, logs, messages, activities, usage and questions |
| POST | `/tasks/{id}/cancel` | Cancels queued or running tasks → **202** |
| GET | `/queue` | `{running: Task[], queued: Task[]}` |
| GET | `/events` | Global SSE (see below) |
| GET | `/screen` | Current desktop snapshot. `?format=png\|jpeg` (default jpeg), `?maxWidth=` (default full size), `?quality=1..100` (default 80). With `sessionDesktops`, `?sessionId=` picks the conversation; an asleep desktop answers with its last frame and `X-Mob-Desktop-State: asleep` without waking; **409** `no_desktop` when it has none |
| POST | `/uploads` | `multipart/form-data` field `file`; PNG/JPEG/GIF/WebP up to 20 MiB → `{id, name, mime, size, url}` |
| GET | `/artifacts/{id}` | Stored image (screenshots from tool results, uploads). Authenticated, `Cache-Control: no-store` |
| GET | `/files` | Shared directory path, upload limit, files and folders; `?path=` lists a folder; see [file transfers](file-transfers.md) |
| GET, HEAD | `/files/{path}` | Authenticated attachment with SHA-256, ETag, Range and If-Match support |
| POST | `/files/{path}/rename` | `{newName}`: rename a file or folder in place, never replacing anything |
| DELETE | `/files/{path}` | Delete a file, or a folder with its contents |
| POST | `/file-folders` | `{path}`: create a folder (empty folders of a folder upload) |
| POST, HEAD, PATCH, DELETE | `/file-uploads/`, `/file-uploads/{id}` | tus 1.0 creation, resume and cancellation; maximum 20 GiB |
| POST | `/file-uploads/{id}/complete` | Verify checksum and publish without overwriting; idempotent receipt |
| POST | `/tasks/{id}/questions/{qid}/answer` | Answer a pending question: `{answer}` (questions) or `{approved, answer?}` (approvals) → question; 409 if already settled ([runners.md](runners.md#asking-the-user-ask_user-request_approval)) |
| POST | `/tasks/{id}/steer` | `{prompt}`: add a message to the running turn (runners with `steer`; Codex app-server). 409 when the turn cannot take input |
| GET | `/probe/{code}` | No credential. **204** when `code` is a live probe code from the management socket (`mobd remote` checks an address reaches this mobd), else **404**. Grants nothing |
| POST | `/internal/tasks/{id}/ask` | Used by `mobd mcp` only (per-turn token); blocks until the question is settled |
| POST | `/stream` | Opens the live desktop: `{offer?, maxWidth, maxHeight, quality, sessionId?}` → **201** `{session, answer?, wsTicket, width, height, display}` ([desktop-streaming.md](desktop-streaming.md)). With `sessionDesktops`, `sessionId` is required (**400** `session_required`, **404** `session_not_found`, **409** `no_desktop`); opening wakes an asleep desktop, and the stream ends with the reason when the desktop closes |
| GET | `/desktops` | `{desktops: Info[], memory, disk}`, largest memory first; `{desktops: []}` with one shared desktop |
| POST | `/desktops/{sessionId}` | Starts or wakes the conversation's desktop without a turn → **200** Info. **503** `low_memory`, `low_disk`, `max_desktops`, `desktop_unavailable`; **501** with a shared desktop |
| DELETE | `/desktops/{sessionId}` | Stops the conversation's running turn, then closes its desktop → **204** |
| POST | `/desktops/{sessionId}/seed` | Makes the conversation's browser profiles the seed for new conversations → **204**; **409** `browser_running`, `no_profile` |
| DELETE | `/stream/{id}` | Ends the client's live desktop session |
| GET | `/stream/ws` | `?session=` (and `?ticket=` for webviews): the same session over a WebSocket where UDP cannot pass. 403 for unknown origins, 404 unknown session, 409 if WebRTC already carries it |

### Health

Example response (abridged):

```json
{
  "ok": true, "service": "mobd", "apiVersion": 3,
  "runner": "codex", "maxRunning": 1,
  "mobMcp": "/path/to/mobd",
  "features": ["queue", "activities", "sessionAdmin", "events", "screen", "uploads", "fileTransfers", "taskOptions", "runners", "questions", "steer", "pairing", "stream", "streamResize", "settings"],
  "checks": {
    "codex":     { "ok": true,  "detail": "/Users/me/.local/bin/codex" },
    "actuation": { "ok": true,  "detail": "built-in (mobd mcp)" },
    "accessibility":   { "ok": false, "detail": "grant Accessibility to the app that launches mobd" },
    "screenRecording": { "ok": true },
    "display":   { "ok": true,  "detail": "unlocked and awake" }
  }
}
```

`ok` is true when mobd is serving. `checks` explain readiness without blocking anything and are cached for 10 s. Each check is `{ok, unknown?, detail?}`; permission checks come from `CGPreflightPostEventAccess` / `CGPreflightScreenCaptureAccess` in mobd and are `unknown` off macOS. `agents` reports every agent mobd supports, enabled or not: `{name, label, state, enabled, detail, install?}` with `state` `ready` (enabled and working), `disabled` (installed, not in `config.json`'s `runners`), `missing` (not installed; `install` is the command) or `error` (installed but failing, e.g. an unsupported version). mobd only checks that each agent is installed in a supported version; the agent's own login and models are not checked, and a mistake there shows as the agent's error on the first turn. Agents are checked in parallel, each for up to 12 s, and not with the request's deadline. `runners` lists each enabled runner's capabilities and readiness ([runners.md](runners.md)), with its models, defaults and permissions from settings.json; a default that settings.json leaves empty is the agent's own. `display` reports a locked console or sleeping display (snapshots need it awake; the live view wakes it); `streamSessions` counts open live desktop sessions.

### Settings

`config.json` says how mobd itself runs (listen address, agents enabled and their programs, work
directories…). **settings.json**, in the data directory, holds the runtime settings of agents and
conversations, shared by every paired device and changed in the apps (feature `settings`): the
default agent for new conversations, and per agent its model list, default model, default effort
and permissions. mobd creates it on the first start, complete and explicit, and adds an entry when
an agent is enabled later:

```json
{
  "rev": 1,
  "defaultAgent": "codex",
  "agents": {
    "codex":  { "models": ["gpt-6-astra"], "defaultModel": "", "defaultEffort": "", "permissions": "bypass" },
    "claude": { "models": ["opus", "sonnet", "haiku"], "defaultModel": "", "defaultEffort": "", "permissions": "bypass" }
  }
}
```

The default agent is the first enabled one. Claude starts with its aliases; Codex and Pi with the
default model of their own config, if any. An empty `defaultModel`/`defaultEffort` sends none, so
the agent uses its own default. An unreadable file is moved aside (`settings.json.bad-…`) and a
fresh one written.

`GET /settings` answers `{settings, agents, installed}` (`installed` is the same list as `/health`'s `agents`): `settings` is the file, `agents` gives each enabled
agent's `label`, `efforts`, and its own defaults (`agentDefaultModel`, `agentDefaultEffort`) for
labelling an empty default. Edit `settings` and `PUT` it back whole with the same `rev`. A stale
`rev` (another device saved first) gets **409** `settings_changed`: reload and edit again. Every
enabled agent needs an entry; values are validated (models as model names, at most 50 per agent,
efforts from the agent's list, permissions `bypass`/`approve`; Codex `approve` needs `codexMode`
`app-server`). Changes apply to the next turn without a restart; each is logged with the device's
name and sent to the other devices as a `settings` event.

**Conversation settings:** a conversation has `model` and `effort`. A new one takes the request's
or else the defaults of settings.json, and keeps them when those change later. `PATCH
/sessions/{id}` changes them; `""` follows the default. Each turn uses them and records what it
actually ran with (`model` may be replaced by the model the agent reports). The runner is fixed
when a conversation starts.

### Global events (`GET /events`)

SSE with monotonic `id`s from a 1024-entry in-memory ring. `Last-Event-ID` / `?after=` resume. If the cursor is older than the ring, the server first sends `event: reset`, and the client should refetch snapshots.

| `event` | `data` |
| --- | --- |
| `session` | session summary (no messages) after create/update/status change |
| `session.deleted` | `{id}` |
| `task` | task snapshot (with `queuePosition`) on every status change |
| `queue` | same shape as `GET /queue` whenever it changes |
| `desktop` | desktop Info (below) on every state or viewer change; `reason: "oomKill"` when the kernel ended one of its apps |
| `memory`, `disk` | `{level: ok\|low\|critical, …}` when the level changes |
| `settings` | same as `GET /settings`, after the machine settings change |
| `reset` | `{}` |

Per-task timelines stay on `/tasks/{id}/events`, including `question` events for
question/approval changes. The global stream carries session/task summaries with
their `waiting` flags; question snapshots are also available in `GET /sessions/{id}`.

## Conversation desktops (Linux)

`/health` lists the `sessionDesktops` feature, `maxDesktops`, `desktops: {count, asleep,
memoryBytes, diskBytes}` and the checks `xvfb`, `windowManager`, `dbus`, `keyboard`, `cgroup`,
`display`, `encoder`, `browsers`, `memory` and `disk` (memory and disk read live). A desktop Info:

```ts
type DesktopInfo = {
  sessionId: string; title?: string; display?: string;   // ":101"
  width: number; height: number; scale: number;          // points
  state: "starting" | "running" | "asleep" | "waking" | "closing" | "closed" | "failed";
  reason?: string;          // why it closed or failed: archived, deleted, user, stop, crashed, reboot, thawFailed...
  memoryBytes: number; diskBytes: number;
  startedAt?: string; lastUsedAt?: string; activeTaskId?: string; viewers: number;
};
```

A desktop starts with its conversation's first turn (or `POST /desktops/{id}`), sleeps after
`desktopSleepAfter` with no turn, viewer, CPU use or screen change, and wakes for a turn or a live
view. Archiving or deleting the conversation closes it; a running turn is cancelled first. Errors
for these conditions carry a machine-readable `code` next to `error`.

## API configuration

See the [configuration reference](../README.md#configuration-reference) for all
JSON keys, environment variables and defaults.

| Variable | Default | Meaning |
| --- | --- | --- |
| `MOBD_MAX_RUNNING` | `1` | Tasks allowed to run at once (one desktop → 1) |
| `MOBD_DATA_DIR` | directory of `MOBD_STATE_PATH` | Root for `artifacts/` (images) |
| `MOBD_FILES_DIR` | `~/mob-files` | Folder shared with paired devices; tus staging in its hidden `.uploads/` |
| `MOBD_QUESTION_TIMEOUT` | `30m` | How long a turn waits for an answer to `ask_user` / `request_approval` |

## Internal structure

```text
cmd/mobd            CLI: init, serve, start, stop, restart, status, logs, mcp, link, clients,
                    connections, auth, --version
cmd/stream-probe    Command-line live desktop viewer for testing
internal/api        HTTP handlers, SSE, global event hub
internal/pairing    Invitations, device credentials, WebSocket tickets, auth.json
internal/control    Private Unix-socket management API used by link/clients/connections/auth
internal/stream     Live desktop sessions: WebRTC (Pion), WebSocket fallback, rate control, input
internal/task       Store (snapshot persistence), queue ordering, activities
internal/runner     Registry, Codex app-server/exec and Claude adapters, shared CLI lifecycle
internal/desktop    Capture, H.264 encoding, snapshots and input, one backend per OS (macOS; placeholders elsewhere)
internal/mcpserver  mobd mcp: desktop tools, questions and approvals over stdio
internal/artifact   File store for screenshots and uploads (random immutable IDs)
internal/config     JSON config, environment overrides and CLI flags
internal/service    Per-user macOS LaunchAgent and saved service configuration
```

- `runner.Runner` interface: `Start(ctx, *task.Task, Options, EmitFunc) error`. Codex and Claude implement it; optional `runner.Steerer` enables mid-turn input.
- In-memory debug logs are capped per task (oldest log lines dropped first). Persisted messages and activities are never dropped from replay.
- Every state change goes through the atomic snapshot commit. Image bytes never enter the snapshot.
