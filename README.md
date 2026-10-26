# mobd

Office control plane: HTTP task API + Codex, Claude Code and Pi runners. The same binary provides the desktop actuation MCP server (`mobd mcp`), launched by the agent for each task.

Desktop clients connect over HTTP/SSE and the live desktop stream (WebRTC, with a WebSocket fallback). The desktop app lives in
[xrdavies/mob](https://github.com/xrdavies/mob); this repository builds independently.

On macOS the agent drives the office Mac's own desktop. On Linux every conversation gets its
own virtual desktop (Xvfb and openbox) that sleeps when idle; see [Linux](docs/linux.md).

Source: Go 1.27+ and Xcode Command Line Tools on macOS; on Linux a C compiler, `pkg-config` and
the GStreamer development headers ([building on Linux](docs/linux-build.md)). Run `make build`,
`make check` (Go vet/tests plus Node.js 24+ distribution tests), or `make race`.
See [architecture](docs/architecture.md), [API reference](docs/mobd-v3.md)
and [runners](docs/runners.md).

Releasing: configure the GitHub `release` environment using the [release guide](docs/releases.md),
then run `make release VERSION=vX.Y.Z` with a new stable version from a clean `main` matching `origin/main`; it commits the version to `cmd/mobd/main.go`, tags that commit and pushes both
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

Install and authenticate the agent CLI you plan to use (Codex, Claude Code or Pi).
Pi is opt-in: add `"pi"` to `runners`; see [Pi setup and tests](docs/runners.md#pi).
On macOS, mobd requests Accessibility and Screen Recording when it starts if
they are missing; approve the system prompts for the app that launches mobd.
You can also grant them in System Settings → Privacy & Security. The live desktop is
streamed by mobd itself (H.264 over WebRTC); Screen Sharing is not needed. For direct LAN/VPN access,
configure `listen` for that interface; loopback is enough behind Tailscale Serve,
a gateway or an SSH tunnel. `mobd init` sets `listen` to `0.0.0.0:8080` (all IPv4 interfaces).
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

## Install / run (office Linux computer)

Ubuntu 22.04/24.04 and Debian 12/13 (amd64, arm64) are supported. The `.deb` from the
[release](https://github.com/xrdavies/mobd/releases) brings Xvfb, openbox, dbus and GStreamer:

```bash
sudo apt install ./mobd_<version>_amd64.deb
mobd init && mobd start          # a systemd user service; it enables linger
mobd desktops doctor             # programs, cgroup v2, encoder, browsers, memory, disk
```

Or the same installer as on macOS, which puts the tarball's `mobd` in `~/.local/bin` and prints
the `apt install` line for the packages above. Other distributions:
[building on Linux](docs/linux-build.md).

- **A desktop per conversation:** its first turn starts it; agents get its `DISPLAY`, bus and a
  `PATH` whose `google-chrome`, `chromium` and `firefox` use the conversation's own browser
  profile and `Downloads`. Desktops sleep in the cgroup freezer after `desktopSleepAfter` with no
  turn, viewer, CPU use or screen change, and wake for the next turn or live view.
- **Closing:** archiving or deleting a conversation, the apps' "Close desktop", or
  `mobd desktops close`. `mobd stop` asks whether to close them (`--close-desktops`,
  `--keep-desktops`); kept desktops re-attach on the next start. `mobd uninstall` (and
  `apt remove`) closes every desktop and removes the service; data is kept.
- **CLI:** `mobd desktops list | close | wake | logs | doctor | seed`. `seed SESSION` makes that
  conversation's browser profiles (log in to sites there first, then close the browser) the
  start of every new conversation's profile.
- **Memory and disk:** below `memoryFloorPercent` or `diskFloorBytes` no new desktop starts;
  turns of conversations with a desktop still run. `/health`, `GET /desktops` and SSE report it.
- **Live view:** H.264 from VA-API, NVENC or OpenH264 in software through GStreamer. Allow UDP
  `streamPort` in the firewall (`sudo ufw allow 9527/udp`) or use the WebSocket fallback.
- **Not isolated:** desktops, agents and mobd run as one Unix user; agents can read each other's
  profiles and files.

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

A plain `make build` is ad-hoc signed, so macOS treats every rebuild as a new app and drops
its Screen Recording and Accessibility grants. On a Mac you rebuild often, sign dev builds
with a self-signed code-signing certificate kept in its own keychain
(`make build SIGN_IDENTITY="mobd dev"`): grant once, and later rebuilds keep the grants.
Setup and design: [Signing dev builds](docs/dev-signing.md).

### Connect clients

Every client pairs once and then authenticates with its own persistent credential
until it is revoked, deleted or authentication is reset. There is no anonymous mode.
See [pairing](docs/pairing.md).

```bash
mobd link --url https://office.example.com  # QR + one-use link, expires in 10 minutes
mobd clients list
mobd connections
mobd clients revoke CLIENT_ID
mobd clients delete CLIENT_ID
mobd link --close                         # close unused invitations only
mobd auth reset --yes                     # invalidate all credentials; retain history
```

Commands use a private local management socket. Use the address the client actually
reaches; link generation does not test connectivity or change the listener. An HTTP URL
such as `http://192.168.1.20:8080` prints a risk warning to stderr and still generates
the link: HTTP does not encrypt invitations, credentials or data.

For separate home and office networks, use **Tailscale + Tailscale Serve** when both
devices can install Tailscale: a private HTTPS URL carries API requests, SSE and live
desktop signaling, and the video flows directly over the tailnet. Without Tailscale on
the client, use Tailscale Funnel, Cloudflare Tunnel, a self-hosted HTTPS gateway or an SSH tunnel; there
the live desktop falls back to the `/stream/ws` WebSocket. The
[connection guide](docs/connection-setup.md) covers each option step by step.
A managed relay is planned, not implemented.

### Configuration

JSON `mcpArgs`, `corsOrigins`, `codexModels`, `claudeModels`, `piModels` and `runners` are arrays of strings. `questionTimeout` is a Go duration string such as `"30m"`.

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

# live view: POST /stream with a WebRTC offer (see docs/desktop-streaming.md);
# cmd/stream-probe is a command-line viewer for testing:
go run ./cmd/stream-probe -url http://localhost:8080 -token "$MOBD_TOKEN" -seconds 10

# global live stream (session / session.deleted / task / queue / reset)
curl -H "Authorization: Bearer $MOBD_TOKEN" -N localhost:8080/events

# desktop snapshot; tool screenshots and uploads
curl -H "Authorization: Bearer $MOBD_TOKEN" -s 'localhost:8080/screen?maxWidth=720' -o screen.jpg
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/artifacts/ARTIFACT_ID -o artifact.png
```

## Configuration reference

| JSON key | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `listen` | `MOBD_LISTEN` | `:8080` | HTTP bind |
| `streamPort` | `MOBD_STREAM_PORT` | `9527` | UDP port for live desktop WebRTC on all interfaces; `0` = random per session |
| `codexBin` | `MOBD_CODEX_BIN` | `codex` | Codex CLI |
| `workdir` | `MOBD_CODEX_WORKDIR` | process cwd | Codex working directory |
| `mcpCommand` | `MOBD_MOB_MCP_COMMAND` | this `mobd` binary | MCP server command for Codex `-c` injection; empty = built-in `mobd mcp` |
| `mcpArgs` | `MOBD_MOB_MCP_ARGS` | `["mcp"]` when built-in, else `[]` | JSON array of args (external command only) |
| `sharedSecret` | `MOBD_SHARED_SECRET` | empty | Not supported; a nonempty value fails startup. Pair each client instead. |
| `controlSocket` | `MOBD_CONTROL_SOCKET` | `~/.mobd/run/control.sock` (Windows: `%LOCALAPPDATA%\mobd\run\control.sock`) | Private local management; parent directory must be owned by this user with mode 0700 |
| `statePath` | `MOBD_STATE_PATH` | macOS: `~/Library/Application Support/mob/state.json`; Linux: `~/.local/share/mob/state.json` (`$XDG_DATA_HOME`); Windows: `%APPDATA%\mob\state.json` | Private conversation snapshot |
| `maxRunning` | `MOBD_MAX_RUNNING` | `1`; `4` on Linux with a desktop per conversation | Tasks running at once across all sessions; others wait in the FIFO desktop queue |
| `dataDir` | `MOBD_DATA_DIR` | directory of `MOBD_STATE_PATH` | Root of `artifacts/` (tool screenshots, uploads) and private `auth.json` |
| `codexModels` | `MOBD_CODEX_MODELS` | empty | Comma-separated model IDs advertised on `/health` for client pickers; `MOBD_MODELS` is also accepted, with lower priority |
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
| `piBin` | `MOBD_PI_BIN` | `pi` | Pi CLI (tested with 1.4.0; requires 1.4.x or later 1.x); uses `workdir` |
| `piModels` | `MOBD_PI_MODELS` | empty | Pi model picker; prefer exact `provider/model` IDs |
| `piPermissions` | `MOBD_PI_PERMISSIONS` | `bypass` | `approve` asks before shell commands, edits, writes and other non-allowlisted tools |

Conversation desktops (Linux; accepted and validated everywhere):

| JSON key | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `desktopMode` | `MOBD_DESKTOP_MODE` | `session` on Linux, `shared` elsewhere | Linux: `session` (a desktop per conversation) or `none` |
| `desktopSleepAfter` | `MOBD_DESKTOP_SLEEP_AFTER` | `10m` | Idle time before a desktop sleeps; `0` never |
| `desktopSleepCPU` | `MOBD_DESKTOP_SLEEP_CPU` | `0.02` | CPU use (fraction of one core) under which a desktop counts as idle |
| `desktopSize` | `MOBD_DESKTOP_SIZE` | `1920x1080` | Desktop size in points |
| `desktopScale` | `MOBD_DESKTOP_SCALE` | `1` | `1` or `2` (HiDPI) |
| `desktopKeyboardLayout` | `MOBD_DESKTOP_KEYBOARD_LAYOUT` | the machine's `XKBLAYOUT`, else `us` | XKB layout of every desktop |
| `desktopDisplayBase` | `MOBD_DESKTOP_DISPLAY_BASE` | `100` | First X display number |
| `maxDesktops` | `MOBD_MAX_DESKTOPS` | `0` (no cap) | Desktops open at once; new conversations wait (`queueReason: desktops`) |
| `memoryWarnPercent` / `memoryFloorPercent` | `MOBD_MEMORY_WARN_PERCENT` / `MOBD_MEMORY_FLOOR_PERCENT` | `15` / `5` | Available memory below which mobd warns / starts no new desktop |
| `diskWarnPercent`, `diskWarnBytes` / `diskFloorBytes` | `MOBD_DISK_WARN_PERCENT`, `MOBD_DISK_WARN_BYTES` / `MOBD_DISK_FLOOR_BYTES` | `10`, `5G` / `1G` | Free space (the lower of percent and bytes) to warn / floor for new desktops |
| `desktopBrowserSeed` | `MOBD_DESKTOP_BROWSER_SEED` | `<dataDir>/seed` | Browser profiles (`chrome/`, `firefox/`) new conversations start from |

`vncAddr` and `vncViewUrl` in older configs are accepted and ignored.


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
- [Connection setup](docs/connection-setup.md), [pairing](docs/pairing.md): networks, client pairing and authentication.
- [Office smoke checklist](docs/office-smoke-checklist.md), [security](docs/security.md): deployment and verification.
- [Live desktop streaming](docs/desktop-streaming.md): capture, encoding, WebRTC and WebSocket transports, rate control and input.
- [Signing dev builds](docs/dev-signing.md): keep macOS grants across rebuilds.
- [Linux](docs/linux.md): one virtual desktop per conversation (Xvfb), sleep, memory, browsers, packaging.
- [Linux design](docs/linux-design.md): packages, interfaces, desktop state machine, API, CLI, and what was built differently.
- [Building on Linux](docs/linux-build.md): other distributions, from source.
- [Windows](docs/windows.md) (draft plan, not implemented): backend for the logged-in user's desktop, session model, packaging and phases.
