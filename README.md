# mobd

Office control plane: HTTP task API + Codex and Claude Code runners. The same binary is the desktop actuation MCP server (`mobd mcp`, [ADR 0008](docs/adr/0008-actuation-in-mobd.md)); Codex launches it for each task.

Desktop clients connect over HTTP/SSE and the VNC relay. The desktop app lives in
[xrdavies/mob](https://github.com/xrdavies/mob); this repository builds independently.

Source: Go 1.27+ and Xcode Command Line Tools on macOS. Run `make build`,
`make check` (Go vet/tests plus Node.js 24+ distribution tests), or `make race`.
See [architecture](docs/architecture.md), [API v3](docs/mobd-v3.md),
[runners](docs/runners.md) and [repository split notes](docs/repository-split.md).

## Install / run (office Mac)

Published packages and the one-line installer are served from the public R2 download
domain, independently of the private source repository. See the product website's
download page and the [release/installation guide](docs/releases.md#4-安装和升级).
The installer uses `~/.local/bin`, verifies the archive and Apple signing identity,
and preserves configuration. Stop an existing service before upgrading.

The standalone binary needs no source checkout, Go, Makefile or service scripts:

```bash
mobd --version
mobd init                       # creates ~/.mobd/config.json; never overwrites
# Edit listen to "0.0.0.0:8080" for LAN clients; init defaults to loopback.
# Set workdir and codexBin explicitly for a stable background environment.
mobd start                      # background service until stopped/logout
mobd status                     # address and /health check
mobd logs                       # follow logs (Ctrl-C to stop following)
mobd restart                    # reload config/environment/flags
mobd stop                       # stop; also removes login startup
mobd start --login               # opt into startup at user login
mobd serve                      # foreground; Ctrl-C to stop
```

The background service is a per-user macOS LaunchAgent, not a root daemon. `start`
rejects an already loaded service or occupied port. `restart` preserves login
startup and reuses the saved config path/flags when no new options are supplied. Configuration resolved at start is saved privately to
`~/.mobd/service.json`; the LaunchAgent uses that snapshot, absolute Codex/Claude paths
when available, and the invoking shell's PATH/workdir. Config edits take effect
on `restart`. There is one managed service per user. `stop`, `status` and `logs`
operate on that service, independently of the caller's environment. Logs are in
`~/.mobd/mobd.log`. Use a stable binary path and stop before replacing the binary.

Developers build/run the same CLI:

```bash
make build
make init
make start ARGS='--listen 0.0.0.0:8080'
make restart
make status
make logs
make stop
make serve ARGS='--config ./config.dev.json'
```

### Configuration

JSON `mcpArgs`, `corsOrigins`, `codexModels`, `claudeModels` and `runners` are arrays of strings. `questionTimeout` is a Go duration string such as `"30m"`.

Priority: **command-line flags > exported `MOBD_*` variables > JSON > defaults**.
The default config is `~/.mobd/config.json`; a missing default file is allowed,
while an explicitly supplied `--config` path must exist. Unknown fields, malformed
JSON, invalid addresses and invalid concurrency values fail with an error.
`init` creates a private directory/file (0700/0600). No `.env` is read or executed.

Supported flags: `--config`, `--listen`, `--codex-bin`, `--workdir`,
`--state-path`, `--max-running`. Put other options in JSON or exported variables.
Relative filesystem paths resolve from the invoking directory. Use absolute paths
in long-lived configs; JSON does not expand `~`, `$HOME`, or shell expressions.

All JSON keys and their environment equivalents are listed below. An exported
empty string overrides JSON too: it clears optional values, but is invalid for
required settings. `MOBD_MOB_MCP_ARGS` must be a JSON array (no comma fallback).
Existing snapshots stay at `~/Library/Application Support/mob/state.json` on
macOS, so this upgrade does not hide or move conversation history. Explicit
`statePath` / `MOBD_STATE_PATH` still takes precedence. The old `scripts/.env`
is not automatically migrated; export it yourself once or copy its values into
JSON. Stop any old script-managed process before starting the LaunchAgent.

If `codex` is not on `PATH`, creating a task still works: the runner logs a clear error and marks the task **failed** (safe stub — mobd does not crash).

### JSON configuration (recommended for end users)

End users configure `~/.mobd/config.json` and run the installed `mobd` directly;
no `.env`, Makefile or source checkout is required. Create a minimal config once:

```bash
mobd init
# Edit ~/.mobd/config.json before starting.
mobd start
mobd status
```

[`config.example.json`](config.example.json) is a more complete example
with runner settings. If you have downloaded the example or checked out the
repository, run the following from the directory containing it:

```bash
mkdir -p -m 700 "$HOME/.mobd"
cp -n config.example.json "$HOME/.mobd/config.json"  # preserves an existing config
chmod 600 "$HOME/.mobd/config.json"
```

Use either `mobd init` or the copy command for initial setup. If the config
already exists, edit it and use the example as a reference; neither command
overwrites or merges your existing settings.

Before starting, replace `/Users/you/Projects` with an existing absolute working
directory. Set `codexBin` / `claudeBin` to executable names on your shell's PATH,
or their absolute paths. Claude uses `workdir` unless you set `claudeWorkdir`.
To enable only Codex, set `runners` to `["codex"]` and keep `defaultRunner` as
`"codex"`. Permissions use `"bypass"` (the current default, skips approval prompts);
choose `"approve"` to request approval for supported actions.

The example listens on `127.0.0.1:8080`; change `listen` to `"0.0.0.0:8080"`
for LAN/VPN clients. Optional fields omitted from the example use their defaults,
including the existing conversation and artifact locations. JSON must not contain
comments or trailing commas, and paths do not expand `~` or `$HOME`.

After editing the config, run `mobd restart` to apply it. Use `mobd serve` to run
in the foreground, or select another file with `mobd start --config /path/to/config.json`.
Exported `MOBD_*` variables and command-line flags still take precedence over JSON.

### Environment variables (developers only)

For development, the repository root contains a [mobd `.env.example`](.env.example).
From the repository root, copy it once and edit `.env` for your machine:

```bash
cp -n .env.example .env   # preserves an existing .env
# Edit .env: listen address, runner executables and working directories.
```

This developer example listens on `127.0.0.1:8080`; use `0.0.0.0:8080` for LAN/VPN
clients. Its working directory is the directory where you source the file
(`$PWD`); use a fixed absolute path if needed. This file configures **mobd**;
[the desktop environment example](https://github.com/xrdavies/mob/blob/main/.env.example) configures the frontend.

Load the variables into the current shell and start the background service:

```bash
set -a
source ./.env
set +a
make start
make status
```

`set -a` exports the assignments so the child mobd process can read them.
Use `make serve` instead of `make start` for foreground operation. If you
installed the standalone binary, use `mobd start` / `mobd serve` / `mobd status`
after the same `source` steps. Neither mobd nor Makefile reads `.env` automatically.
`mobd init` is optional: an existing JSON config still loads, and exported
variables override its values; other fields use JSON or defaults.

After editing `.env`, reload it and restart the service:

```bash
set -a
source ./.env
set +a
make restart             # or: mobd restart
```

The LaunchAgent keeps the resolved configuration from `start` / `restart`,
so it continues using those values after you close the shell. Removing a line
from `.env` does not unset a variable already exported in your shell; use
`unset VARIABLE_NAME` before reloading to fall back to JSON/defaults. An empty
assignment is an explicit override, not a request to use the default. `.env`
is ignored by Git; commit only the example.

## curl examples

```bash
# health
curl -s localhost:8080/health | jq .

# first turn: creates a conversation and returns its sessionId
curl -s -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Use mob-mcp to open Safari"}' | jq .

# follow-up: reuse the returned sessionId (never use Codex --last)
curl -s -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"sessionId":"SESSION_ID","prompt":"Now open example.com in that window"}' | jq .

# list conversations, then read the full user/assistant transcript and turns
curl -s localhost:8080/sessions | jq .
curl -s localhost:8080/sessions/SESSION_ID | jq .

# list (filters: sessionId, status, limit 1-500)
curl -s 'localhost:8080/tasks?status=running' | jq .

# get one (replace ID)
curl -s localhost:8080/tasks/TASK_ID | jq .

# SSE events
curl -N localhost:8080/tasks/TASK_ID/events

# resume delivery after event 12 (Last-Event-ID takes precedence over ?after=)
curl -N -H 'Last-Event-ID: 12' localhost:8080/tasks/TASK_ID/events

# cancel
curl -s -X POST localhost:8080/tasks/TASK_ID/cancel | jq .
```

v3 additions ([design](docs/mobd-v3.md)):

```bash
# per-turn model / reasoning effort / image attachments
curl -s -X POST localhost:8080/uploads -F file=@shot.png | jq .      # → {id, url, …}
curl -s -X POST localhost:8080/tasks -H 'Content-Type: application/json' \
  -d '{"prompt":"What is in this image?","effort":"high","attachments":["UPLOAD_ID"]}' | jq .

# desktop queue (one running task by default)
curl -s localhost:8080/queue | jq .

# session admin
curl -s -X PATCH localhost:8080/sessions/SESSION_ID -d '{"title":"Weekly report","pinned":true}' | jq .
curl -s -X PATCH localhost:8080/sessions/SESSION_ID -d '{"archived":true}' | jq .
curl -s 'localhost:8080/sessions?archived=all' | jq .
curl -s -X DELETE localhost:8080/sessions/SESSION_ID -o /dev/null -w '%{http_code}\n'   # 204, 409 while active

# timeline: messages + activities (tool calls, commands, plans…) + task usage
curl -s localhost:8080/sessions/SESSION_ID | jq '.activities'

# live view: WebSocket relay to Screen Sharing (ADR 0010)
# ws://localhost:8080/vnc   ← svnc / the App connect here

# global live stream (session / session.deleted / task / queue / reset)
curl -N localhost:8080/events

# desktop snapshot without VNC; tool screenshots and uploads
curl -s 'localhost:8080/screen?maxWidth=720' -o screen.jpg
curl -s localhost:8080/artifacts/ARTIFACT_ID -o artifact.png
```

With shared secret:

```bash
curl -s -X POST localhost:8080/tasks \
  -H "Authorization: Bearer $MOBD_SHARED_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"…"}'
```

## Configuration reference

| JSON key | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `listen` | `MOBD_LISTEN` | `:8080` | HTTP bind |
| `codexBin` | `MOBD_CODEX_BIN` | `codex` | Codex CLI |
| `workdir` | `MOBD_CODEX_WORKDIR` | process cwd | Codex working directory |
| `mcpCommand` | `MOBD_MOB_MCP_COMMAND` | this `mobd` binary | MCP server command for Codex `-c` injection; empty = built-in `mobd mcp` |
| `mcpArgs` | `MOBD_MOB_MCP_ARGS` | `["mcp"]` when built-in, else `[]` | JSON array of args (external command only) |
| `sharedSecret` | `MOBD_SHARED_SECRET` | empty | Optional auth for POST create/cancel — **deferred** until Phase 2 App; leave unset |
| `vncViewUrl` | `MOBD_VNC_VIEW_URL` | empty | Returned on tasks / health for svnc viewOnly |
| `statePath` | `MOBD_STATE_PATH` | user config directory + `mob/state.json` | Private conversation snapshot; on macOS: `~/Library/Application Support/mob/state.json` |
| `maxRunning` | `MOBD_MAX_RUNNING` | `1` | Tasks running at once across all sessions; others wait in the FIFO desktop queue |
| `dataDir` | `MOBD_DATA_DIR` | directory of `MOBD_STATE_PATH` | Root of `artifacts/` (tool screenshots, uploads) |
| `codexModels` | `MOBD_CODEX_MODELS` (legacy `MOBD_MODELS`) | empty | Comma-separated model IDs advertised on `/health` for client pickers |
| `vncAddr` | `MOBD_VNC_ADDR` | `127.0.0.1:5900` | VNC server (macOS Screen Sharing) relayed by `GET /vnc` and checked by `/health` |
| `corsOrigins` | `MOBD_CORS_ORIGINS` | `http://localhost:1420,http://127.0.0.1:1420,tauri://localhost,http://tauri.localhost` | Comma-separated exact origins; a nonempty value replaces the defaults |
| `questionTimeout` | `MOBD_QUESTION_TIMEOUT` | `30m` | How long a turn waits for your answer to the agent's question or approval request |
| `runners` | `MOBD_RUNNERS` | `codex,claude` | Enabled runners |
| `codexMode` | `MOBD_CODEX_MODE` | `app-server` | `app-server` (approvals, steering, streaming) or `exec` |
| `codexPermissions` | `MOBD_CODEX_PERMISSIONS` | `bypass` | `approve` makes Codex ask you before commands and file edits |
| `claudePermissions` | `MOBD_CLAUDE_PERMISSIONS` | `bypass` | `approve` makes Claude ask you before shell commands and other prompted actions |
| `claudeSettingSources` | `MOBD_CLAUDE_SETTING_SOURCES` | `project` | Claude settings loaded; `all` includes your personal `~/.claude` |
| `defaultRunner` | `MOBD_DEFAULT_RUNNER` | `codex` | Runner for new conversations ([runners](docs/runners.md)) |
| `claudeBin` | `MOBD_CLAUDE_BIN` | `claude` | Claude Code executable |
| `claudeWorkdir` | `MOBD_CLAUDE_WORKDIR` | Codex workdir | Claude working directory |
| `claudeModels` | `MOBD_CLAUDE_MODELS` | `opus,sonnet,haiku` | Claude model picker |


## Conversations and runner lifecycle

Update the office **mobd** together with the client. `/health` advertises `apiVersion: 3` with `features`, `runners` (each with models, efforts and readiness), the default runner's `codexModels`/`efforts`, and readiness `checks`; v2 clients keep working because v3 only adds fields and routes. Old in-memory tasks cannot be recovered after upgrading/restarting.

- Every new turn starts `queued`. A FIFO dispatcher runs at most `MOBD_MAX_RUNNING` turns at once so agents never fight over the desktop; queued tasks report `queuePosition`, and cancelling one finishes it without starting Codex. Queued turns are marked failed on restart, like running ones.
- Codex `mcp_tool_call`, `command_execution`, `reasoning`, `todo_list`, `file_change`, `web_search` and `error` items become persisted **activities** (upserted by item ID, text capped at 16 KiB); tool screenshots are saved as artifacts. `turn.completed` usage is stored on the task.
- `POST /tasks` without `sessionId` creates a conversation and its first turn atomically. With `sessionId`, it continues that conversation; an active turn returns **409**, without adding a user message.
- The first turn uses `codex exec --json`; following turns use `codex exec … resume <resumeId> -`. Each conversation is pinned to the runner that started it ([runners](docs/runners.md)). Prompts travel through stdin. Completed `agent_message` items become assistant messages; stderr/tool output stays in debug logs. Replies appear when Codex completes a message item, rather than token by token.
- Conversation IDs, runner resume IDs, messages and task states are written atomically before acknowledgement/delivery. The file has mode **0600**. Keep it and Codex's own session directory on the office host. Run one mobd writer per state path; this snapshot store is intended for a single office daemon.
- On restart, interrupted turns become **failed** and are never rerun automatically. Completed messages and resume IDs remain available for continuation.
- Cancel returns **202** and keeps the conversation busy until the process stops and output readers finish. SIGINT/SIGTERM also cancels active turns before shutdown.
- Use a service supervisor that cleans up child processes after a hard daemon crash/SIGKILL; otherwise stop any orphaned Codex process before resuming its conversation. Snapshot recovery marks the interrupted turn failed, but cannot stop a process left by a dead daemon.
- SSE events have monotonic per-task `id` fields. `Last-Event-ID` / `?after=` resume in-process replay without duplicating messages. Raw logs are memory-only and capped at 2000 lines per task; after a daemon restart, `GET /sessions/:id` is the authoritative transcript and task snapshot.

The runner follows the session/turn separation and structured message recovery used by [Paseo](https://github.com/getpaseo/paseo/blob/main/packages/server/src/server/agent/providers/codex-app-server-agent.ts) and [Orca](https://github.com/stablyai/orca/blob/main/src/main/codex/codex-app-server-session.ts). MOB keeps the existing CLI runner; app-server RPC is a later option for token deltas or interactive approvals. CLI event and resume behavior follows the [Codex non-interactive documentation](https://developers.openai.com/codex/noninteractive/).

CORS covers API responses and SSE, plus `OPTIONS` preflight for `GET`/`POST` with `Content-Type`, `Authorization`, or `X-Mob-Token`. Use the client's origin (scheme, host, and port, without a trailing slash), not the office API address. CORS does not replace authentication or LAN/VPN access controls.
