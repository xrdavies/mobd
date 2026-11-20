# Remote maintenance: config, restart and upgrade from the apps

Status: design 2026-11-20; not implemented. Issue: [#11](https://github.com/xrdavies/mobd/issues/11) (input, not
spec; see [Part 2](#part-2-decisions) for what we kept, changed and dropped).

This document has three parts: [the feature](#part-1-feature), [the decisions](#part-2-decisions) and
[the design](#part-3-design).

## Part 1: Feature

### Problem

Changing how mobd runs (enabled agents, their programs, working folders) means logging in to that computer, editing
`~/.mobd/config.json` and running `mobd restart`. Upgrading means stopping mobd, running the installer and starting
it again. For an office computer used from a phone or another Desktop, that breaks the point of Mob. Mob Desktop
upgrades this computer's mobd itself (stop → install → start); if Desktop quits midway, mobd stays stopped.

### What the feature does

**From Mob Desktop or a phone, a person can change mobd's config, restart it and upgrade it on any paired
computer, and mobd never ends up unreachable because of it.**

- Settings → a computer → Agents / Service: edit enabled agents, program paths, working folders and similar keys;
  mobd says which changes need a restart.
- "Restart to apply" and "Update to X.Y.Z" run as maintenance operations inside mobd. They wait for running tasks,
  survive the app disconnecting, and report their result when it reconnects.
- If the new config or binary does not come up, mobd goes back to the last one that worked.

### User stories

1. On my phone, I turn on Pi for the office Mac and restart it; a minute later it is back with Pi ready.
2. I change Claude's program path on the Linux box from Desktop; mobd tells me it needs a restart before applying.
3. I tap "Update to 0.6.0" on a remote computer; it waits for the running task, upgrades, and shows the new version.
4. I make a mistake in a path; the restart fails its check and mobd keeps running on the old config, telling me why.
5. An agent edited `config.json` and restarted mobd with a broken value; mobd came back on the last working config
   and the app shows what changed and what was rejected.

### Scope

In scope: a config API for a limited set of keys, controlled restart with validation and fallback, upgrade with
the installer's checks and fallback, the app screens that use them (Desktop redesign: Agents and Service tabs;
phones later).

Not in scope: any agent-facing interface for maintenance (D2), forced stops of running tasks, Windows (no service
manager yet, [#9](https://github.com/xrdavies/mobd/issues/9)), `.deb` upgrades (done with apt), changing OS
permissions or agent logins, data migrations that cannot be rolled back.

### Platforms

macOS (LaunchAgent) and Linux (systemd user unit). Windows once it has service management.

## Part 2: Decisions

### D1. People maintain mobd remotely; that is the core

- **Choice:** config changes, restart and upgrade are available to paired clients (Desktop, phones) over the
  authenticated API. The user's click, made with their device credential, is the approval.
- **Reason:** nobody should need to log in to the mobd computer to maintain it.

### D2. No interfaces for agents, and no reliance on them

- **Options:** (a) MCP tools for agents to propose config changes and request restarts or upgrades, approved by the
  user in a card; (b) nothing agent-specific.
- **Choice:** (b). #11 asked for (a); dropped.
- **Reason:** an agent saves a few clicks, but a wrong change can leave a remote computer that only someone at the
  keyboard can fix. Approval cards are approved quickly, and a config diff is hard to judge in one. The apps already
  do it in one click.
- **What we do not do either:** block agents. They run as the user and may edit `config.json` or run `mobd
  restart` like any program. No feature, flow or doc assumes they will. D5 makes such edits safe.
- Agents keep what they have today: reading status and version (e.g. to tell the user what to change).

### D3. Only some keys are editable remotely

- **Choice:** editable: `runners`, `codexBin`, `claudeBin`, `piBin`, `workdir`, `claudeWorkdir`,
  `claudeSettingSources`, `codexMode`, `maxRunning`, `questionTimeout`, `filesDir`. Local only: `listen`,
  `streamPort`, `controlSocket`, `statePath`, `dataDir`, `corsOrigins`, `mcpCommand`, `mcpArgs`, `relay`, desktop
  keys.
- **Reason:** the local-only keys can cut off the client making the change (listen, CORS, relay), move data or
  credentials, or run an arbitrary program for every turn (`mcpCommand`). #11 asked for general config read and
  write; narrowed.

### D4. Maintenance runs as an operation with an ID

- **Choice:** restart and upgrade return an `operationId`; their state is stored on disk and readable after the
  client reconnects. One operation at a time; a second request gets the running one.
- **Reason:** mobd cannot report on its own restart over a connection that the restart closes. Kept from #11.

### D5. Every restart validates first and falls back

- **Choice:** before restarting, mobd checks the candidate config with the startup checks (and an upgrade checks
  the new binary's version). After restarting, a helper outside mobd's service waits for it to answer; if it does
  not within the deadline, the helper restores the last config or binary that worked and starts it again. This
  applies however the restart was triggered: the app, `mobd restart`, or an agent.
- **Reason:** a remote computer that does not come back is the worst outcome. Kept and widened from #11.

### D6. Wait for running tasks; no force in v1

- **Choice:** new tasks wait in the queue; running ones finish; then the operation proceeds. A deadline turns the
  operation into "waiting for N tasks" for the user to retry or cancel. No forced stop.
- **Reason:** forcing is where work is lost; stopping a task is already one click in the conversation. #11's
  forced path dropped.

### D7. Upgrade checks happen in mobd, before anything stops

- **Choice:** mobd downloads to a temporary folder and checks it like `install.sh` (manifest, SHA-256, archive
  contents, macOS signature, version) while still serving. Only then does it wait for tasks and swap. Refused for
  `.deb` installs and binaries in non-writable paths, with the reason.
- **Reason:** a failed download must not stop service. Desktop's own installer stays for the first install only.

### D8. Settings stay where they are

- **Choice:** `settings.json` (models, defaults, permissions) keeps applying immediately through `PUT /settings`.
  `config.json` keys go through `/config` and a restart. No hot reload of runners in v1.
- **Reason:** hot-reloading runners and paths is a larger change; a controlled restart is fast enough once it is
  safe.

## Part 3: Design

### API (authenticated HTTP, feature `maintenance`)

| Method | Path | Body / result |
| --- | --- | --- |
| GET | `/config` | `{rev, editable: {key: value}, effective: {key: value}, overridden: [key], pendingRestart: bool, lastRejected?}`. `effective` includes environment and flag overrides; overridden keys are read-only in the apps |
| PUT | `/config` | `{rev, changes: {key: value}}` → validated with the startup checks; **200** `{rev, diff, needsRestart}`; **409** stale `rev`; **422** `{errors: {key: message}}`. Writes `config.json` atomically, keeping unrelated keys. Does not restart |
| POST | `/maintenance` | `{action: "restart"}` or `{action: "upgrade", version?}` (latest stable when omitted) → **202** `{operationId}`; **409** with the running operation |
| GET | `/maintenance/{id}` | `{id, action, state, step, version?, error?, startedAt, finishedAt?}` |
| GET | `/maintenance/latest` | the latest release's version and notes, and whether this install can upgrade itself |

`/health` adds `maintenance` (the running operation, if any). Events: `maintenance` on `/events` at each step.

### Operation steps

| Step | Restart | Upgrade |
| --- | --- | --- |
| `preparing` | Validate the candidate config | Download, verify, run `--version` on the new binary |
| `waiting` | New tasks queue; wait for running ones (deadline: 30 min, then `waiting` with the count) | Same |
| `swapping` | Save the current config as last-good | Keep the current binary as `mobd.previous`; move the new one in |
| `restarting` | Start the helper; exit non-zero so launchd (`KeepAlive`) / systemd (`Restart=on-failure`) starts mobd again | Same |
| `done` / `failed` / `rolled-back` | Recorded by the new mobd, or by the helper after a fallback | Same |

The helper is `mobd maintain --operation ID`, started with `launchctl submit` (macOS) or `systemd-run --user`
(Linux) so it is not killed with the service. It polls the control socket for health up to 60 s. On timeout it
restores last-good (config or `mobd.previous`), restarts the service, waits again, and records the result. If the
fallback also fails, the operation is `failed` with "needs attention on the computer" and the log path.

Kept across every operation: `serverId`, client credentials, conversations, files, login startup, service
arguments. Interrupted tasks are never replayed.

### Restarts not started from the apps

`mobd restart` from a terminal (or an agent) runs the same validation and helper. A config edited by hand is
compared with the last-good copy at startup; the apps show "Config changed outside Mob" with the diff, and
`lastRejected` when a change was refused.

### Apps

- **Desktop redesign:** Agents tab editable for every computer (enabled, program, settings source); Service tab
  shows "Restart to apply" when `pendingRestart`, "Update to X.Y.Z", and the running operation's step.
  Overridden keys show as read-only with the reason.
- **This computer:** Desktop drops its own stop → install → start and uses `POST /maintenance`. Its Rust installer
  remains for the first install.
- **Phones:** the same Agents and Service actions, after Desktop.

### Phases

1. `GET/PUT /config`, controlled restart with validation, helper and fallback (also for `mobd restart`).
2. Upgrade operation.
3. Apps: Desktop (redesign tabs), then iPhone and Android.

mobd ships first; apps gate on the `maintenance` feature.

### Acceptance

1. A paired client changes an editable key; `PUT /config` validates it and reports `needsRestart`; local-only keys
   are refused.
2. A restart with tasks running waits, then completes; the client reconnects and reads the same `operationId`.
3. A config that fails validation is not applied; mobd keeps running and reports why.
4. A config or binary that passes validation but does not come up is rolled back within about two minutes, and the
   operation says `rolled-back`.
5. An upgrade whose download or check fails never stops mobd.
6. After restart or upgrade, serverId, credentials, conversations and login startup are unchanged.
7. `mobd restart` from a terminal uses the same validation and fallback.
8. Tested on a macOS LaunchAgent and a Linux systemd user service.
