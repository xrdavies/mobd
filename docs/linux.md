# Linux

> **Status: implemented** (phases 0–4 of the [Linux design](linux-design.md), branch
> `feat/linux`). This page is the plan the design started from; where the code ended up
> differently, [As built](linux-design.md#as-built) says so. Items still marked *(to verify)*
> were not confirmed on real machines. Install and use: [README](../README.md#install--run-office-linux-computer);
> other distributions: [building on Linux](linux-build.md). The Windows plan is separate:
> [Windows](windows.md).
>
> Decided by the owner: desktops sleep (freeze) when idle and are closed only when the user
> archives or deletes the conversation or closes its desktop; low memory is reported to the user
> (warning below 15% available), never solved by closing desktops; low disk is reported the same
> way. `mobd stop` asks before closing desktops (default no); uninstalling closes them; data is
> always kept. Desktops use the machine's locale. Ubuntu and Debian are supported; other
> distributions get build and configuration instructions.

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
| Desktop model | one virtual X display per conversation (session), created on demand, frozen when idle, closed only when the user deletes the conversation or closes the desktop |
| Display server | Xvfb, one per desktop, plus openbox and a private D-Bus session bus |
| Capture | XShm + XDamage on that display (pure Go, [jezek/xgb](https://github.com/jezek/xgb)) |
| Encode | GStreamer: VA-API, then NVENC, then OpenH264 (not bundled); never x264 |
| Input | XTest on our own display: no permission prompt |
| Permissions | none |
| Process model | `systemd --user` unit with linger (default) or a system unit with a dedicated user |
| Concurrency | one running turn per conversation, `maxRunning` turns overall; memory is reported, never reclaimed by closing desktops |
| Distributions | Ubuntu 22.04/24.04 and Debian 12/13 supported (`.deb`, tarball); others build and configure it themselves from a guide |
| Tests | real end-to-end desktop and stream tests on hosted Ubuntu CI runners |

## Desktop model

- **One desktop per conversation.** A desktop is created when a turn of that session is
  dispatched (see [Concurrency](#concurrency)) and kept across turns, so the agent's browser,
  windows and half-finished forms are still there on the next turn.
- **A desktop is never closed behind the user's back.** It is the conversation's working state
  (open windows, tabs, half-filled forms, unsaved text), so mobd only freezes it:

  | State | When | Resume |
  | --- | --- | --- |
  | **Running** | a turn is active, someone watches it, or it is still busy (see [When a desktop sleeps](#when-a-desktop-sleeps)) | — |
  | **Asleep** | idle by every signal for `desktopSleepAfter` (proposed default 10 min) | instant, everything as it was |
  | **Closed** | only when the user archives the conversation (`PATCH /sessions/{id}` with `archived: true`), deletes it (`DELETE /sessions/{id}`) or closes its desktop (`DELETE /desktops/{sessionId}`, a "Close desktop" action in the apps) | the next turn starts a new one: files and browser profile kept, windows gone |

  Archiving closes the desktop to free its memory; the apps confirm first ("Open windows on this
  conversation's desktop will be closed. Files and browser logins are kept."). An unarchived
  conversation gets a new desktop with its next turn. A turn, `POST /stream`, or `POST /desktops/…` wakes an asleep
  desktop; a snapshot does not.
- **Sleeping = cgroup freezer.** Every desktop lives in its own cgroup (a systemd transient scope,
  see [Process model](#process-model)): Xvfb, openbox, dbus, the apps, and the runners of its
  turns. Writing `cgroup.freeze = 1` stops every process in it, including apps the agent started
  from its shell with `setsid`, which a process-group `SIGSTOP` would miss. An asleep desktop uses
  no CPU; its memory stays allocated (and can be swapped out, see [Memory](#memory)). After
  waking, web pages may have dropped their network connections while frozen, so some reconnect or
  need a reload; the windows themselves are intact.
- **What sleeps with the desktop:** everything in its cgroup: the apps, and any command or server
  the agent left running in the background (`setsid npm run dev &`, a long download). Turns never
  sleep: a desktop with an active turn, including one waiting for a question or approval, stays
  awake, and a desktop that is still doing work is not put to sleep (next section). If a frozen
  background job matters, waking the desktop resumes it where it stopped; network connections it
  held may have timed out.
- **Restarts:** desktops survive a mobd restart or upgrade (they are not mobd's children; mobd
  re-attaches at start, see [Process model](#process-model)). A **machine reboot** cannot keep
  them: windows are lost, files and browser profiles are kept, and browsers restore their tabs.
  The apps say "the desktop was restarted after the office machine rebooted". Checkpointing a
  running X server and browser to disk (CRIU) is not practical.
- **When a desktop sleeps:** mobd checks every desktop once a minute. It goes to sleep only after
  **all** of these have held for `desktopSleepAfter`:
  1. **No active turn** for the session: none running, queued, or waiting for an answer
     (task store).
  2. **Nobody watching:** no live-view session (stream manager); snapshots do not count (below).
  3. **No CPU work:** the desktop's cgroup used less than `desktopSleepCPU` (proposed 2% of one
     core) on average (`cpu.stat` `usage_usec`). A build, test run, server handling requests or
     page still loading keeps it awake.
  4. **No screen changes:** no XDamage events on the display (mobd keeps a damage subscription on
     every desktop; it costs nothing while the screen is still). A playing video, a progress bar
     or a terminal printing output keeps it awake.

  Any signal resets the timer. Signals 3 and 4 catch background work the agent left behind, so
  "idle" means really idle, not just "no turn". A job that waits silently for minutes (a long
  `sleep`, a slow network transfer with no output) can still be frozen; the agent's MCP
  instructions tell it to finish or report background work before ending a turn. The apps show
  the state (`running`/`asleep`) per conversation.
- **Viewers do not keep a desktop awake forever.** An open but unattended live view or a polling
  snapshot would otherwise block sleep indefinitely (signal 2):
  - **Snapshots never wake or hold a desktop.** Before freezing, mobd keeps the desktop's last
    frame; `GET /screen` on an asleep desktop returns it with `X-Mob-Desktop-State: asleep` and
    does not count as watching. Only a live view (`POST /stream`), a turn or
    `POST /desktops/{sessionId}` wakes it.
  - **Clients pause an unattended live view** *(client work, all platforms including macOS)*:
    after `liveIdlePause` (proposed 10 min) with no input from the user and no new frame, the
    app ends the stream and shows "Live view paused · click to resume".
  - **Clients stop when nobody can see the view:** a hidden or minimised window, or a phone app in
    the background, ends the live view and stops snapshot polling (the desktop app already stops
    snapshots when its window is hidden).

  Once the stream ends, the normal timer applies and the desktop sleeps after
  `desktopSleepAfter`; resuming the view wakes it instantly.
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
| Xorg + dummy driver | real Xorg, xrandr modes | needs a generated `xorg.conf` per display, heavier, no real GPU benefit with the dummy driver |
| headless Wayland (sway/weston `--headless`) | modern stack | capture and input need compositor-specific protocols (wlr-screencopy, virtual-pointer), no portable window list; out of scope by decision |

Each desktop runs
`Xvfb :N -screen 0 <W>x<H>x24 -dpi <96·scale> -nolisten tcp -auth <xauth> -s 0 +extension RANDR`
(`-s 0` disables the screen saver, so a desktop is never "asleep" or "locked").
Size is `desktopSize` (default `1920x1080`) and `desktopScale` (default `1`; `2` sets the DPI,
`Xft.dpi` and `GDK_SCALE`, and `Display.Scale = 2`, so agent coordinates stay in points like on
macOS). Changing the size means a new desktop; Xvfb cannot resize a running display.
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

- **Locale and time zone:** each desktop uses the machine's settings: `LANG`/`LC_*` from
  `/etc/default/locale` (or `localectl`), the time zone from `/etc/localtime`, and the matching XKB
  layout as the default for `desktopKeyboardLayout`. A system unit gets them the same way, since
  systemd does not pass the login locale. Fonts for the locale (e.g. `fonts-noto-cjk` for zh/ja/ko)
  become a dependency of the `.deb`, not a recommendation. Typing still goes through mobd (keysym
  remapping); no input method (ibus/fcitx) runs inside a desktop in v1.
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
- **Keyboard layout:** mobd sets a fixed XKB layout on each desktop (`desktopKeyboardLayout`,
  default the machine's layout, else `us`). Live-view `key` messages map `KeyboardEvent.code` to evdev codes
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
  cgroup (`apps/` in its scope) and its own session, not the turn's process group: the runner's
  group is killed when a turn is cancelled (`internal/runner/cli/cli.go`), and apps opened through `open_app` should survive it. Apps the
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
  `logs` follows `~/.mobd/mobd.log` (the unit appends to it, as on macOS). `restart` and upgrades leave desktops running; mobd
  re-attaches to them. Keep capturing `PATH` and absolute `codex`/`claude`
  paths at `start`, as `service.go` does (nvm and `~/.npm-global/bin` are missing from systemd).
- **Option: dedicated user** for servers: a `mobd` system user (home `/var/lib/mobd`) with linger
  runs the same user unit in its own user manager (`mobd setup-system-user`, shipped by the
  `.deb`). A system unit in `system.slice` is not used: it could not move runners into desktop
  scopes under a user manager, and its own cgroup would take the desktops down with it
  ([design](linux-design.md#28-service-manager-internalservice)). The runners must be logged in as
  that user.
- **`mobd stop` and desktops:** desktops are not mobd's children, so stopping mobd does not end
  them by itself. With desktops open, `mobd stop` asks
  `Close 3 desktops (2 asleep)? Open windows will be lost; files and browser profiles are kept. [y/N]`.
  **No (the default)** stops mobd and leaves the desktops as they are; the next `mobd start`
  re-attaches. **Yes** closes them first. `--close-desktops` / `--keep-desktops` answer without a
  prompt (scripts, no terminal: keep). Session data is never deleted.
- **Uninstall:** `apt remove mobd` (the `.deb` `prerm`) and the tarball's uninstall step stop mobd
  and close every desktop; `~/.mobd` and `<dataDir>/desktops` (profiles, downloads) are kept.
  `apt purge` keeps user data too; the docs say which directories to delete by hand.
- **Desktop supervision:** each desktop runs in its own systemd transient scope
  (`mobd-desktop-<session>.scope`, created over D-Bus with `StartTransientUnit` under the user
  manager, `Delegate=yes`): Xvfb, then
  dbus-daemon, then openbox (each waits for the previous one: Xvfb readiness via `-displayfd` or by
  connecting). The scope gives the freezer, per-desktop memory accounting (`memory.current`) and
  lets the desktop outlive mobd. Runners of that session's turns start in a child cgroup of the
  same scope, so the turn-cancel kill (process group, `internal/runner/cli/cli.go`) still works
  and the runner's shell-started apps sleep with the desktop. Requires cgroup v2 (default on
  every supported release).
  - **Xvfb or dbus-daemon exits:** the desktop is dead. The running turn's desktop calls fail with
    a clear error, live-view sessions get `ended` with the reason, an SSE `desktop` event says
    `closed` (reason `crashed`), and the next turn creates a new one. Three crashes within a few minutes mark the
    session's desktop failed until the next turn *(threshold to tune)*.
  - **openbox exits:** restarted in place; X clients survive a WM restart.
  - **The kernel OOM killer** may kill an app inside a desktop (usually a browser tab or the
    browser); the desktop continues and the apps show it. Apps and runners get a raised
    `oom_score_adj` (an unprivileged user manager can raise scores but not lower them
    *(to verify)*), so mobd and Xvfb are the last to go.
- **Display numbers:** allocated from `desktopDisplayBase` (default 100) upward, skipping numbers
  with a live `/tmp/.X<N>-lock`, so mobd displays are easy to recognize and never collide with a
  real `:0`. Each desktop gets a random MIT-MAGIC-COOKIE in its own Xauthority file: Xvfb also
  listens on the abstract socket `@/tmp/.X11-unix/X<N>`, so without a cookie any local user could
  connect.
- **Re-attach at start:** mobd records each desktop (`display`, scope, pids, Xauthority, bus,
  state) in `<dataDir>/desktops/<sessionId>/desktop.json`. At startup it re-attaches to every
  recorded desktop whose scope is alive and whose Xvfb answers with the recorded cookie, keeping
  its state (an asleep desktop stays asleep). Records whose processes are gone (after a reboot)
  are marked closed and their lock, socket and runtime files removed; their sessions start a new
  desktop on the next turn. Desktops of sessions that no longer exist are closed. mobd touches
  nothing it did not record.
- **Runtime directory:** `$XDG_RUNTIME_DIR/mobd/d<N>` (0700; bus socket, Xauthority), or
  `<dataDir>/run/d<N>` when `XDG_RUNTIME_DIR` is unset. Paths stay short for the
  108-byte `sun_path`.

## Concurrency

Desktops no longer compete, so the global "one desktop, FIFO" rule becomes:

- **One running turn per conversation:** already enforced (`ErrSessionBusy`).
- **`maxRunning`** keeps its name and meaning, "turns running at once across all sessions", but
  now limits CPU, memory and API use, not desktop access. Its default stays 1 on macOS and becomes
  4 on Linux when session desktops are on *(to tune)*; `mobd init` must then stop writing
  `"maxRunning": 1` into new configs (`internal/config/config.go` `Init`). `/health` keeps
  reporting it.
- **`maxDesktops`** (new, `MOBD_MAX_DESKTOPS`, default `0` = no cap): an optional hard cap for
  admins who want one. At the cap a turn that needs a new desktop waits; mobd never closes another
  desktop to make room.
- **`desktopSleepAfter`** (new, default `10m`), **`desktopSize`**, **`desktopScale`**,
  **`desktopKeyboardLayout`**, **`desktopDisplayBase`**: as above.

When a turn is dispatched (`scheduleLocked`): an asleep desktop of its session is woken; a session
without a desktop gets a new one unless the [memory guard](#memory) or `maxDesktops` says no, in
which case the turn stays queued with the reason ("waiting: the office machine is low on memory").
Turns whose session already has a desktop may pass it, so one waiting turn does not block the
queue.

### Memory

Desktops are only closed by the user, so memory is managed by **telling the user**, not by
closing desktops:

- **Usage per desktop** comes from its cgroup (`memory.current`), the machine's from
  `/proc/meminfo` (`MemAvailable`) and pressure from PSI (`/proc/pressure/memory`).
- **Low (warning):** `MemAvailable` below `memoryWarnPercent` (proposed 15%) or sustained memory
  pressure. `/health` gets a `memory` check with the detail, `GET /events` sends a `memory` event,
  and the apps show a banner: "The office machine is low on memory. Close the desktops or delete
  the conversations you no longer need." with the desktops listed largest first, each with
  **Close desktop** and **Delete conversation**.
- **Critical:** below `memoryFloorPercent` (proposed 5%) mobd starts **no new desktops**; turns that
  need one wait (above). Running and asleep desktops are untouched.
- **systemd-oomd** (active by default on Ubuntu desktop installs) may kill a whole desktop's scope
  under memory pressure, which loses its windows. mobd does not change it (that needs root);
  `mobd desktops doctor` warns when it is active, and users who want to exclude mobd can add a
  drop-in for `user@.service` with `ManagedOOMMemoryPressure=auto` *(to verify the exact setting)*
  or raise `ManagedOOMMemoryPressureLimit`.
- **Swap or zram** lets the kernel move asleep desktops out of RAM, so many more conversations
  can stay open; the docs recommend it (Ubuntu desktop installs have a swap file; servers often
  do not). Waking a swapped-out desktop takes a moment longer.
- **Disk, reported the same way:** mobd measures each desktop's directory (profiles, cache,
  downloads; a walk every few minutes, cached) and the free space of the data dir's filesystem.
  Below `diskWarnPercent` (proposed 10%) or `diskWarnBytes` (5 GB), whichever is lower: a `disk` check in `/health`, a
  `disk` SSE event and the same banner, listing conversations by disk use. Below `diskFloorBytes`
  (proposed 1 GB) mobd starts no new desktops. Browser wrappers cap the cache
  (`--disk-cache-size`, Firefox `browser.cache.disk.capacity`) so caches do not dominate.
- **Apps learn it from mobd only:** the levels and per-desktop numbers come from `/health`,
  `GET /desktops` and SSE; the apps compute nothing themselves.
- **Sizing:** Xvfb at 1080p is ~10–20 MB plus the framebuffer (8 MB), openbox and dbus a few MB
  each. The browser dominates: 0.5–1.5 GB per desktop with a few tabs. Without swap, about 8–10
  open conversations with a browser fit a 16 GB machine.

### Encoders

Each viewer is one encode; asleep desktops are never encoded. NVIDIA consumer GPUs limit
concurrent NVENC sessions (a driver limit of a few sessions *(to verify the current number)*);
VA-API has no hard limit but finite throughput; OpenH264 costs roughly one CPU core per 1080p30
stream *(to measure)*. When a hardware encoder refuses a new session, mobd falls back to the next
encoder for that stream. Sharing one encode among viewers of the same desktop and size is a later
optimisation.

## Agent environment

- **Runner env:** today codex and claude get `os.Environ()` (`internal/runner/codex/runner.go`,
  `internal/runner/claude/runner.go`) and `mobd mcp` gets `MOB_API`, `MOB_TASK_ID`,
  `MOB_TASK_TOKEN` through `MCPEnv` (`internal/api/media.go` `runOptions`). Runners launched for a
  session additionally get `DISPLAY`, `XAUTHORITY` and `DBUS_SESSION_BUS_ADDRESS` of
  that session's desktop (`XDG_RUNTIME_DIR` stays the user's, so `systemctl --user` and agents
  keep working), and a per-session `bin` directory first on `PATH`
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
    cannot decrypt its cookies. The seed is made from a conversation: log in to the sites in any
    conversation's desktop, close its browser, then "Use this browser profile as the seed" in the
    apps (or `mobd desktops seed <session>`). Profiles are copied, never shared: two running
    browsers cannot use one profile.
  - profiles survive sleep, closing the desktop and reboots, and are removed with the session.
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
  state) after closing its desktop; archiving closes the desktop and keeps the files and profiles.
  `/tmp` stays shared.

## API and clients

Gated by a new `/health` feature **`sessionDesktops`**; without it (macOS, Windows) clients behave
as today.

- **`POST /stream`** body gains `"sessionId"`. With `sessionDesktops` it is required: 404 for an
  unknown session, 409 "the conversation has no desktop yet" when none exists. It wakes an asleep
  desktop, and a viewer keeps it awake. When the desktop stops, the
  session gets `ended` with the reason. The stream manager today takes one global
  `Start`/`Display`/`NewInput` (`internal/api/server.go` `New`); these become per-desktop.
- **`GET /screen?sessionId=…`**: same rule, except that an asleep desktop is not woken: mobd
  returns its last frame with `X-Mob-Desktop-State: asleep`.
- **`GET /desktops`**: `{desktops: [{sessionId, title, display, width, height, scale, state,
  memoryBytes, diskBytes, startedAt, lastUsedAt, activeTaskId, viewers}], memory, disk}`. `DELETE /desktops/{sessionId}` closes
  one (besides archiving or deleting the conversation). `POST /desktops/{sessionId}` starts or
  wakes one without a turn, so a user can log in somewhere before the agent runs.
- **SSE:** `GET /events` gains `desktop` events `{sessionId, state: starting|running|asleep|waking|closing|closed|failed,
  reason}` (a crash is `closed` with reason `crashed`) and `memory` events `{level: ok|low|critical, availableBytes, totalBytes}` and `disk` events
  `{level, freeBytes, totalBytes}`.
- **`/health`:** adds `desktops: {count, asleep, memoryBytes, diskBytes}`, `maxDesktops`, and `memory` and `disk` checks. The macOS permission checks are
  generalized to backend checks: on Linux `xvfb`, `windowManager`, `dbus`, `encoder` (detail e.g.
  "vah264enc (VA-API)" or "openh264enc (software)"), and `fonts` (a warning; see
  [Packaging](#networking-packaging-testing)). `accessibility` and `screenRecording`
  (`internal/api/health.go` `permissionChecks`) are reported `ok` with detail "not needed on Linux"
  so API v3 clients show no false warning; `display` reports the desktop cap instead of sleep/lock.
- **`mobd desktops` CLI** (over the control socket, like `mobd clients`):
  - `mobd desktops list`: session, title, display, state, memory, disk, uptime, last used;
  - `mobd desktops close <session>` (asks; `--yes`), `mobd desktops wake <session>`;
  - `mobd desktops logs <session>`: Xvfb, openbox and dbus output, kept per desktop in
    `<dataDir>/desktops/<sessionId>/logs/` (rotated, 1 MB each) along with crash reasons;
  - `mobd desktops doctor`: checks Xvfb, openbox, dbus, cgroup v2 and the freezer, encoders, fonts
    and locale, and prints what to install or fix.
- **Clients:** with `sessionDesktops`, the live view follows the open conversation: opening
  another conversation re-POSTs `/stream` (one session per client already replaces the old one).
  Before the first turn it shows "no desktop yet; it starts with the first turn". A conversation's
  menu gets **Close desktop** (confirming that open windows will be lost and a running turn
  stopped); the chat list marks
  conversations with a running or asleep desktop; the low-memory banner lists desktops by memory.
  Archiving and deleting a conversation warn that its desktop closes too.
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
- **Desktop size per conversation:** fixed `desktopSize` for everyone in v1, also when watched from
  a portrait phone. A per-conversation size (a new desktop) can come later.
- **Audio:** none. Apps find no managed sound server and stay silent. The
  [remote audio draft](desktop-audio.md) proposes per-desktop virtual outputs,
  monitor capture and streaming over both existing transports.

## Networking, packaging, testing

- **Networking:** TCP listen port and UDP `streamPort` 9527 as on macOS. The `.deb` does not touch
  the firewall; docs give `ufw allow 9527/udp` and
  `firewall-cmd --add-port=9527/udp --permanent`. Tailscale works the same (`tailscale serve` /
  Funnel for the WebSocket path, UDP over the tailnet). On a server without a desktop, host-only
  ICE candidates still cover LAN and tailnet addresses.
- **Supported distributions:** Ubuntu 22.04 and 24.04, Debian 12 and 13, amd64 and arm64. These
  get the `.deb`, CI tests and support. The glibc floor is 2.35 (Ubuntu 22.04); Debian 12 has
  2.36.
- **Other distributions** (Fedora, RHEL/Rocky/Alma, Arch, openSUSE, …): not packaged or tested. A
  guide, `docs/linux-build.md` (written in phase 4), tells users how to do it themselves:
  - build: Go, a C compiler, `pkg-config` and the GStreamer development headers, then `make build`
    (cgo links the system GStreamer, so the binary matches their glibc);
  - runtime packages by name per family (e.g. `xorg-x11-server-Xvfb`, `openbox` from EPEL on RHEL,
    `gstreamer1-plugins-bad-free`, `openh264` from Cisco's repository on Fedora/RHEL);
  - the systemd user unit and linger, cgroup v2 check, firewall (`firewall-cmd`), SELinux notes;
  - `mobd status` and `/health` checks to confirm the setup.
  The tarball (built on Ubuntu 22.04) also runs where glibc ≥ 2.35 and the libraries are present.
- **Packages:** keep the R2 layout and add `linux-x86_64` / `linux-aarch64` keys to `release.json`.
  - `mobd_<v>_linux_amd64.tar.gz` / `_arm64`: `mobd`, LICENSE, README, unit templates; SHA256SUMS.
  - `mobd_<v>_amd64.deb` / `_arm64.deb`: `/usr/bin/mobd`, the disabled system unit. **Depends:**
    `xvfb`, `openbox`, `dbus-x11` (or `dbus-daemon`), `xkb-data`, `x11-xkb-utils` (setxkbmap), `gstreamer1.0-plugins-base`,
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
  - two sessions run at once on separate displays; sleep after a short `desktopSleepAfter` and
    wake with windows intact (a counter in an `xterm` does not advance while asleep); a busy
    background process or a changing screen keeps it awake; archive, close and delete end the
    desktop; the memory guard with a low fake threshold; killing
    Xvfb mid-turn; restarting and `kill -9` of mobd re-attach to running and asleep desktops.

  The mob clients can also be tested end to end against a Linux mobd in CI. Hardware encoders
  (VA-API, NVENC) still need manual or self-hosted runs.
- **Manual matrix** (extend `docs/office-smoke-checklist.md`): Ubuntu 24.04 and 22.04, Debian 12, desktop PC
  with Intel VA-API, NVIDIA NVENC, a GPU-less VM; several sessions watched at once; Chrome and
  Firefox with a seed profile; HiDPI scale 2; `stream-probe` first-picture time, bitrate and fps
  against the macOS baseline.

## Portable core (phase 0, shared by macOS, Linux and Windows)

Phase 0 changes code that all three platforms run, not only the new ones:

| Phase 0 item | macOS | Linux | Windows |
| --- | --- | --- | --- |
| Desktop provider between the API and the desktop | one shared desktop, behaves as today | one desktop per conversation | one shared desktop |
| Service manager interface | launchd (today's code moves behind it) | systemd user unit | per-user logon task |
| File locking, runner process kill | Unix version (today's code) | Unix version | new: `LockFileEx`, Job Objects |
| Control socket | Unix version | Unix version | new: AF_UNIX file with an ACL |
| Config and data folders per OS (`config.Dir()`) | `~/.mobd`, data in `~/Library/Application Support/mob` (unchanged) | `~/.mobd`, data in `~/.local/share/mob` | `%LOCALAPPDATA%\\mobd` ([Windows](windows.md)) |
| Health checks per platform | Accessibility, Screen Recording (as today) | Xvfb, openbox, dbus, encoder, fonts | Windows checks |
| Shared H.264 cleanup and frame delivery | moves out of `stream_darwin.go`, used by macOS too | used | used |
| Key tables per OS | macOS table (today's `keys.go`) | Linux table | Windows table |
| Agent desktop calls through `mobd serve` (MCP proxy) | optional (`MOB_DESKTOP_PROXY=1`, off by default) | always | always |

Phase 0 is mostly a restructuring of today's macOS code: new interfaces with the current behaviour
behind them, then the Linux and Windows implementations. It can break macOS, so every phase 0 step
must pass the macOS tests, and before merging it is checked on a real office Mac (live view, agent
clicks and typing, pairing, `mobd start/stop`).

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
| **0. Portable core** | the list above. Result: mobd runs runners on Linux and Windows (no desktop), `GOOS=windows` builds, and macOS behaves as before. | 1.5–2 weeks |
| **1. Desktop manager** | systemd scopes, Xvfb/openbox/dbus supervision, `mobd stop`/uninstall handling, `mobd desktops`, per-desktop logs, locale, disk guard, display numbers, Xauthority, re-attach after restart, sleep and wake (cgroup freezer), close and delete, runners in the desktop's cgroup, memory guard, `/desktops`, SSE events, health checks, systemd unit. | 2.5–3 weeks |
| **2. X11 backend** | xgb capture and snapshots, XTest input, key tables, Unicode, clipboard, EWMH windows, `OpenApp`, MCP text, CI desktop tests. | 1.5 weeks |
| **3. Streaming** | GStreamer shim, encoder probe and fallback, per-session `POST /stream` and `/screen`, stream-probe in CI. | 1.5–2 weeks |
| **4. Agent environment and release** | browser wrappers, per-session profiles, seed profile, downloads, `.deb` and tarball, `install.sh`, CI release matrix, `linux-build.md` for other distributions, docs. | 1–1.5 weeks |
| **4b. Clients** | mob, mob-ios, mob-android: live view per conversation, running/asleep state, Close desktop, archive/delete warnings, memory and disk banners, pausing an unattended view, stopping when hidden or in the background. Each app's docs are updated when this ships. | 1.5–2 weeks |
| **5. Later** | cursor shape, shared encodes, attach mode, per-session users, GPU, audio. | as needed |

Linux first remains the recommendation: phase 0 is mostly Unix code that already works, there are
no permission prompts or code-signing costs, and every desktop path is testable in CI.

## Risks

1. **Memory:** desktops are never closed automatically, so open conversations accumulate. Asleep
   desktops cost no CPU but keep their RAM. Mitigated by swap, the low-memory banner and the
   critical floor that stops new desktops; it depends on users acting on the reminder.
   **CPU:** browsers on software rendering and software encoding on GPU-less machines; encoding
   runs only while watched.
2. **Encoder variability:** VA-API driver differences (Intel iHD vs i965, Mesa AMD), NVENC session
   limits. Probe at start, fall back per stream.
3. **OpenH264 licensing:** cannot be bundled; on GPU-less servers it is the only encoder.
4. **Apps that assume a full desktop session:** no keyring, no portal, no notification daemon, no
   GPU. Some apps may warn or misbehave *(to collect while testing)*.
5. **Lost windows on reboot:** a machine reboot (or an Xvfb crash) loses open windows and unsaved
   state; only profiles and files survive. Pages may need a reload after waking from sleep.
6. **Same-user isolation:** sessions can read each other's files and profiles.

## Open questions for the owner

1. ~~Browser profiles~~ decided: one per conversation, copied from a seed made from a
   conversation; never shared.
2. `desktopSleepAfter` 10 min and `desktopSleepCPU` 2% of a core (proposed)? Stop new desktops at
   5% available memory (proposed; the 15% warning is decided)?
3. Should every turn get a desktop, or should clients be able to mark a turn "no desktop"?
4. Should mobd ever support the physical desktop (attach to an X11 display; Wayland is out)?
5. Service default: `systemd --user` with linger (proposed) or a system unit with a dedicated user?
6. OpenH264: download Cisco's binary at install, rely on the distro package, or refuse to stream
   without a hardware encoder?
7. Desktop keyboard layout: fixed `us`, configurable, or follow the client?
8. Should the live view map a Mac client's Cmd to Ctrl when the desktop is Linux?
9. Should `mobd mcp` use the desktop proxy on macOS too (one code path)?
