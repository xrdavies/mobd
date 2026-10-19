# Linux (draft plan, not implemented)

> **Status: draft for review.** Nothing here is implemented. Today only macOS has a desktop
> backend; Linux compiles a placeholder that returns `ErrUnsupported`
> (`internal/desktop/backend_linux.go`) and the API answers 501 for `/screen` and `/stream`.
> Items marked *(to verify)* are behaviour we have not confirmed on real machines.
> The Windows plan is separate: [Windows](windows.md).

On macOS mobd drives the one real desktop of the logged-in user, shared by every task and
serialized by the FIFO desktop queue (`maxRunning`, default 1, `internal/api/server.go`
`scheduleLocked`). On Linux mobd instead gives **each conversation its own virtual X display**.
Desktops do not compete, so conversations run side by side, mobd needs no graphical login, no
permission prompts, and the same setup works on a desktop PC, a headless server or a VM.

There is **no Wayland and no xdg-desktop-portal** in this plan. The physical screen of the Linux
machine is not watched or controlled (see [Not supported in v1](#not-supported-in-v1-and-later-options)).

## Summary

| | Linux |
| --- | --- |
| Desktop model | one virtual X display per conversation (session), created on demand, closed when idle |
| Display server | Xvfb, one per desktop, plus openbox and a private D-Bus session bus |
| Capture | XShm + XDamage on that display (pure Go, [jezek/xgb](https://github.com/jezek/xgb)) |
| Encode | GStreamer: VA-API, then NVENC, then OpenH264 (not bundled); never x264 |
| Input | XTest on our own display: no permission prompt |
| Permissions | none |
| Process model | `systemd --user` unit with linger (default) or a system unit with a dedicated user |
| Concurrency | one running turn per conversation, `maxRunning` turns overall, `maxDesktops` live desktops |
| Packages | `.deb` (Ubuntu 22.04/24.04), tarball; `.rpm` later |
| Tests | real end-to-end desktop and stream tests on hosted Ubuntu CI runners |

## Desktop model

- **One desktop per conversation.** A desktop is created when a turn of that session is
  dispatched (see [Concurrency](#concurrency)) and kept across turns, so the agent's browser,
  windows and half-finished forms are still there on the next turn.
- **Idle close:** a desktop with no running turn and no live-view viewer closes after
  `desktopIdleTimeout` (proposed default 60 min). It also closes when its session is archived
  (`PATCH /sessions/{id}` with `archived: true`) or deleted (`DELETE /sessions/{id}`), and is
  recreated on demand by the next turn. Closing loses open windows, not files: profiles and
  downloads live in the session's directory (see [Agent environment](#agent-environment)).
- **Why per conversation, not per task:** a conversation is the unit the user continues ("now
  submit the form"), and its turns already run one at a time (a session with an active turn
  rejects a new one with `ErrSessionBusy`, `internal/task/store.go`). A per-task desktop would
  close the browser at the end of every turn, and the follow-up turn would start from an empty
  screen and a logged-out browser. A per-conversation desktop gives exactly one actor per
  display, so no locking is needed inside a desktop.
- **Why start at dispatch, not at the first desktop tool call:** runners get `DISPLAY` when they
  start, and agents often launch apps from their shell (`google-chrome &`) before any MCP call.
  Starting Xvfb + openbox + dbus costs well under a second and some 30–50 MB, so mobd starts the
  desktop with the turn. A per-turn "no desktop" option (`taskOptions`) can come later.

## Stack per desktop

### Display server: Xvfb

| Option | For | Against |
| --- | --- | --- |
| **Xvfb** (chosen) | in every distro, no GPU, driver or config file, small; has MIT-SHM, DAMAGE, XTEST, XFIXES, RANDR; starts in ~100 ms | size fixed at start; software GL only (Mesa llvmpipe) |
| Xvnc (TigerVNC) | live resize through RandR | carries a VNC server we do not use; one more dependency |
| Xorg + dummy driver | real Xorg, xrandr modes | needs a generated `xorg.conf` per display, heavier, no real GPU benefit with the dummy driver |
| headless Wayland (sway/weston `--headless`) | modern stack | capture and input need compositor-specific protocols (wlr-screencopy, virtual-pointer), no portable window list; out of scope by decision |

Each desktop runs
`Xvfb :N -screen 0 <W>x<H>x24 -dpi <96·scale> -nolisten tcp -auth <xauth> -s 0 +extension RANDR`
(`-s 0` disables the screen saver, so a desktop is never "asleep" or "locked").
Size is `desktopSize` (default `1920x1080`) and `desktopScale` (default `1`; `2` sets the DPI,
`Xft.dpi` and `GDK_SCALE`, and `Display.Scale = 2`, so agent coordinates stay in points like on
macOS). Changing the size means a new desktop; live resize would need Xvnc *(only if needed)*.
The live view's `resize` keeps working: it scales the encoded picture, not the display.

### Window manager: openbox

Without a window manager, X windows have no focus handling, no EWMH client list and no
maximize/activate, so `list_windows` and "click to focus" break. **openbox**: small (~10 MB),
fully EWMH compliant (`_NET_CLIENT_LIST`, `_NET_ACTIVE_WINDOW`, `_NET_WM_STATE`), stacking
(overlapping windows like macOS/Windows, which agents expect), configured by one XML file.
mobd writes its own `rc.xml` per desktop: no root menu, no desktops/pager, smart placement,
title bars kept (agents use them to read window titles). Tiling WMs (i3) rearrange windows
unexpectedly; xfwm4/metacity pull in more of a desktop environment. No panel or desktop shell.

### Session bus, notifications, clipboard

- **D-Bus:** one `dbus-daemon --session --nofork --address=unix:path=<rundir>/bus` per desktop.
  Browsers, GTK and Electron apps expect a session bus; sharing one across desktops would mix
  single-instance apps between displays.
- **Notifications (optional):** mobd can own `org.freedesktop.Notifications` on each bus
  (godbus) so apps do not block on a missing daemon, and log them as activity. Without it,
  most apps just skip notifications.
- **Clipboard:** X selections are per display, so each desktop has its own clipboard. `Paste`
  owns `CLIPBOARD` on that display through xgb (answering `SelectionRequest`), then presses
  Ctrl+V. Terminals need Ctrl+Shift+V, an accepted limitation; agents can fall back to `key`.
  No clipboard sync with clients (as on macOS).

### Capture

- **Pure Go through xgb:** `XShmGetImage` of the root window into a shared segment, with XDamage
  telling which regions changed. An unchanged screen produces no frames, which keeps the stream
  contract (`internal/desktop/backend.go` `StreamOptions`). Snapshots (`/screen`, agent
  screenshots) use the same path and the existing `desktop.Encode`; no GStreamer involved.
- GStreamer `ximagesrc display-name=:N use-damage=true` is a quick prototype path but emits
  frames at the configured rate even when nothing changes *(to verify)*, so it is not the target.
- Xvfb's `-shmem` / `-fbdir` framebuffer export could avoid even the `XShmGetImage` copy; a later
  optimisation *(to verify)*.

### Encode

- **GStreamer in a thin C shim** (`stream_linux.c`): `appsrc` fed with damaged frames, then
  `vapostproc ! vah264enc` (VA-API, Intel/AMD), `nvh264enc` (NVIDIA), or `openh264enc`, then
  `h264parse config-interval=-1 ! appsink` (Annex B, SPS/PPS on every IDR). Settings: constrained
  baseline (the stream service signals `profile-level-id=42e01f`, `internal/stream/stream.go`),
  no B-frames, CBR/low latency, keyframes on request only, live `bitrate`, `GstForceKeyUnit` for
  `Keyframe()`, a `videoscale` capsfilter change for `SetSize`, `videorate` for `SetFrameRate`.
  Probe at start which encoder actually encodes a frame and report it in `/health`.
- VA-API and NVENC encode through the render node (`/dev/dri/renderD*`) or the NVIDIA driver,
  not the display, so they work with Xvfb and on headless machines. The mobd user needs the
  `render` group for VA-API.
- **x264 is never used** (GPL); the pipeline excludes `x264enc`. FFmpeg libs are avoided for the
  same reason (distro builds are often `--enable-gpl`).
- **OpenH264:** Cisco covers the H.264 patent royalties only for its own binary downloaded from
  Cisco, so we do not bundle `libopenh264`. Rely on the distro package or download Cisco's binary
  at install time like Firefox and Fedora do *(legal check needed)*. Servers and VMs usually
  have no GPU, so here software encoding is the common case, not a rare fallback. Encoding runs
  only while someone watches; an unwatched desktop costs no encoder time.
- Ubuntu 22.04 ships GStreamer 1.20 and may need `vaapih264enc` (gstreamer1.0-vaapi) instead of
  `vah264enc` *(to verify)*; 24.04 ships 1.24.

### Input

- **XTest** (`FakeMotion`, `FakeButton`, `FakeKey`) through xgb. No prompt: mobd owns the display.
- **Keyboard layout:** mobd sets a fixed XKB layout on each desktop (`us` by default,
  `desktopKeyboardLayout`). Live-view `key` messages map `KeyboardEvent.code` to evdev codes
  (X keycode = evdev + 8) and stay physical, which is what `code` means. Agent `key("a")` resolves
  the keysym through the known keymap, so letters are right whatever the layout.
- **Unicode text** (live-view `text`, agent typing): for keysyms missing from the keymap, map a
  spare keycode with `XChangeKeyboardMapping`, press, restore (as xdotool does). CJK and emoji
  work this way because the keymap is ours.
- **Cursor:** position from `QueryPointer` for the `cursor` message. XFixes `GetCursorImage`
  also gives the cursor *shape*, which macOS cannot; sending it to clients is a later option.
- **Click(count):** presses within the double-click time, as on macOS.

### Windows and apps

- **ListWindows:** EWMH `_NET_CLIENT_LIST_STACKING`, `_NET_WM_NAME`, `_NET_WM_PID`, `WM_CLASS` as
  `App`, frame geometry, `_NET_WM_STATE_HIDDEN` for on-screen. Works fully on X11, unlike Wayland.
- **OpenApp:** match the target against desktop entries (`XDG_DATA_DIRS`,
  `~/.local/share/applications`; desktop ID or `Name=`, case-insensitive), else an executable on
  `PATH`. mobd serve launches it itself with the desktop's environment, in the **desktop's**
  process group, not the turn's: the runner's group is killed when a turn is cancelled
  (`internal/runner/cli/cli.go`), and apps opened through `open_app` should survive it. Apps the
  agent starts from its shell must detach (`setsid`, output redirected) to outlive a cancelled
  turn; the MCP instructions say so. The MCP argument `name_or_bundle_id` keeps its name; its
  description says "desktop file ID" on Linux.

### Display state

A virtual desktop never sleeps or locks: `State()` is always awake and unlocked, `KeepAwake` is a
no-op, and the macOS lock-screen rules do not apply.

## Process model

mobd no longer needs a graphical login session. It runs as a systemd unit; today
`internal/service/service.go` fails unless `GOOS == darwin`, so a systemd `Manager` is new work
(phase 0).

- **Default: `systemd --user` unit with linger.** `mobd start` writes
  `~/.config/systemd/user/mobd.service` (`ExecStart=<exe> serve --config ~/.mobd/service.json`,
  `Restart=on-failure`, captured `PATH`) and enables linger (`loginctl enable-linger`), so mobd
  starts at boot without anyone logging in. Agents run as the user, with their `codex`/`claude`
  logins, git and SSH keys. `--login` = `systemctl --user enable`; `stop` = stop + disable;
  `logs` = `journalctl --user -u mobd -f`. Keep capturing `PATH` and absolute `codex`/`claude`
  paths at `start`, as `service.go` does (nvm and `~/.npm-global/bin` are missing from systemd).
- **Option: system unit** for dedicated servers: `/etc/systemd/system/mobd.service` with
  `User=mobd`, home `/var/lib/mobd`, shipped by the `.deb` but disabled. The runners must be logged
  in as that user.
- **Desktop supervision:** each desktop is one supervised process group started by mobd serve:
  Xvfb, then dbus-daemon, then openbox (each waits for the previous one: Xvfb readiness via
  `-displayfd` or by connecting). Children get `Pdeathsig=SIGKILL` so a crashed mobd does not
  leave displays behind.
  - **Xvfb or dbus-daemon exits:** the desktop is dead. The running turn's desktop calls fail with
    a clear error, live-view sessions get `ended` with the reason, an SSE `desktop` event says
    `stopped`, and the next turn creates a new one. Three crashes within a few minutes mark the
    session's desktop failed until the next turn *(threshold to tune)*.
  - **openbox exits:** restarted in place; X clients survive a WM restart.
- **Display numbers:** allocated from `desktopDisplayBase` (default 100) upward, skipping numbers
  with a live `/tmp/.X<N>-lock`, so mobd displays are easy to recognize and never collide with a
  real `:0`. Each desktop gets a random MIT-MAGIC-COOKIE in its own Xauthority file: Xvfb also
  listens on the abstract socket `@/tmp/.X11-unix/X<N>`, so without a cookie any local user could
  connect.
- **Stale cleanup at start:** mobd records each desktop (`display`, pids, paths) in
  `<dataDir>/desktops/<sessionId>/desktop.json`. At startup it kills recorded pids whose
  `/proc/<pid>/cmdline` still matches, removes their lock and socket files and runtime
  directories, and touches nothing it did not record.
- **Runtime directory:** `$XDG_RUNTIME_DIR/mobd/d<N>` (0700; bus socket, Xauthority), or
  `<dataDir>/run/d<N>` when `XDG_RUNTIME_DIR` is unset (system unit). Paths stay short for the
  108-byte `sun_path`.

## Concurrency

Desktops no longer compete, so the global "one desktop, FIFO" rule becomes:

- **One running turn per conversation:** already enforced (`ErrSessionBusy`).
- **`maxRunning`** keeps its name and meaning, "turns running at once across all sessions", but
  now limits CPU, memory and API use, not desktop access. Its default stays 1 on macOS and becomes
  `maxDesktops` on Linux when session desktops are on. `/health` keeps reporting it.
- **`maxDesktops`** (new, `MOBD_MAX_DESKTOPS`, proposed default 4): cap on live virtual desktops.
- **`desktopIdleTimeout`** (new, default `60m`), **`desktopSize`**, **`desktopScale`**,
  **`desktopKeyboardLayout`**, **`desktopDisplayBase`**: as above.

When a turn is dispatched (`scheduleLocked`) and its session has no live desktop:

1. under `maxDesktops`: start one;
2. at the cap: **evict the least recently used idle desktop** (no running turn, no viewer) and
   start one; the evicted session's next turn recreates its desktop (profile kept, windows lost);
3. every desktop busy: the turn stays queued ("waiting for a desktop", `queuePosition` as today).
   Turns whose session already has a desktop may pass it, so one waiting turn does not block
   the queue.

Sizing notes for the docs:

- **Memory:** Xvfb at 1080p is ~10–20 MB plus the framebuffer (8 MB), openbox and dbus a few MB
  each. The browser dominates: 0.5–1.5 GB per desktop with a few tabs. 4 desktops fit a 16 GB
  machine comfortably.
- **Encoders:** each viewer is one encode. NVIDIA consumer GPUs limit concurrent NVENC sessions
  (a driver limit of a few sessions *(to verify the current number)*); VA-API has no hard limit but
  finite throughput; OpenH264 costs roughly one CPU core per 1080p30 stream *(to measure)*. When a
  hardware encoder refuses a new session, mobd falls back to the next encoder for that stream.
  Sharing one encode among viewers of the same desktop and size is a later optimisation.

## Agent environment

- **Runner env:** today codex and claude get `os.Environ()` (`internal/runner/codex/runner.go`,
  `internal/runner/claude/runner.go`) and `mobd mcp` gets `MOB_API`, `MOB_TASK_ID`,
  `MOB_TASK_TOKEN` through `MCPEnv` (`internal/api/media.go` `runOptions`). Runners launched for a
  session additionally get `DISPLAY`, `XAUTHORITY`, `DBUS_SESSION_BUS_ADDRESS` and
  `XDG_RUNTIME_DIR` of that session's desktop, and a per-session `bin` directory first on `PATH`
  (browser wrappers, below). This needs a `runner.Options.Env` field.
- **Desktop calls are forwarded.** `mobd mcp` is its own process and calls `internal/desktop`
  directly (`hostDesktop`, `internal/mcpserver/server.go`). On Linux it uses a `proxyBackend` that
  sends `Screenshot/Click/Key/TypeText/ListWindows/OpenApp` to
  `POST /internal/tasks/{id}/desktop/{op}` on mobd serve, authenticated with the task token like
  `ask` does (`internal/mcpserver/ask.go`, route next to `/internal/tasks/{id}/ask` in
  `internal/api/server.go`). mobd serve maps the task to its session and routes the call to that
  session's desktop. Tokens exist only while a turn is dispatched (`runTask` deletes them), so a
  finished turn cannot drive a desktop.
- **Browser profiles:** apps start with no logged-in state, and Chrome and Firefox lock a profile
  to one running instance. Worse, launching Chrome with a profile already open on another display
  opens the window in the *existing* process, on the other desktop. So **every session gets its
  own profiles** under `<dataDir>/desktops/<sessionId>/profiles/`:
  - per-session wrappers for `google-chrome`, `chromium` and `firefox` on `PATH` add
    `--user-data-dir=…` / `--profile … --no-remote`; `OpenApp` uses the same wrappers. Tools that
    manage their own browser (Playwright, Chrome DevTools MCP) are unaffected.
  - **seed profile (optional):** `desktopBrowserSeed` points at a prepared profile that is copied
    (without lock and cache files) when a session's profile is created, so sites the user logged
    into once are logged in for every new conversation. There is no keyring on a virtual desktop,
    so the wrappers pass `--password-store=basic`; a seed must be created the same way or Chrome
    cannot decrypt its cookies. The simplest way to make one: a `mobd desktop seed` command opens
    a desktop for a "seed" session that the user prepares through the live view.
  - profiles survive idle close and eviction and are removed with the session.
- **HOME and XDG dirs:** **shared by default.** A per-session `HOME` or `XDG_CONFIG_HOME` would
  isolate apps but would also hide the runners' and tools' logins (`gh` keeps its token in
  `~/.config/gh`). `desktopIsolateConfig: true` can later give each session its own
  `XDG_CONFIG_HOME`/`XDG_CACHE_HOME`/`XDG_STATE_HOME` seeded with chosen shared entries.
- **Downloads:** the wrappers point browser downloads at `<sessionDir>/Downloads`
  *(mechanism to verify per browser)*, so files stay with the conversation.
- **Isolation:** all desktops run as the same Unix user. Each desktop has its own X cookie, bus
  and clipboard, so they do not interfere by accident, but an agent can read another session's
  profile or Xauthority file. This is not a security boundary (on macOS all tasks share one
  desktop today). Later option: a pool of per-session Unix users, which needs root to start
  runners.
- **Cleanup:** deleting a session removes `<dataDir>/desktops/<sessionId>` (profiles, downloads,
  state); archiving closes the desktop and keeps the files. `/tmp` stays shared.

## API and clients

Gated by a new `/health` feature **`sessionDesktops`**; without it (macOS, Windows) clients behave
as today.

- **`POST /stream`** body gains `"sessionId"`. With `sessionDesktops` it is required: 404 for an
  unknown session, 409 "the conversation has no desktop yet" when none is live. A viewer counts as
  activity, so a watched desktop is never closed for idleness. When the desktop stops, the
  session gets `ended` with the reason. The stream manager today takes one global
  `Start`/`Display`/`NewInput` (`internal/api/server.go` `New`); these become per-desktop.
- **`GET /screen?sessionId=…`**: same rule.
- **`GET /desktops`**: `[{sessionId, display, width, height, scale, state, startedAt, lastUsedAt,
  running, viewers}]`. `DELETE /desktops/{sessionId}` closes one. Optional
  `POST /desktops/{sessionId}` starts one without a turn, so a user can log in somewhere before the
  agent runs.
- **SSE:** `GET /events` gains `desktop` events `{sessionId, state: starting|running|stopped,
  reason}`.
- **`/health`:** adds `desktops: {count, max}` and `maxDesktops`. The macOS permission checks are
  generalized to backend checks: on Linux `xvfb`, `windowManager`, `dbus`, `encoder` (detail e.g.
  "vah264enc (VA-API)" or "openh264enc (software)"), and `fonts` (a warning; see
  [Packaging](#networking-packaging-testing)). `accessibility` and `screenRecording`
  (`internal/api/health.go` `permissionChecks`) are reported `ok` with detail "not needed on Linux"
  so API v3 clients show no false warning; `display` reports the desktop cap instead of sleep/lock.
- **Clients:** with `sessionDesktops`, the live view follows the open conversation: opening
  another conversation re-POSTs `/stream` (one session per client already replaces the old one).
  Before the first turn it shows "no desktop yet; it starts with the first turn". The desktop list
  can show which conversations have a live desktop.
- **macOS unchanged:** one desktop, `sessionId` accepted and ignored, no `sessionDesktops`
  feature, FIFO desktop queue with `maxRunning` 1.
- **MCP text:** instructions and tool descriptions stop saying "office Mac", "Quartz",
  "pasteboard + Cmd+V" and "bundle id" off macOS (`internal/mcpserver/server.go`); on Linux they
  tell agents to use `ctrl+c`/`ctrl+v` and that the desktop is theirs for the conversation.

## Not supported in v1 and later options

- **The physical screen of the Linux machine** is neither watched nor controlled. On Wayland this
  is impossible without a portal dialog or private compositor APIs, which is why this plan avoids
  it entirely.
- **Later, opt-in: attach to an existing X11 display** (`desktopMode: "attach"`, with `DISPLAY` and
  `XAUTHORITY` of a logged-in X11 session). One shared desktop, the macOS FIFO queue and
  `maxRunning` 1 come back. Same capture/input code.
- **GPU inside Xvfb:** none. GL is Mesa llvmpipe; browsers fall back to software compositing and
  SwiftShader WebGL. Pages work but use more CPU; WebGL-heavy sites and video are slow; WebGPU is
  off. Later options: Xorg with a real GPU driver headless, or VirtualGL.
- **Audio:** none. Apps find no sound server and stay silent. A PipeWire/PulseAudio null sink per
  desktop could come later.

## Networking, packaging, testing

- **Networking:** TCP listen port and UDP `streamPort` 9527 as on macOS. The `.deb` does not touch
  the firewall; docs give `ufw allow 9527/udp` and
  `firewall-cmd --add-port=9527/udp --permanent`. Tailscale works the same (`tailscale serve` /
  Funnel for the WebSocket path, UDP over the tailnet). On a server without a desktop, host-only
  ICE candidates still cover LAN and tailnet addresses.
- **Packages:** keep the R2 layout and add `linux-x86_64` / `linux-aarch64` keys to `release.json`.
  - `mobd_<v>_linux_amd64.tar.gz` / `_arm64`: `mobd`, LICENSE, README, unit templates; SHA256SUMS.
  - `mobd_<v>_amd64.deb` / `_arm64.deb`: `/usr/bin/mobd`, the disabled system unit. **Depends:**
    `xvfb`, `openbox`, `dbus-x11` (or `dbus-daemon`), `xkb-data`, `gstreamer1.0-plugins-base`,
    `gstreamer1.0-plugins-bad` (va, nvcodec, openh264 elements). **Recommends:** `fonts-dejavu`,
    `fonts-noto-cjk` (a bare Xvfb machine has almost no fonts, so pages render boxes),
    `xdg-utils`, `gstreamer1.0-vaapi` on 22.04. **Suggests:** a browser.
  - Built with cgo (GStreamer shim) on `ubuntu-22.04` for the glibc floor (2.35) and
    `ubuntu-24.04-arm`. GStreamer is linked dynamically, never bundled. AppImage is not planned.
- **Install:** `install.sh` gains a Linux branch (today it exits on anything but Darwin arm64):
  detect `uname -s/-m`, download the tarball, check SHA256 from `release.json`, install into
  `~/.local/bin`, print the `apt install` line for the dependencies, and offer `mobd start`.
- **CI can run real end-to-end desktop tests.** Xvfb needs no GPU, no display and no login, so a
  hosted `ubuntu-24.04` runner gets exactly the environment production uses. This is a big change
  from macOS, where desktop tests need a logged-in Mac with TCC grants
  (`internal/stream/desktop_darwin_test.go` cannot run on hosted runners). A CI job installs
  `xvfb openbox` and an OpenH264 build (CI does not ship it, so the licensing question does not
  arise there), starts `mobd serve` with a fake runner and checks, on every pull request:
  - a turn creates a desktop, `xterm` opens through `open_app`, and click, key, Unicode text,
    paste and `list_windows` through the MCP proxy change what a snapshot shows;
  - `cmd/stream-probe` receives and decodes a stream for the session, over WebRTC and with `-ws`,
    with `-pli`, `-click` and `-type`;
  - two sessions run at once on separate displays; `maxDesktops` eviction and queueing; idle close
    with a short timeout; archive and delete; killing Xvfb mid-turn; stale cleanup after a
    `kill -9` of mobd.

  The mob clients can also be tested end to end against a Linux mobd in CI. Hardware encoders
  (VA-API, NVENC) still need manual or self-hosted runs.
- **Manual matrix** (extend `docs/office-smoke-checklist.md`): Ubuntu 24.04 and 22.04, desktop PC
  with Intel VA-API, NVIDIA NVENC, a GPU-less VM; several sessions watched at once; Chrome and
  Firefox with a seed profile; HiDPI scale 2; `stream-probe` first-picture time, bitrate and fps
  against the macOS baseline.

## Portable core (phase 0, shared with Windows)

These macOS-only assumptions block both Linux and [Windows](windows.md); each is cited so the work
list is concrete.

- **Build:** `GOOS=linux CGO_ENABLED=0 go build ./...` succeeds today. `GOOS=windows` fails in
  `internal/pairing/store.go` (`syscall.Flock`) and `internal/runner/cli/cli.go` (`Setpgid`,
  `syscall.Kill`); `internal/control/control.go` (`Flock`, `Stat_t` ownership check) and
  `internal/service` follow. Work: an `internal/filelock` package (flock / `LockFileEx`),
  `control_unix.go`/`control_windows.go`, `proc_unix.go`/`proc_windows.go` (Job Objects).
- **Service:** one `Manager` interface with launchd (today), systemd (this plan) and Scheduled Task
  ([Windows](windows.md)) implementations, sharing the preflight checks and `service.json`.
- **Paths:** one `config.Dir()` per OS (`~/.mobd` on macOS and Linux).
- **Desktop per session:** `desktop.Current()` is one process-wide backend today
  (`internal/api/server.go` `New`). A `desktop.Provider` returns the backend for a session: macOS
  and Windows return the single one, Linux the session's desktop.
- **Desktop proxy:** `proxyBackend` for `mobd mcp` and the `/internal/tasks/{id}/desktop/{op}`
  routes (above). Required on Linux, used on Windows too so both behave the same; macOS keeps
  direct calls when `MOB_API` is unset *(to decide)*.
- **Shared stream code** in `internal/desktop`: an Annex B normalizer (AUD first, SPS/PPS before
  IDR, `Key` set) and the delivery and drop logic now in `stream_darwin.go` (16-unit buffer, drop
  deltas until the next keyframe), pure Go, tested with the `h264_test.go` helpers.
- **Keys:** per-OS tables keyed by `KeyboardEvent.code` (`internal/desktop/keys.go` holds macOS
  key codes today); `command` means Super on Linux and Win on Windows, `option` means Alt; add
  `super`/`win` aliases.
- **Errors and health:** `ErrUnsupported` says "only supported on macOS" and `unavailable()`
  mentions the "office Mac" (`internal/desktop/desktop.go`, `screen.go`); `/health` hard-codes
  macOS TCC checks. Both become per-backend.

## Phases

Rough effort assumes one engineer familiar with the codebase.

| Phase | Scope | Effort |
| --- | --- | --- |
| **0. Portable core** | the list above. Result: mobd runs runners on Linux and Windows (no desktop), and `GOOS=windows` builds. | 1.5–2 weeks |
| **1. Desktop manager** | Xvfb/openbox/dbus supervision, display numbers, Xauthority, stale cleanup, idle close, archive/delete, runner env, `maxDesktops` and eviction, `/desktops`, SSE events, health checks, systemd unit. | 1.5–2 weeks |
| **2. X11 backend** | xgb capture and snapshots, XTest input, key tables, Unicode, clipboard, EWMH windows, `OpenApp`, MCP text, CI desktop tests. | 1.5 weeks |
| **3. Streaming** | GStreamer shim, encoder probe and fallback, per-session `POST /stream` and `/screen`, stream-probe in CI. | 1.5–2 weeks |
| **4. Agent environment and release** | browser wrappers, per-session profiles, seed profile, downloads, `.deb` and tarball, `install.sh`, CI release matrix, docs. | 1–1.5 weeks |
| **5. Later** | cursor shape, shared encodes, attach mode, per-session users, GPU, audio, `.rpm`. | as needed |

Linux first remains the recommendation: phase 0 is mostly Unix code that already works, there are
no permission prompts or code-signing costs, and every desktop path is testable in CI.

## Risks

1. **Memory and CPU:** browsers on software rendering, several at once, and software encoding on
   GPU-less machines. Mitigated by `maxDesktops`, idle close, and encoding only while watched.
2. **Encoder variability:** VA-API driver differences (Intel iHD vs i965, Mesa AMD), NVENC session
   limits. Probe at start, fall back per stream.
3. **OpenH264 licensing:** cannot be bundled; on GPU-less servers it is the only encoder.
4. **Apps that assume a full desktop session:** no keyring, no portal, no notification daemon, no
   GPU. Some apps may warn or misbehave *(to collect while testing)*.
5. **Lost windows:** idle close and eviction lose open windows and unsaved state; only profiles and
   files survive.
6. **Same-user isolation:** sessions can read each other's files and profiles.

## Open questions for the owner

1. Browser profiles: per-session by default (proposed) or one shared profile? Is a seed profile
   enough for "logged in everywhere"?
2. `desktopIdleTimeout` default: 60 min (proposed), shorter to save memory, or longer so a user can
   come back the next day?
3. `maxDesktops` default 4, and at the cap: evict the least recently used idle desktop (proposed)
   or always queue?
4. Should every turn get a desktop, or should clients be able to mark a turn "no desktop"?
5. Should mobd ever support the physical desktop (attach to an X11 display; Wayland is out)?
6. Service default: `systemd --user` with linger (proposed) or a system unit with a dedicated user?
7. OpenH264: download Cisco's binary at install, rely on the distro package, or refuse to stream
   without a hardware encoder?
8. Desktop keyboard layout: fixed `us`, configurable, or follow the client?
9. Should the live view map a Mac client's Cmd to Ctrl when the desktop is Linux?
10. Should `mobd mcp` use the desktop proxy on macOS too (one code path)?
