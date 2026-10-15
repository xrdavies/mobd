# mobd

Office control plane: HTTP task API + Codex and Claude Code runners. The same binary provides the desktop actuation MCP server (`mobd mcp`), launched by the agent for each task.

Desktop clients connect over HTTP/SSE and the VNC relay. The desktop app lives in
[xrdavies/mob](https://github.com/xrdavies/mob); this repository builds independently.

Source: Go 1.27+ and Xcode Command Line Tools on macOS. Run `make build`,
`make check` (Go vet/tests plus Node.js 24+ distribution tests), or `make race`.
See [architecture](docs/architecture.md), [API reference](docs/mobd-v3.md)
and [runners](docs/runners.md).

Releasing: configure the GitHub `release` environment using the [release guide](docs/releases.md),
then run `make release VERSION=vX.Y.Z` with a new stable version from a clean `main` matching `origin/main`
(Node.js 24+ and Git required). CI signs and notarizes the binary and creates a draft
Release; test its archive, then publish the stable Release on GitHub to distribute it through R2.

## Install / run (office Mac)

Install on macOS 13+ with Apple Silicon, as your normal user:

```bash
curl -fsSL https://mob-releases.0xd.app/mobd/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

Add the `export PATH=…` line to `~/.zshrc` to make it available in new terminals.
The installer downloads the latest stable version from [the public manifest](https://mob-releases.0xd.app/mobd/latest.json),
checks SHA-256 and the Apple signing identity, and installs only `mobd` into
`~/.local/bin`. The archive also includes `LICENSE` and `README.md`.
No sudo, Go, Node.js, Python or source checkout is needed to install the binary.
See the [release/installation guide](docs/releases.md#4-安装和升级) for a pinned version or custom directory.

The standalone binary needs no source checkout, Go, Makefile or service scripts:

```bash
mobd --version
mobd init                       # creates ~/.mobd/config.json; never overwrites
# init defaults to "0.0.0.0:8080" for LAN clients.
# Set workdir and codexBin explicitly for a stable background environment.
mobd start                      # background service until stopped/logout
mobd status                     # address and /health check
mobd logs                       # follow logs (Ctrl-C to stop following)
mobd restart                    # reload config/environment/flags
mobd stop                       # stop; also removes login startup
mobd start --login               # opt into startup at user login
mobd serve                      # foreground; Ctrl-C to stop
```

Install and authenticate the agent CLI you plan to use (Codex or Claude Code).
Grant Accessibility and Screen Recording to the responsible app or mobd process,
and enable macOS Screen Sharing for live viewing. For direct LAN/VPN access,
configure `listen` for that interface; use loopback when using the remote-access
proxy below. `mobd init` sets `listen` to `0.0.0.0:8080` (all IPv4 interfaces).
The download domain serves files; clients connect to your office Mac's mobd URL.
Follow the [office smoke checklist](docs/office-smoke-checklist.md) to verify the setup.

To upgrade, finish active tasks, run `mobd stop`, rerun the installer, then run
`mobd start` (or `mobd start --login` to restore login startup). Configuration,
conversations and attachments are preserved. Stop a foreground `mobd serve`
process manually before installing. mobd does not use Tauri updater.

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

### Connect from home

The proposed connection setup offers three choices per office device:

- **Private network:** the office machine and remote clients use the user's
  Tailscale network or existing VPN.
- **User-managed Funnel:** the user enables Tailscale Funnel at the office;
  remote clients connect over public HTTPS without installing Tailscale.
- **Our relay:** clients sign in and select a bound device through our managed
  gateway; device enrollment, authorization and relay infrastructure are still to be built.

All three reuse mandatory per-device authentication: pair once, then authenticate
with a persistent credential until revoked, deleted or authentication is reset.
There is no anonymous mode. mobd implements [pairing](docs/pairing.md); desktop and
mobile client integration and the mode-selection UI are follow-up work. Existing
clients need upgrading before they can connect to this version.

```bash
mobd link --url https://office.example.com  # QR + one-use link, expires in 10 minutes
mobd clients list
mobd connections
mobd clients revoke CLIENT_ID
mobd clients delete CLIENT_ID
mobd link --close                         # close unused invitations only
mobd auth reset --yes                     # invalidate all credentials; retain history
```

Commands use a private local management socket. Invitations are temporary;
paired credentials survive invitation expiry and daemon/client restarts.

For direct LAN access, `mobd link --url http://192.168.1.20:8080` prints an HTTP
risk warning to stderr and still generates the link and QR. HTTP does not encrypt
invitations, credentials or data; an encrypted tunnel can protect the network path.
Pairing remains mandatory. Use the actual client-accessible address; link generation
does not test connectivity or change the listener.

For separate home and office networks, use **Tailscale + Tailscale Serve** when
both devices can install Tailscale. Keep mobd on `127.0.0.1:8080`; Serve provides
a private HTTPS URL for the home mob client, carrying API requests, SSE and the
`/vnc` WebSocket relay through one endpoint. No office public IP or router port
forward is needed. Reuse an existing company VPN when available.

Follow the [connection setup guide](docs/connection-setup.md) for step-by-step LAN,
Tailscale Funnel and self-hosted HTTPS relay setup, pairing tests and shutdown.
The [remote-access guide](docs/remote-access.md) covers the mode design, private
Tailscale Serve deployment and an SSH alternative.
Public deployments require HTTPS/WSS and authenticated requests, including SSE and
VNC. WebView clients can use the [short-lived VNC ticket](docs/pairing.md#实时桌面票据)
when they cannot set WebSocket headers. Validate real client and tunnel behavior
before deployment; the managed relay is not implemented.

### Configuration

JSON `mcpArgs`, `corsOrigins`, `codexModels`, `claudeModels` and `runners` are arrays of strings. `questionTimeout` is a Go duration string such as `"30m"`.

Priority: **command-line flags > exported `MOBD_*` variables > JSON > defaults**.
The default config is `~/.mobd/config.json`; a missing default file is allowed,
while an explicitly supplied `--config` path must exist. Unknown fields, malformed
JSON, invalid addresses and invalid concurrency values fail with an error.
`init` creates a private directory/file (0700/0600). No `.env` is read or executed.

Supported flags: `--config`, `--listen`, `--codex-bin`, `--workdir`,
`--state-path`, `--max-running`, `--control-socket`. Put other options in JSON or exported variables.
Relative filesystem paths resolve from the invoking directory. Use absolute paths
in long-lived configs; JSON does not expand `~`, `$HOME`, or shell expressions.

All JSON keys and their environment equivalents are listed below. An exported
empty string overrides JSON too: it clears optional values, but is invalid for
required settings. `MOBD_MOB_MCP_ARGS` must be a JSON array (no comma fallback).
Conversation snapshots default to `~/Library/Application Support/mob/state.json`
on macOS. Set `statePath` / `MOBD_STATE_PATH` to use a different location.

If the configured agent executable is unavailable, its runner reports an error
and marks the task **failed**.

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

The example listens on `127.0.0.1:8080`; keep it for Tailscale Serve or SSH tunnels.
For direct LAN/VPN clients, bind to the intended private interface (or
`"0.0.0.0:8080"` behind appropriate network controls).
Optional fields omitted from the example use their defaults,
including the conversation and artifact locations. JSON must not contain
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

This developer example listens on `127.0.0.1:8080`; keep it for proxy/tunnel access,
or choose a private interface for direct LAN/VPN clients.
Its working directory is the directory where you source the file
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

First generate an invitation with `mobd link`, then redeem it with `POST /pair`
as described in the [pairing protocol](docs/pairing.md). Set `MOBD_TOKEN` to the
returned device credential. Every example below requires it, including reads.
Do not put the credential in URLs or share it between devices.

```bash
# health
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/health | jq .

# first turn: creates a conversation and returns its sessionId
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Use mob-mcp to open Safari"}' | jq .

# follow-up: reuse the returned sessionId
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"sessionId":"SESSION_ID","prompt":"Now open example.com in that window"}' | jq .

# list conversations, then read the full user/assistant transcript and turns
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/sessions | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/sessions/SESSION_ID | jq .

# list (filters: sessionId, status, limit 1-500)
curl -H "Authorization: Bearer $MOBD_TOKEN" -s 'localhost:8080/tasks?status=running' | jq .

# get one (replace ID)
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/tasks/TASK_ID | jq .

# SSE events
curl -H "Authorization: Bearer $MOBD_TOKEN" -N localhost:8080/tasks/TASK_ID/events

# resume delivery after event 12 (Last-Event-ID takes precedence over ?after=)
curl -H "Authorization: Bearer $MOBD_TOKEN" -N -H 'Last-Event-ID: 12' localhost:8080/tasks/TASK_ID/events

# cancel
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks/TASK_ID/cancel | jq .
```

Task options, attachments, session management and live updates
([API reference](docs/mobd-v3.md)):

```bash
# per-turn model / reasoning effort / image attachments
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/uploads -F file=@shot.png | jq .      # → {id, url, …}
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks -H 'Content-Type: application/json' \
  -d '{"prompt":"What is in this image?","effort":"high","attachments":["UPLOAD_ID"]}' | jq .

# desktop queue (one running task by default)
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/queue | jq .

# session admin
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X PATCH localhost:8080/sessions/SESSION_ID -d '{"title":"Weekly report","pinned":true}' | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X PATCH localhost:8080/sessions/SESSION_ID -d '{"archived":true}' | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s 'localhost:8080/sessions?archived=all' | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X DELETE localhost:8080/sessions/SESSION_ID -o /dev/null -w '%{http_code}\n'   # 204, 409 while active

# timeline: messages + activities (tool calls, commands, plans…) + task usage
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/sessions/SESSION_ID | jq '.activities'

# live view: WebSocket relay to Screen Sharing
# ws://localhost:8080/vnc   ← svnc / the App connect here

# global live stream (session / session.deleted / task / queue / reset)
curl -H "Authorization: Bearer $MOBD_TOKEN" -N localhost:8080/events

# desktop snapshot without VNC; tool screenshots and uploads
curl -H "Authorization: Bearer $MOBD_TOKEN" -s 'localhost:8080/screen?maxWidth=720' -o screen.jpg
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/artifacts/ARTIFACT_ID -o artifact.png
```

## Configuration reference

| JSON key | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `listen` | `MOBD_LISTEN` | `:8080` | HTTP bind |
| `codexBin` | `MOBD_CODEX_BIN` | `codex` | Codex CLI |
| `workdir` | `MOBD_CODEX_WORKDIR` | process cwd | Codex working directory |
| `mcpCommand` | `MOBD_MOB_MCP_COMMAND` | this `mobd` binary | MCP server command for Codex `-c` injection; empty = built-in `mobd mcp` |
| `mcpArgs` | `MOBD_MOB_MCP_ARGS` | `["mcp"]` when built-in, else `[]` | JSON array of args (external command only) |
| `sharedSecret` | `MOBD_SHARED_SECRET` | empty | Removed; nonempty values fail startup with migration instructions. Pair each client instead. |
| `controlSocket` | `MOBD_CONTROL_SOCKET` | `~/.mobd/run/control.sock` | Private local management; parent directory must be owned by this user with mode 0700 |
| `vncViewUrl` | `MOBD_VNC_VIEW_URL` | empty | Returned on tasks / health for svnc viewOnly |
| `statePath` | `MOBD_STATE_PATH` | user config directory + `mob/state.json` | Private conversation snapshot; on macOS: `~/Library/Application Support/mob/state.json` |
| `maxRunning` | `MOBD_MAX_RUNNING` | `1` | Tasks running at once across all sessions; others wait in the FIFO desktop queue |
| `dataDir` | `MOBD_DATA_DIR` | directory of `MOBD_STATE_PATH` | Root of `artifacts/` (tool screenshots, uploads) and private `auth.json` |
| `codexModels` | `MOBD_CODEX_MODELS` | empty | Comma-separated model IDs advertised on `/health` for client pickers; `MOBD_MODELS` is also accepted, with lower priority |
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

mobd and the desktop client are released independently. `/health` advertises `apiVersion: 3` with `features`, `runners` (each with models, efforts and readiness), the default runner's `models`/`efforts`, and readiness `checks`. Clients should use these capabilities rather than require matching product versions. API version 3 is separate from the binary release version printed by `mobd --version`.

- Every new turn starts `queued`. A FIFO dispatcher runs at most `MOBD_MAX_RUNNING` turns at once; keep the default `1` for a shared desktop. Dispatch is asynchronous, so a newly submitted turn can still be queued before its runner starts. Queued tasks report `queuePosition`; cancelling an undispatched task finishes it without starting a runner. Queued turns are marked failed on restart, like running ones.
- Codex `mcp_tool_call`, `command_execution`, `reasoning`, `todo_list`, `file_change`, `web_search` and `error` items become persisted **activities** (upserted by item ID, text capped at 16 KiB); tool screenshots are saved as artifacts. `turn.completed` usage is stored on the task.
- `POST /tasks` without `sessionId` creates a conversation and its first turn atomically. With `sessionId`, it continues that conversation; an active turn returns **409**, without adding a user message.
- Codex defaults to `app-server` mode: a new conversation starts a thread, and later turns resume its stored ID. It supports streamed replies, mid-turn steering and approval requests. `codexMode: "exec"` selects the JSONL CLI fallback. Claude Code is also implemented; each conversation stays pinned to the runner that started it ([runners](docs/runners.md)).
- Conversation IDs, runner resume IDs, messages and task states are written atomically before acknowledgement/delivery. The file has mode **0600**. Keep it and Codex's own session directory on the office host. Run one mobd writer per state path; this snapshot store is intended for a single office daemon.
- On restart, interrupted turns become **failed** and are never rerun automatically. Completed messages and resume IDs remain available for continuation.
- Cancel returns **202** and keeps the conversation busy until the process stops and output readers finish. SIGINT/SIGTERM also cancels active turns before shutdown.
- Use a service supervisor that cleans up child processes after a hard daemon crash/SIGKILL; otherwise stop any orphaned Codex process before resuming its conversation. Snapshot recovery marks the interrupted turn failed, but cannot stop a process left by a dead daemon.
- SSE events have monotonic per-task `id` fields. `Last-Event-ID` / `?after=` resume in-process replay without duplicating messages. Raw logs are memory-only and capped at 2000 lines per task; after a daemon restart, `GET /sessions/:id` is the authoritative transcript and task snapshot.

Runner protocol details, approval behavior and the shared contract tests are documented in [runners](docs/runners.md).

CORS covers API responses and SSE, plus `OPTIONS` preflight for `GET`/`POST`/`PATCH`/`DELETE` with `Content-Type` or `Authorization`. Use the client's origin (scheme, host, and port, without a trailing slash), not the office API address. CORS does not replace authentication or LAN/VPN access controls.

## Documentation

- [Releases and installation](docs/releases.md): signing, GitHub Actions, R2, install and upgrade.
- [Architecture](docs/architecture.md), [API reference](docs/mobd-v3.md), [runners](docs/runners.md): server behavior and supported capabilities.
- [Office smoke checklist](docs/office-smoke-checklist.md), [VNC setup](docs/vnc-svnc-setup.md), [security](docs/security.md): deployment and verification.
