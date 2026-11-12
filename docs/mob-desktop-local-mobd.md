# Mob Desktop works with this computer

Status: decided 2026-11-12; not implemented yet. Work is in mob (Desktop) and mobd.

This document has three parts: [the feature](#part-1-feature) (what and why), [the decisions](#part-2-decisions)
(options considered, choice, reason) and [the design](#part-3-design) (how it works).

## Part 1: Feature

### Problem

Mob Desktop can only connect to remote machines. Most users want agents to work not only on a remote computer but
also on the one in front of them. Today that means installing mobd in a terminal, then pairing Desktop with its
own computer by hand.

### What the feature does

**Installing Mob Desktop gives this computer a working mobd.**

- Desktop opens on **This computer**: chat with local agents and manage the local mobd, without a terminal.
- The same mobd serves **phones and Desktops on other computers**: once paired, they use this computer from anywhere.
- Remote machines are added as today. One Desktop works with this computer and any number of remote machines.

### User stories

1. I install Mob Desktop and, without opening a terminal, ask Claude Code or Codex to work in a folder on this
   computer.
2. I already run mobd from a terminal; Desktop finds it and uses it without changing anything.
3. I tap **Pair a device**, scan the QR code with my phone, and later steer agents on this computer from my phone,
   including live view and control.
4. I quit Desktop; my phone can still use this computer, also after a reboot.
5. From the same Desktop I switch between this computer and my office machines.
6. When an agent or my phone moves the mouse on this computer, Desktop tells me so.
7. Desktop tells me when a newer mobd is available and upgrades it without losing anything.

### What the user sees

- **First launch:** "Set up this computer" installs and starts mobd, then opens This computer.
- **Machine list:** This computer first, then paired remote machines.
- **This computer:** chats, tasks, approvals, agent collaboration, settings. No live view, control or USB: the user
  is sitting at it.
- **Pair a device:** QR code and link for phones and other Desktops; list of paired devices with Revoke.
- **Indicator:** "Agent is using the screen" (or "Phone is controlling this computer"), naming the conversation.
- **Local settings:** service status and controls, logs, agent CLIs, defaults, working directory, permissions
  (macOS), devices, mobd upgrades.

### Scope

In scope:

- Installing the standard mobd from Desktop, or using one already installed.
- Desktop pairing itself with that mobd, automatically.
- Pairing phones and other Desktops with it.
- Managing the service, agents, defaults, working directory, permissions and upgrades from Desktop.

Not in scope:

- Live view, control and USB redirection of this computer from its own Desktop.
- A second, Desktop-private mobd next to the standard one.
- Remote access setup beyond what mobd already offers (`mobd remote`); no new relay or account system.
- Editor, project tree, terminal or Git panels; Desktop stays agent-chat focused.
- Linking this computer's mobd with remote mobds (see [cross-machine collaboration](agent-cross-machine-collaboration.md)).

### Platforms

| Platform | Support |
| --- | --- |
| macOS 13+, Apple Silicon | First version: everything in this document. |
| Linux (x86_64, arm64) | Second: install, local agents, pairing, management. Phone live view needs the `.deb` packages (installed with sudo by the user) and shows per-conversation virtual desktops, not the user's screen. |
| Windows | Later: mobd first needs an installer and service management (see [Windows](#windows)). Until then Desktop on Windows works with remote machines only. |

## Part 2: Decisions

### D1. mobd runs as the standard background service

- **Options:** (a) Desktop runs mobd as its child process; (b) Desktop installs a background service of its own;
  (c) the standard mobd background service (LaunchAgent / systemd user unit).
- **Choice:** (c).
- **Reason:** phones must reach this computer after Desktop quits and after a reboot, which rules out (a).
  (b) would mean two mobd instances per user, but the service label `com.xrdavies.mobd`, `~/.mobd` and the control
  socket are fixed and shared. One mobd per computer, as on every office machine, avoids conflicts entirely.

### D2. Use the official install, not a mobd bundled in Mob.app

- **Options:** (a) bundle mobd inside Mob.app; (b) install with the official installer.
- **Choice:** (b).
- **Reason:** with (a) the service would point into the app bundle, which a Desktop update replaces under the
  running service and moving the app or App Translocation breaks; standalone and bundled copies could coexist; macOS
  grants would need both signed identically. (b) gives one binary at a stable path (`~/.local/bin/mobd`), so grants
  survive upgrades, one install method to document, independent releases (compatibility is already gated by
  `apiVersion` and features), and a smaller app. The cost, needing internet on first setup, is acceptable: the
  agent CLIs need it too.

### D3. Desktop repeats the installer's steps in Rust

- **Options:** (a) run `curl … | sh`; (b) ship `install.sh` inside Desktop; (c) repeat its steps in Rust.
- **Choice:** (c).
- **Reason:** no shell script runs from the GUI; progress and errors come back as structured results instead of
  terminal text; the steps cannot change without a Desktop release. The release manifests (`latest.json`,
  `release.json`) become the contract shared with `install.sh`, guarded by a test in mob.

### D4. Keep the official network default

- **Options:** (a) listen on `127.0.0.1` until the first device is paired; (b) the `mobd init` default,
  `0.0.0.0:8080`.
- **Choice:** (b).
- **Reason:** a Desktop install behaves exactly like a terminal install, and pairing a phone works at once. Every
  request still needs a pairing credential.

### D5. No live view, control or USB for this computer in its own Desktop

- **Choice:** hide them for the local machine only; paired phones and other Desktops keep them.
- **Reason:** the user is at the computer. Watching or redirecting to yourself has no use. The decision is based on
  "this machine is the local one", not on `/health` features, because the same mobd still offers them to others.

### D6. Agent screen tools stay on, with an indicator

- **Options:** (a) screen tools off on the computer Desktop runs on; (b) on, as on every machine.
- **Choice:** (b), plus an indicator in Desktop while an agent uses the screen or a phone controls it.
- **Reason:** phones and agents behave the same on every machine, and users connect from their phone to have agents
  work this computer. The indicator prevents surprise when the mouse moves while the user is present.

### D7. An existing mobd is used as is

- **Choice:** if mobd is already installed, Desktop pairs with it and never changes its configuration, listen
  address or login startup, and never replaces its binary on its own. It only shows the version and offers upgrades.
- **Reason:** existing office setups (remote access, custom config) must not break because Desktop was installed.

### D8. Platform order: macOS, then Linux, Windows later

- **Reason:** macOS has the service, installer, signing and permission APIs this needs. Linux has the service and
  installer; only live view dependencies differ. Windows lacks a service manager and screen features in mobd.

### D9. The live view UDP port keeps listening on all interfaces

- **Options:** (a) bind the WebRTC UDP port to the `listen` address; (b) keep it on all interfaces.
- **Choice:** (b). Proposed in the first design, rejected during implementation.
- **Reason:** the HTTP address and the media path are independent. Behind Tailscale Serve, `listen` is
  `127.0.0.1:8080` while video still reaches the tailnet address over UDP
  ([connection setup](connection-setup.md)), and the relay's TURN traffic leaves from the same socket. Binding it to
  `listen` would push those setups to the WebSocket fallback or break relayed video. With D4, Desktop installs
  listen on all interfaces anyway.

## Part 3: Design

Desktop is a graphical installer and manager for the standard mobd. Each computer has one mobd, identical to a
terminal install.

```mermaid
flowchart TB
    subgraph LOCAL[This computer]
        DESK[Mob Desktop] -->|control socket: pairing, permissions, status| D[mobd background service]
        DESK -->|HTTP API, local credential| D
        D --> AGENT[Codex / Claude Code / Pi]
    end
    PHONE[Mob on phone] -->|after pairing: HTTP + live view| D
    OTHER[Mob Desktop on another computer] -->|after pairing: HTTP + live view| D
    DESK -->|existing pairing| REMOTE[Remote mobd]
```

| | macOS | Linux |
| --- | --- | --- |
| Binary | `~/.local/bin/mobd` | `~/.local/bin/mobd` (or `/usr/bin/mobd` from the `.deb`) |
| Service | LaunchAgent `com.xrdavies.mobd` | systemd user unit `mobd`; `mobd start` enables linger |
| Data | `~/.mobd` | `~/.mobd` |
| Control socket | `~/.mobd/run/control.sock` | `~/.mobd/run/control.sock` |
| Desktop credential | Keychain | Secret Service (encrypted file fallback, see mob's credential storage) |

### 1. Install

Desktop looks for a local mobd: the control socket answers, or `~/.local/bin/mobd` exists, or `mobd` is on PATH
(on Linux also `/usr/bin/mobd`).

- **Installed:** use it as is (D7).
- **Not installed:** show "Set up this computer". Desktop's native layer performs the steps of
  [`.github/release/install.sh`](../.github/release/install.sh) (D3), then runs `mobd init` and `mobd start --login`.

Install steps:

1. Refuse root; check the platform (macOS 13+ on Apple Silicon; Linux x86_64 / arm64).
2. If mobd is running, stop: installing is only for first installs and upgrades (section 6).
3. Download `<download base>/mobd/latest.json` for the version, then `v<version>/release.json`; check the version
   and take this platform's `sha256`.
4. Download `mobd_<version>_<platform>.tar.gz` and verify its SHA-256; the archive may contain only the regular
   files `LICENSE`, `README.md` and `mobd`.
5. macOS only: `codesign --verify --strict` against the script's requirement (identifier `com.xrdavies.mobd`, fixed
   Team ID, Developer ID). Linux releases are not signed; the SHA-256 from the HTTPS manifest is the check.
6. `mobd --version` must equal the target version; write a temporary file inside the install directory, then rename
   it to `mobd` atomically.

The download base and Team ID are fixed when Desktop is built, with the same values as the release script.

**Environment.** A GUI app does not load the shell profile: from Finder (macOS) or the app menu (Linux), PATH has
only system directories, so CLIs installed with Homebrew, npm, nvm or into `~/.local/bin` are not found. Desktop
reads PATH once from the login shell (`$SHELL -lc`, with a timeout) and runs `mobd init` / `mobd start` with it;
`mobd start` resolves the CLIs to absolute paths and writes them into the service definition. The settings page
shows the paths found and allows editing them.

**Default configuration.** The `mobd init` defaults (D4). Working directory: `~/Mob` proposed, to be confirmed during
implementation; the Desktop process's current directory is not meaningful.

**Linux desktop packages.** The tarball install runs agents without extra packages. For phone live view, Desktop
checks `mobd desktops doctor` and, if packages are missing, shows the `apt install` line (or suggests the `.deb`);
it does not run sudo.

### 2. Desktop connects to this computer

Desktop creates an invitation through the control socket (`POST /invitations`, address `http://127.0.0.1:<port>`),
redeems it with the existing `POST /pair`, and stores the credential in the system's secure storage. The user never
sees the invitation; localhost is not exempt from authentication.

- Invitations and credentials never go into logs, command-line arguments or frontend persistent storage.
- On 401 or a revoked credential, Desktop shows "Reconnect this computer" and creates a new invitation only when the
  user clicks it.
- This computer is identified by `serverId`. A machine already in the list with the same `serverId` (paired earlier
  by its LAN address) is merged into This computer.

### 3. Working on this computer

This computer is first in the machine list, selected on first launch; later launches restore the last selection.
Chats, tasks, approvals, agent collaboration and settings work as for remote machines. Live view, control and USB
are hidden (D5); Files can open the shared folder directly.

### 4. Pairing other devices

"Pair a device" shows the QR code and link, as `mobd link` does (control socket `POST /invitations`, address from
`mobd remote` or the LAN address). The device list uses the control socket's `/clients` to list and revoke.

### 5. Screen use indicator

mobd publishes a `screenUse` event on `/events` when someone starts acting on a desktop and again when they stop;
`/health` lists the uses in progress (`screenUse`) for a client that connects mid-way. The feature is advertised as
`screenUse`.

- **Agent:** every desktop tool call (`screenshot`, `click`, `type_text`, `key`, `list_windows`, `open_app`) goes
  through mobd (`/internal/tasks/{id}/desktop/{op}`) on every platform, so mobd sees it directly.
  `{source: "agent", sessionId, taskId}`.
- **Control:** each pointer, key, text or scroll message from a live view client; view-only watching does not count.
  `{source: "control", sessionId, clientId}`.
- **End:** 15 s after the last action (agents pause between tool calls while they think, so a shorter wait would
  make the indicator flicker). Event: the same object with `active: false`.

Desktop shows the indicator for this computer while any use is active, naming the conversation.

- **macOS:** the tools act on the user's real screen, so the indicator is important.
- **Linux:** agents and live view use per-conversation virtual desktops, not the user's screen, so the indicator is
  informational; it uses the same events.

### 6. Management and upgrades

| Area | Capabilities | Implementation |
| --- | --- | --- |
| Service | Status, version, start, stop, restart, logs, login startup | `mobd start/stop/restart/logs`; status from the control socket `GET /health` |
| Agents | Detected CLI paths, availability, enabled runners | `/health` runners and checks; paths into `config.json`, then restart |
| Defaults | Default agent, model, reasoning effort, permission options | Existing `GET/PUT /settings` |
| Working directory | View and change `workdir` / `claudeWorkdir` | `config.json` + restart |
| System permissions | macOS only: Accessibility, Screen Recording and others | Control socket `GET /permissions`, `POST /permissions/{name}`; Screen Recording needs a mobd restart after granting. Hidden on Linux. |
| Desktop packages | Linux only: missing live view packages | `mobd desktops doctor`; shows the install command |
| Devices | Paired devices, revoke, pair a new device | Control socket `/clients`, `/invitations` |

Edits to `config.json` keep unrelated fields; changes that need a restart are marked, and Desktop names the running
tasks a restart affects. Quitting Desktop does not stop mobd.

**Upgrades.** Desktop checks `latest.json` and offers new versions. When no tasks are running: `mobd stop` → install
steps → `mobd start` (with `--login` if it was on). Configuration, conversations, attachments and paired devices are
kept; on macOS the path and signing identifier are unchanged, so permissions survive. A mobd installed from the
`.deb` is upgraded with `apt`, so Desktop shows the command instead. Desktop and mobd release independently;
`apiVersion` and features decide compatibility, and Desktop asks for an upgrade when mobd is too old.

### Windows

Not in the first versions. Before Desktop can set up this computer on Windows, mobd needs:

- a signed installer (today: an unsigned portable ZIP);
- service management (`start/stop/restart/status/logs` are unsupported; planned as a logon task);
- `install.sh`-equivalent release manifests for Windows;
- verified launching of npm `.cmd` agent shims.

Screenshots, input and live view are not implemented on Windows either; agents would work without screen tools,
and the indicator would not apply. Until then, Desktop on Windows shows remote machines only.

### Changes by repository

**mobd**

- Control socket `GET /health` adds `serverId`, `version`, `executable` and `loginStartup` (absent where the system
  has no service management), so Desktop can recognise an installed instance, match machines and show service
  status.
- `screenUse` events and `/health` field (section 5).
- Keep the terminal walkthrough in `mobd permissions`; Desktop uses the same capability through the control socket.

**mob (Desktop)**

- Rust installer and the manifest format test; reading the login shell PATH.
- "Set up this computer" flow, automatic local pairing, This computer merged by `serverId`.
- Hide live view, control and USB for this computer; the indicator.
- Local settings page and "Pair a device".
- mobd upgrade prompt and flow (`apt` hint for the `.deb`).

### Acceptance

1. A Mac without mobd: open Desktop, finish "Set up this computer", and chat with a local agent without a terminal.
2. mobd already installed from a terminal: Desktop uses it, without changing its configuration, reinstalling, or
   showing this computer twice.
3. The installer rejects a wrong checksum, wrong signature (macOS), unexpected archive contents or wrong version; it
   never replaces the binary while mobd is running.
4. Agent CLIs installed with Homebrew / npm are found when Desktop is launched from Finder or the app menu.
5. This computer shows no live view, control or USB; a paired phone can view and control it.
6. Desktop shows the indicator while an agent or a phone operates this computer's screen.
7. After quitting Desktop the phone can still use this computer; likewise after a reboot.
8. After an upgrade, configuration, conversations, paired devices and macOS permissions are kept; no upgrade while
   tasks are running.
9. After the local credential is revoked, Desktop shows "Reconnect this computer" and does not re-pair on its own.
10. Adding and using remote machines is unaffected.
11. Linux: setup and local agents work from the tarball install; missing live view packages are reported with the
    install command.
