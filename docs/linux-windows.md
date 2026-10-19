# Linux and Windows (draft plan, not implemented)

> **Status: draft for review.** Nothing here is implemented. Today only macOS has a desktop
> backend; Linux and other systems compile `unsupported` (`internal/desktop/backend.go`,
> `backend_linux.go`, `backend_other.go`) and the API answers 501 for `/screen` and `/stream`.
> Items marked *(to verify)* are OS behaviour we have not confirmed on real machines.

mobd keeps one shape on every OS: a per-user daemon inside the logged-in graphical session that
runs agent CLIs, drives the desktop through one `desktop.Backend`, and streams H.264 over Pion.
This plan adds a Linux and a Windows backend and the service, socket, path and packaging pieces
each OS needs, without changing the HTTP API, the stream protocol or the `Backend` interface's
meaning.

## Summary

| | Linux | Windows |
| --- | --- | --- |
| Session model | `systemd --user` unit bound to `graphical-session.target` | per-user logon Scheduled Task (no Windows service) |
| Capture | Wayland: xdg-desktop-portal ScreenCast + PipeWire; X11: XShm + XDamage | DXGI Desktop Duplication (Windows.Graphics.Capture later) |
| Encode | GStreamer: VA-API, then NVENC, then OpenH264 (Cisco binary) | Media Foundation H.264 MFT (hardware, then Microsoft software MFT) |
| Input | Wayland: portal RemoteDesktop over D-Bus (libei later); X11: XTest | `SendInput` (scan codes, `KEYEVENTF_UNICODE`) |
| Permissions | one portal grant, kept with `restore_token` | none beyond integrity level (UIPI) |
| Control socket | Unix socket, as today | AF_UNIX socket file + ACL (named pipe rejected, see below) |
| Packages | `.deb` (Ubuntu 22.04/24.04), tarball; `.rpm` later | signed zip + PowerShell installer; MSI later |
| Recommended order | **first** (Ubuntu 24.04 GNOME Wayland, then X11) | second |

## What the current code assumes

These macOS-only assumptions must be generalized; each is cited so the work list is concrete.

- **Backend contract** (`internal/desktop/backend.go`): main display only, points (logical
  pixels) with `Display.Scale`, snapshots without the cursor, a `Stream` adjusted while it runs
  (`SetBitrate`, `SetSize` with a keyframe at the new size, `SetFrameRate`, `Keyframe`) and
  never restarted, Annex B access units **starting with an AUD, SPS/PPS on keyframes**
  (`AccessUnit`). The stream service signals `profile-level-id=42e01f` (constrained baseline,
  `internal/stream/stream.go`), so other encoders must produce constrained baseline, no B-frames.
- **Delivery semantics** (`internal/desktop/stream_darwin.go`): a 16-unit buffer, and after a
  drop the stream discards deltas until the next keyframe and requests one. New backends should
  reuse this logic, not copy it (see [Build structure](#build-structure)).
- **Errors** (`internal/desktop/desktop.go`, `screen.go`): `ErrUnsupported` maps to 501
  (`internal/api/stream.go`, `media.go`) and its text says "only supported on macOS";
  `ErrScreenUnavailable` maps to 503 with the reason from `unavailable()`, whose wording mentions
  "office Mac"; `ErrNoAccessibility` is macOS wording. All three get OS-neutral or per-backend text.
- **Permissions** (`desktop.Permissions{Accessibility, ScreenRecording}`) feed `/health`
  `accessibility` and `screenRecording` (`internal/api/health.go`), with macOS hints.
  `serve` calls `desktop.RequestPermissions()` first (`cmd/mobd/main.go`).
- **Keys** (`internal/desktop/keys.go`, `remote_darwin.go`): `keyCodes` and `webKeyCodes` are
  macOS virtual key codes; `ParseHotkey` maps `cmd`/`meta` to `command` and `alt` to `option`;
  `Modifiers` has `Option`/`Command` fields.
- **Agent tools** (`internal/mcpserver/server.go`): the instructions and tool descriptions say
  "office Mac", "Quartz", "pasteboard + Cmd+V", "macOS application ... bundle id".
- **`mobd mcp` is its own process** and calls `internal/desktop` directly (`hostDesktop`). On
  macOS it inherits TCC grants from the responsible app. It already reaches the daemon for
  `ask_user` through `MOB_API`, `MOB_TASK_ID` and `MOB_TASK_TOKEN` (`internal/mcpserver/ask.go`).
- **Service** (`internal/service/service.go`): `New()` fails unless `GOOS == darwin`; it uses
  launchd (`launchctl bootstrap/bootout/print`, `gui/<uid>`), writes `~/.mobd/service.json`,
  `service-args.json`, `mobd.log`, captures `PATH` and absolute `codex`/`claude` paths at `start`,
  and `logs` runs `/usr/bin/tail -F`.
- **Control socket** (`internal/control/control.go`): `~/.mobd/run/control.sock`, directory must be
  0700 and owned by `os.Getuid()` (`syscall.Stat_t`), a `flock` on `control.sock.lock`, socket
  chmod 0600.
- **Pairing store** (`internal/pairing/store.go`) also uses `syscall.Flock` on a `.lock` file.
- **Runners** (`internal/runner/cli/cli.go`): `Setpgid` and `syscall.Kill(-pid, SIGKILL)` for the
  process group; codex and claude are found with `exec.LookPath` and get `os.Environ()`.
- **Paths** (`internal/config/config.go`): config `~/.mobd/config.json`; `statePath` defaults to
  `os.UserConfigDir()/mob/state.json` (Linux `~/.config/mob/state.json`, Windows
  `%APPDATA%\mob\state.json`); `dataDir` defaults to its directory.
- **Build today:** `GOOS=linux CGO_ENABLED=0 go build ./...` succeeds. `GOOS=windows` fails
  in `internal/pairing` (`syscall.Flock`) and `internal/runner/cli` (`Setpgid`, `syscall.Kill`).
  `internal/control` and `internal/service` (`os.Getuid` works, `Stat_t`/`Flock` do not) are next.
- **Release** (`.github/workflows/release.yml`, `.github/release/*`, `docs/releases.md`): one
  macOS arm64 job; `install.sh` refuses anything but Darwin arm64 and checks the Apple signature;
  `release.json` has `platforms.darwin-aarch64`.

## Cross-cutting changes (both OSes)

1. **Desktop proxy for `mobd mcp`.** On Wayland each process needs its own portal session
   (another dialog and another restore token). On Windows `mobd mcp` would hit the same UIPI and
   session limits. So `mobd serve` should own the only backend instance, and `mobd mcp` should
   use a `proxyBackend` that forwards `Snapshot/Click/Key/Paste/ListWindows/OpenApp` to
   `POST /internal/tasks/{id}/desktop/...`, authenticated with the existing task token like
   `ask`. Required on Linux. Recommended everywhere so behaviour is the same, with macOS keeping
   direct calls as a fallback when `MOB_API` is unset (to decide).
2. **Shared Go pieces** in `internal/desktop`: an Annex B normalizer (prepend AUD, cache and
   prepend SPS/PPS before IDR slices, set `Key`), the unit delivery and drop logic from
   `macStream.deliver`, and per-OS key tables keyed by web `KeyboardEvent.code`. These are pure
   Go and unit-tested with the existing `h264_test.go` helpers (`nalTypes`, `spsSize`).
3. **Generalize permissions:** `Permissions` becomes `{Input, Capture bool; Detail string}`.
   `/health` keeps `accessibility` and `screenRecording` for API v3 clients and adds neutral
   `input` and `capture` checks plus `desktop: {backend, session}` (e.g. `linux/wayland-gnome`,
   `windows`). Clients move to the new names; the old keys are removed in a later API version.
4. **Modifiers and hotkeys:** `command` means Super on Linux and Win on Windows, `option` means
   Alt. Add `super`/`win` aliases. Make the MCP instructions per OS
   (`instructions(backend.Name())`), telling agents to use `ctrl+c`/`ctrl+v` off macOS, and
   change "Quartz"/"pasteboard"/"bundle id" text to match. Whether the live view should map a Mac
   client's Cmd to Ctrl is an open question.
5. **OS-neutral error text** in `ErrUnsupported`, `unavailable()` and per-backend permission
   errors.
6. **Agent `Key()` letters follow the keyboard layout:** `key("a")` should produce "a" on AZERTY.
   Resolve characters through the layout (X11 keysym→keycode, Windows `VkKeyScanExW`, Wayland
   portal `NotifyKeyboardKeysym`). Live-view `KeyCode` stays physical, which is what
   `KeyboardEvent.code` means. macOS has the same gap today.

## Linux

### Session types

Detect the session once at start from `XDG_SESSION_TYPE`, `WAYLAND_DISPLAY` and `DISPLAY` (and
`XDG_CURRENT_DESKTOP` for desktop-specific paths), and pick `wayland` or `x11`. An X11 backend
running under XWayland would see only X clients, so on Wayland X11 is never used for capture or
input. Without either, the backend is `unsupported` with a detail ("no graphical session; mobd must
run inside the user's desktop session"). Targets: Ubuntu 24.04 GNOME 46 Wayland, Ubuntu 22.04
GNOME 42 Wayland and X11, then KDE Plasma 6 Wayland. Other wlroots compositors are best effort.

### Wayland: one portal session

mobd serve opens one `org.freedesktop.portal.RemoteDesktop` session with a ScreenCast source
attached (`SelectDevices` keyboard+pointer, `ScreenCast.SelectSources` monitor, `Start`). The user
sees one dialog ("allow remote interaction / share screen") on first run. Use pure-Go D-Bus
([godbus/dbus](https://github.com/godbus/dbus)) for all portal, logind and screensaver calls.

- **Persistence:** request `persist_mode=2` and store the returned `restore_token` in
  `~/.local/state/mobd/portal.json` (0600), replacing it after every `Start` (tokens are single
  use). RemoteDesktop persistence needs RemoteDesktop portal v2. ScreenCast-only persistence is
  v4. Ubuntu 22.04's xdg-desktop-portal-gnome may not support RemoteDesktop persistence, which
  would mean a dialog after every mobd restart or login *(to verify)*. That decides whether 22.04
  Wayland is supported unattended.
- **Session lifetime:** the session survives while mobd runs. If the compositor closes it (user
  clicks "stop sharing" in the GNOME indicator, or logs out), the backend reports
  `capture: false` with the reason, and `RequestPermissions` (called at serve start and from a
  new `mobd permissions` / `/health` hint) starts a new one.
- **GNOME private API (optional, later):** `org.gnome.Mutter.RemoteDesktop`/`ScreenCast` on the
  session bus need no dialog (gnome-remote-desktop uses them). This is the only fully unattended
  GNOME path but is unstable, private API, and newer GNOME may restrict callers *(to verify)*.
  Not in the first milestone.

### Capture and encode (Linux)

- **Wayland:** the PipeWire node from `Start` (with `OpenPipeWireRemote` fd) feeds the encoder.
  Prefer DMA-BUF frames (zero copy into VA-API), with shared-memory frames as the fallback.
  `cursor_mode`: hidden for snapshots and `ShowCursor=false`, metadata to get the cursor position
  (`SPA_META_Cursor`), embedded when `ShowCursor`. Changing the cursor mode needs a new source
  selection, so keep the metadata mode and draw the cursor only when asked *(to verify GNOME
  support of metadata mode)*. An unchanged screen produces no PipeWire buffers, which matches
  "an unchanged screen produces no frames".
- **X11:** `XShmGetImage` of the root window plus `XDamage` to skip idle frames, colour conversion
  to NV12 before upload (or `vapostproc`). Pure-Go X11 ([jezek/xgb](https://github.com/jezek/xgb),
  with shm, damage, xtest, randr) keeps capture and input out of cgo; only encode is native.
- **Encoder: GStreamer in a thin C shim** (`stream_linux.c`), mirroring macOS where frames never
  leave native code: `pipewiresrc fd=… path=…` or an `appsrc` fed from XShm, then
  `vapostproc ! vah264enc` (VA-API, Intel/AMD), `nvh264enc` (NVIDIA), or `openh264enc`, then
  `h264parse config-interval=-1 ! appsink` (Annex B, SPS/PPS on every IDR). Settings: constrained
  baseline, `b-frames=0`, CBR/low-latency, a very long GOP with key-int-max ≈ infinite (keyframes
  on request), `bitrate` set live, `GstForceKeyUnit` for `Keyframe()`, a capsfilter change
  (`videoscale`) for `SetSize`, and `videorate` max-rate for `SetFrameRate`. Probe at start which
  encoder element works (actually encode one frame) and report it in `/health`.
  - Why GStreamer: it is LGPL, packaged on every distro, already has the PipeWire source, and
    handles VA-API and NVENC header packing. Raw libva requires writing SPS/PPS ourselves. FFmpeg
    libs from distros are often built `--enable-gpl` (with libx264), which we want to avoid.
  - Ubuntu 22.04 ships GStreamer 1.20, so it may lack `vah264enc` and need `vaapih264enc` from
    gstreamer1.0-vaapi *(to verify)*. 24.04 ships 1.24.
  - **x264 is never used** (GPL); the pipeline excludes `x264enc`.
  - **OpenH264 licensing:** Cisco covers the H.264 patent royalties only for its own binary
    downloaded from Cisco. We must not bundle `libopenh264`. Options: rely on the distro package,
    or download Cisco's binary at install time like Firefox and Fedora do. Distro
    builds may lack Cisco's coverage *(legal check needed)*. Most office PCs have an Intel or AMD
    iGPU with VA-API, so the software fallback is a fallback, not the default.
- **Snapshot:** take one frame from the live PipeWire node (start a short-lived consumer when no
  stream is running, close after ~30 s idle), convert to RGBA and encode with Go
  `image/png`/`image/jpeg` via the existing `desktop.Encode`. Do not open a portal session per
  snapshot. X11: `XShmGetImage` directly.

### Input (Linux)

- **Wayland, milestone 1: RemoteDesktop D-Bus `Notify*` methods**, pure Go:
  `NotifyPointerMotionAbsolute(stream, x, y)` in the stream's logical coordinates (= points),
  `NotifyPointerButton` with evdev `BTN_LEFT/RIGHT/MIDDLE`, `NotifyPointerAxis` (pixel deltas,
  matches `WheelEvent`) + `NotifyPointerAxisDiscrete`, `NotifyKeyboardKeycode` (evdev codes; one
  web-code→evdev table shared with X11, where X keycode = evdev + 8), `NotifyKeyboardKeysym` for
  agent keys and Unicode. These work on GNOME 42+. A D-Bus round trip per event is probably fine
  for pointer moves at 60/s, but needs measuring *(to verify)*.
- **Wayland, later: libei** via `ConnectToEIS` (RemoteDesktop v2, GNOME 45+, KDE 6) for lower
  latency. cgo against libei.
- **X11:** XTest `FakeMotion/FakeButton/FakeKey`. For Unicode, map a spare keycode to the keysym
  with `XChangeKeyboardMapping`, press, then restore (as xdotool does).
- **Unicode on Wayland:** keysyms the current keymap lacks (CJK, emoji) cannot be typed through
  keysym injection *(to verify mutter behaviour)*. Fallback: set the clipboard and press Ctrl+V,
  which is what `Paste` does anyway.
- **uinput** (`/dev/uinput`) works for every compositor and the lock screen, but needs root or an
  `input`/udev rule. That lets any process of the user forge input system-wide, and it cannot
  give absolute coordinates without a virtual tablet. Not used by default; it could be an opt-in
  `.deb` udev rule later.
- **Click(count):** send the presses within the double-click time, as macOS does. Linux has no
  click-state field; toolkits count clicks themselves.

### Paste, windows, apps (Linux)

- **Paste:** Wayland: portal Clipboard interface bound to the RemoteDesktop session
  (`SetSelection` + `SelectionWrite`, xdg-desktop-portal 1.18+ / GNOME 46 *(to verify)*), else
  `wl-copy`, then Ctrl+V. X11: own `CLIPBOARD` with xgb (serve `SelectionRequest`), else `xclip`,
  then Ctrl+V. Terminals need Ctrl+Shift+V, an accepted limitation; agents can fall back to
  `key`.
- **ListWindows:** X11: EWMH `_NET_CLIENT_LIST` (or `_STACKING`), `_NET_WM_NAME`, `_NET_WM_PID`,
  `WM_CLASS` as `App`, frame geometry, `_NET_WM_STATE_HIDDEN` for on-screen. Wayland has no
  global window list. Best effort through AT-SPI (`org.a11y.atspi`: apps and frames with titles,
  bounds often missing). On KDE use a KWin script. On GNOME, without an extension, return the apps
  and titles with zero bounds and say so in the tool description. Agents rely on screenshots
  first.
- **OpenApp:** match the target against desktop entries in `XDG_DATA_DIRS`/`~/.local/share/applications`
  (desktop ID such as `org.gnome.Nautilus`, or `Name=`, case-insensitive), launch with
  `gtk-launch <id>` or `gio launch <file>`, else run an executable on `PATH` detached (`setsid`).
  The MCP argument `name_or_bundle_id` keeps its name. Its description says "desktop file ID" on
  Linux and "AppUserModelID" on Windows.

### Display state and keep-awake (Linux)

- **Locked:** logind `Session.LockedHint` (`org.freedesktop.login1`, session from
  `XDG_SESSION_ID`), else `org.gnome.ScreenSaver.GetActive` /
  `org.freedesktop.ScreenSaver.GetActive`.
- **Asleep:** GNOME Wayland: `org.gnome.Mutter.DisplayConfig` `PowerSaveMode` (≠ 0). X11:
  `DPMSInfo`. KDE: PowerDevil D-Bus *(to verify)*. Unknown means `Asleep=false`.
- **KeepAwake:** `org.freedesktop.portal.Inhibit.Inhibit(flags=Idle)` (or
  `org.freedesktop.ScreenSaver.Inhibit` / `org.gnome.SessionManager.Inhibit`). Release with the
  request handle or cookie. To wake a sleeping display, send a zero-length pointer motion
  through the input path (or `SetActive(false)` on the screensaver interface).
  `systemd-inhibit --what=idle` only affects logind's idle action, not GNOME's display blanking,
  so it is not enough on its own.
- **Lock screen:** on GNOME Wayland the screencast should show the lock screen and RemoteDesktop
  input should reach it, so typing the password through `text` unlocks like macOS *(to verify;
  GNOME may stop the session at lock)*. GDM before login is unreachable, as is the macOS login
  window.

### Displays and HiDPI (Linux)

Main display only. Wayland: the stream's `size` property is logical (points); the PipeWire buffer
is pixels; `Scale = pixel width / logical width` (fractional scales are fine as `float64`).
Pointer coordinates are in the same logical space. X11: report `Scale = 1`, so X coordinates and
screenshot pixels are both "points". The main display is the RandR primary output, and capture
crops the root window to it.

### Process, service and paths (Linux)

- **Unit:** `~/.config/systemd/user/mobd.service` with `PartOf=graphical-session.target`,
  `After=graphical-session.target`, `ExecStart=<exe> serve --config ~/.mobd/service.json`,
  `Restart=on-failure`, `WorkingDirectory=<workdir>`, `Environment=PATH=<captured>`, output to the
  journal. `mobd start` first runs `systemctl --user import-environment WAYLAND_DISPLAY DISPLAY
  XDG_SESSION_TYPE XDG_CURRENT_DESKTOP DBUS_SESSION_BUS_ADDRESS XDG_RUNTIME_DIR`. GNOME and KDE
  already import most of these into the user manager *(to verify per distro)*.
- **Command mapping:** `start` = write unit + `systemctl --user start`. `--login` =
  `systemctl --user enable` (`WantedBy=graphical-session.target`). `stop` = stop + disable.
  `status` = `systemctl --user is-active` + `Health`. `restart` as today. `logs` =
  `journalctl --user -u mobd -f -n 100`; if we also keep `~/.mobd/mobd.log` for parity, the unit
  sets `StandardOutput=append:`. The preflight checks in `Manager.Start` (TCP, UDP, control
  socket) and `service.json` / `service-args.json` stay shared code.
- **Linger:** not needed and not enabled. `loginctl enable-linger` would start mobd at boot with
  no graphical session (no capture). Office use is auto-login plus a session-bound unit. Document
  GDM auto-login for a dedicated office PC.
- **PATH:** keep capturing the invoking shell's `PATH` and absolute `codex`/`claude` paths at
  `start` (as `service.go` does). Desktop sessions often lack `~/.npm-global/bin`, nvm and similar
  paths. If `start` itself runs from a non-login context, resolve through `$SHELL -lc 'command -v
  codex'`.
- **Paths:** keep `~/.mobd` (config, `service.json`, socket under `~/.mobd/run/`) for parity and
  existing docs. Do not migrate to XDG now. `statePath` already defaults to
  `~/.config/mob/state.json`. Possibly put the socket in `$XDG_RUNTIME_DIR/mobd/` (tmpfs,
  per-login) later. Paths in `~/.mobd/run` fit 108-byte `sun_path` for normal home dirs.
- **Control socket:** unchanged. **Runners:** unchanged (`Setpgid` + kill the group). Codex
  sandboxes with Landlock/seccomp on Linux, and mobd already uses `danger-full-access` / bypass
  for desktop actuation.

### Networking (Linux)

UDP `streamPort` 9527 and the TCP listen port. `ufw` (Ubuntu) and `firewalld` (Fedora) are
usually off or allow LAN on desktops. The `.deb` does not change the firewall; docs give
`ufw allow 9527/udp` and `firewall-cmd --add-port=9527/udp --permanent`. Tailscale on Linux
works the same as on macOS (`tailscale serve`/Funnel for the WebSocket path; UDP over the tailnet).

## Windows

### Process and session model

Windows services run in session 0 and cannot capture or inject into the interactive desktop.
mobd runs **as the logged-in user in their session**:

- **Scheduled Task** `\mobd\mobd-<user>`: trigger "at log on of <user>", "run only when user is
  logged on", `LeastPrivilege`, no time limit, restart on failure, no window
  (`CREATE_NO_WINDOW` / built with `-H windowsgui` or a tiny launcher so no console flashes).
  Created through the Task Scheduler COM API or `schtasks /Create /XML`. HKCU `Run` is simpler but
  has no restart-on-failure and starts late, so it is not used.
- **Command mapping:** `start` = register the task (without the logon trigger unless `--login`) and
  `schtasks /Run`. `--login` = logon trigger enabled. `stop` = `schtasks /End` + delete. `status`
  = task state + `Health`. `restart` as today. `logs` = Go-side tail of `mobd.log` (no
  `/usr/bin/tail`).
- **Later:** a LocalSystem service that launches a per-session helper (`WTSQueryUserToken` +
  `CreateProcessAsUser`) would reach the logon/lock screen (Winlogon desktop) and survive logoff.
  This is the analogue of the macOS privileged helper. It is a security-sensitive design and not
  in this plan.

### Capture and encode (Windows)

- **Capture: DXGI Desktop Duplication** (`IDXGIOutput1::DuplicateOutput`, D3D11) on the primary
  output. It gives dirty rects (an idle screen yields `DXGI_ERROR_WAIT_TIMEOUT`, so no frames) and
  a GPU texture that feeds the encoder without a copy. The pointer comes separately (position plus
  shape), so snapshots are cursor-free, and `ShowCursor` composites it in a shader. That would
  also allow sending the cursor shape to clients later. Handle `DXGI_ERROR_ACCESS_LOST` on mode
  changes, secure desktop switches and fullscreen apps by re-duplicating inside the same `Stream`.
  Hybrid-GPU laptops need the adapter that owns the output.
  **Windows.Graphics.Capture** (WinRT, 1803+) is the later alternative. It handles DRM-exempt and
  some fullscreen cases better but needs C++/WinRT and shows a capture border unless
  `IsBorderRequired=false` (Windows 11 / newer 10 *(to verify)*).
- **Encode: Media Foundation H.264 encoder MFT.** Enumerate hardware MFTs
  (`MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, MFT_ENUM_FLAG_HARDWARE|ASYNCMFT)`: NVIDIA NVENC, Intel
  QSV, AMD AMF expose themselves here), then the Microsoft software MFT. Feed D3D11 NV12 textures
  through `IMFDXGIDeviceManager` (BGRA→NV12 with `ID3D11VideoProcessor`). `ICodecAPI`:
  `eAVEncH264VProfile_ConstrainedBase`, `AVEncCommonRateControlMode` CBR or low-delay VBR,
  `AVLowLatencyMode`, `AVEncMPVDefaultBPictureCount=0`, a huge `AVEncMPVGOPSize`,
  `AVEncCommonMeanBitRate` (changeable while running), `AVEncVideoForceKeyFrame`. `SetSize`
  resets the MFT's media types or recreates the MFT inside the same `Stream` and emits a keyframe.
  The Go normalizer adds the AUD and ensures SPS/PPS on IDR.
  - The Microsoft H.264 encoder is licensed with Windows, so no OpenH264 is needed. Windows N/KN
    editions lack Media Foundation until the Media Feature Pack is installed, and the setup check
    says so.
  - Vendor MFT quirks (async events, minimum bitrate, resolution limits, dynamic bitrate support)
    are the main encode risk. Test on Intel, NVIDIA and AMD.
  - Direct NVENC/AMF/QSV SDKs are not planned. MF covers them.
- **Snapshot:** one duplicated frame, copied to a staging texture, encoded in Go via
  `desktop.Encode`, or with WIC for large PNGs.

### Input (Windows)

- `SendInput`: mouse with `MOUSEEVENTF_ABSOLUTE` (0–65535 over the primary monitor), buttons,
  `MOUSEEVENTF_WHEEL`/`HWHEEL`. Pixel deltas convert to wheel units: 120 per notch ≈ 100 px
  *(to tune)*. Keys use scan codes (`KEYEVENTF_SCANCODE`, plus `KEYEVENTF_EXTENDEDKEY` for
  arrows, Home/End, right Ctrl/Alt, NumpadEnter), mapped from `KeyboardEvent.code`, so they stay
  layout-independent. Unicode uses `KEYEVENTF_UNICODE` per UTF-16 unit.
- **UIPI:** a medium-integrity mobd cannot drive elevated (admin) windows; `SendInput` silently
  drops events. Report this in `/health` `input` detail and in `ListWindows` (mark elevated
  windows). Running mobd elevated or with `uiAccess=true` (needs a signed binary in Program
  Files) is not the default. `uiAccess` is an open question.
- **Unreachable:** UAC prompts and the lock screen run on the secure desktop (Winlogon). There
  capture fails (`ACCESS_LOST`) and input does nothing. Ctrl+Alt+Del cannot be synthesized
  (`SendSAS` needs a service and policy). **Unlike macOS, the live view cannot unlock Windows.**
  For a dedicated office PC the docs say: disable automatic lock, or use auto-logon.
- **Paste:** `OpenClipboard` + `SetClipboardData(CF_UNICODETEXT)` + Ctrl+V (parity with macOS).
  `Remote().Unicode` uses `KEYEVENTF_UNICODE`.

### Windows, apps, display state (Windows)

- **ListWindows:** `EnumWindows` filtered by `IsWindowVisible`, not cloaked
  (`DWMWA_CLOAKED`), no owner/tool windows. Title from `GetWindowTextW`, PID from
  `GetWindowThreadProcessId`, `App` from `QueryFullProcessImageNameW` (file description if
  available), bounds from `DWMWA_EXTENDED_FRAME_BOUNDS` converted to points. On-screen means
  visible and not minimized (`IsIconic`).
- **OpenApp:** try an App Paths name (`ShellExecuteExW` with the name, e.g. `chrome`, `notepad`),
  then a Start Menu shortcut whose name matches, then an AppUserModelID
  (`shell:AppsFolder\<AUMID>`) for Store apps.
- **Locked:** `WTSQuerySessionInformation(WTSSessionInfoEx)` `SessionFlags`, or watch
  `WM_WTSSESSION_CHANGE` from a message-only window. Also treat "input desktop is not Default"
  (`OpenInputDesktop`) as locked or secure desktop.
- **Asleep:** `RegisterPowerSettingNotification(GUID_CONSOLE_DISPLAY_STATE)` on a message-only
  window, cached. Also report a **disconnected RDP session** (no console display) as unavailable.
- **KeepAwake:** `PowerCreateRequest` + `PowerSetRequest(PowerRequestDisplayRequired)` (handle
  based, safe from any goroutine). Avoid `SetThreadExecutionState`, which is per OS thread and
  fragile under Go's scheduler. Wake the display with a zero-delta `SendInput` mouse move. This
  does not prevent an *explicit* lock or a policy screen-saver lock.
- **DPI:** call `SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)` at start (before any window)
  so APIs return physical pixels. `Display` reports points = pixels / (`GetDpiForMonitor` / 96)
  and `Scale` = DPI / 96, so agent coordinates behave like macOS points. The primary monitor
  origin is (0, 0). `Cursor()` uses `GetCursorPos`.

### Control socket (Windows)

**Recommend AF_UNIX** (Windows 10 1803+, supported by Go's `net` package) over named pipes: the
code (`net.Listen("unix")`, `control.Call` dialing `unix`) stays unchanged, and the client side
stays standard library (named pipes would need `go-winio`). Per-OS pieces move behind
`control_unix.go` / `control_windows.go`:

- **Ownership/mode check:** replace the `Stat_t` uid + 0700 check with "the directory's owner is
  the current user SID and its DACL grants only the user (and SYSTEM)". Create
  `%LOCALAPPDATA%\mobd\run` with an explicit protected DACL (`SetNamedSecurityInfo`, using
  `golang.org/x/sys/windows`). AF_UNIX on Windows checks the socket file's ACL on connect, so the
  directory ACL is the access control.
- **Lock:** `LockFileEx` instead of `flock`, for both control (`control.sock.lock`) and the pairing
  store (`internal/pairing/store.go`), behind one small `internal/filelock` package.
- **Stale socket:** AF_UNIX files on Windows are reparse points. `os.Lstat` does not report
  `ModeSocket` reliably *(to verify on current Go)*, so detect by dialing, then remove.
- Named pipes stay the fallback only if AF_UNIX proves unreliable (e.g. with some security
  products).

### Paths and runners (Windows)

- **Paths:** everything in **`%LOCALAPPDATA%\mobd\`** (`config.json`, `service.json`,
  `mobd.log`, `run\`). Roaming `%APPDATA%` is wrong for a machine-bound daemon. `config.DefaultPath`, `control.DefaultPath` and the service `Dir` move to
  one `config.Dir()` per OS (`~/.mobd` on macOS/Linux). `statePath`'s default
  (`os.UserConfigDir()` = `%APPDATA%\mob\state.json`) is kept for consistency or moved; to decide.
- **Codex/Claude:** npm installs `codex.cmd`/`claude.cmd` shims. `exec.LookPath("codex")` finds
  them via `PATHEXT`, and Go can run `.cmd` files (through `cmd.exe`, with Go's argument escaping
  rules since 1.22). Long prompts are passed on stdin already (`codex/runner.go`), avoiding the
  `cmd.exe` 8191-char limit. `MCPEnv` and JSON args with quotes need testing through `.cmd`
  quoting *(to verify)*. Prefer resolving to the real `node.exe` + script, or the native binary
  where one is shipped, if quoting breaks.
- **PATH capture:** the Scheduled Task gets the user's environment from the registry, which
  includes npm's `%APPDATA%\npm`. Still record absolute paths at `start` as on macOS.
- **Process tree:** replace `Setpgid` + `kill(-pid)` with a **Job Object** per turn
  (`CreateJobObject`, `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, assign after start, or start suspended
  via `CREATE_SUSPENDED` to avoid the race), and `TerminateJobObject` on cancel. `KILL_ON_JOB_CLOSE`
  also covers the daemon crash case that the `// shortcut:` comment in `cli.go` leaves open on
  Unix. Files: `cli/proc_unix.go`, `cli/proc_windows.go`.
- **Shells:** codex and claude choose their own shell (PowerShell/`cmd` on Windows; Git Bash for
  Claude Code). `runner/activity.go`'s `shellWrapper` regex only strips `zsh/bash/sh -lc`. Extend
  it for `powershell.exe -Command`/`pwsh -c`/`cmd /c`.
- **Signals:** `signal.NotifyContext(..., os.Interrupt, syscall.SIGTERM)` compiles on Windows.
  `schtasks /End` terminates the process without a signal, so `stop` should first ask the daemon
  to exit over the control socket (new `POST /shutdown`) for a graceful runner shutdown.

### Networking (Windows)

The installer adds a Defender Firewall rule for `mobd.exe` (TCP listen port, UDP 9527) on the
Private and Domain profiles (`New-NetFirewallRule`, needs one UAC prompt at install).
Otherwise the first listen pops the "allow access" dialog. That dialog shows only for
interactive users and is easy to dismiss wrongly. Tailscale on Windows works the same; its
interface is on the Private profile *(to verify)*.

## Packaging and release

Keep the R2 layout (`mobd/vX.Y.Z/…`, `latest.json`, `releases.json`) and add platform keys to
`release.json` in the existing style (`darwin-aarch64` today): `linux-x86_64`, `linux-aarch64`,
`windows-x86_64`.

| Artifact | Contents | Signing |
| --- | --- | --- |
| `mobd_<v>_linux_amd64.tar.gz` / `_arm64` | `mobd`, LICENSE, README, `mobd.service` template | SHA256SUMS; optional minisign/cosign signature |
| `mobd_<v>_amd64.deb` / `_arm64.deb` | `/usr/bin/mobd`, Depends on GStreamer base/good/pipewire, va plugin, libva | apt repo later (signed) |
| `mobd_<v>_windows_amd64.zip` | `mobd.exe`, LICENSE, README, `install.ps1` | Authenticode (OV/EV or Azure Trusted Signing), timestamped |

- **Linux build:** cgo (GStreamer shim, optional libei) built on `ubuntu-22.04` runners for the
  glibc floor (22.04 → glibc 2.35), `ubuntu-24.04-arm` for arm64. Link GStreamer dynamically;
  never bundle it. AppImage is not planned (the portal, PipeWire and GPU drivers must come from
  the host anyway). `.rpm` via nfpm later. Open question: `dlopen` GStreamer so the
  same binary runs runner-only on headless machines without GUI libraries.
- **Windows build:** `windows-2022` runner. MF, D3D11 and DXGI are system DLLs, so the shim is
  C/C++ compiled with mingw-w64 (cgo needs gcc/clang, not MSVC). Pure-Go `syscall`/COM vtables
  via `golang.org/x/sys/windows` are an option for the simpler parts (SendInput, clipboard,
  windows, power, WTS) and keep cgo to capture + encode. Cross-compiling from Linux with
  mingw-w64 is possible but CI builds on Windows for simpler testing. Sign with `signtool`
  (or Azure Trusted Signing in CI). New certificates start with SmartScreen warnings until they
  build reputation; EV no longer bypasses this *(to verify current policy)*.
- **Installers:** `install.sh` gains a Linux branch (detect `uname -s/-m`, download the tarball,
  check SHA256 from `release.json`, install into `~/.local/bin`, print the dependency line for
  apt/dnf). Today it exits on non-Darwin. Windows gets `install.ps1` (`irm …/install.ps1 | iex`)
  that verifies SHA256 and the Authenticode signer (`Get-AuthenticodeSignature`), installs to
  `%LOCALAPPDATA%\Programs\mobd`, adds it to the user `PATH`, and adds the firewall rule
  (elevated step). MSI (WiX) only if IT departments ask for it.
- **CI:** `release.yml` grows a matrix (`macos-15`, `ubuntu-22.04`, `ubuntu-24.04-arm`,
  `windows-2022`). The draft-release job collects all assets into one `SHA256SUMS`.
  `publish-r2.yml` and `.github/release/release.mjs` learn the new platform keys. The
  `release.mjs` tests cover the multi-platform manifest.

## Build structure

```text
internal/desktop/
  backend.go                 interface, unsupported (unchanged)
  annexb.go, deliver.go      shared normalizer and unit delivery (pure Go, tested everywhere)
  keymap_evdev.go            web code -> evdev (Linux, pure Go)
  keymap_windows.go          web code -> scan code + extended flag
  proxy.go                   proxyBackend for `mobd mcp` (pure Go)
  backend_linux.go           picks wayland / x11 / unsupported at start
  wayland_linux.go           portal session, D-Bus input, restore token (pure Go, godbus)
  x11_linux.go               xgb: XShm, XDamage, XTest, EWMH (pure Go)
  stream_linux.go/.c/.h      GStreamer pipeline (cgo)
  power_linux.go, apps_linux.go
  backend_windows.go         SendInput, clipboard, windows, apps, WTS, power (x/sys/windows)
  stream_windows.go/.cpp/.h  DXGI duplication + MF encoder (cgo, mingw)
  backend_other.go           //go:build !darwin && !linux && !windows
internal/filelock/           flock / LockFileEx
internal/control/            control_unix.go, control_windows.go (ownership, ACL)
internal/runner/cli/         proc_unix.go (Setpgid), proc_windows.go (Job Object)
internal/service/            service.go (shared preflight, snapshots) + launchd_darwin.go,
                             systemd_linux.go, schtasks_windows.go behind one Manager interface
```

`CGO_ENABLED=0` builds keep working on every OS. A `nocgo` path falls back to `unsupported`
streaming but keeps input and snapshots where pure Go allows (X11, Windows), which is handy for
CI and for runner-only installs. The `Backend` interface keeps its methods. `Permissions`
changes shape (cross-cutting item 3).

## Testing

- **Unit (every OS in CI):** Annex B normalizer, delivery/drop logic, key tables (every
  `KeyboardEvent.code` in the macOS table has a Linux and Windows entry), hotkey modifiers,
  `proxyBackend` against a fake server, systemd unit / task XML generation (golden files like
  `service_test.go`), control socket ownership on each OS, Job Object kill of a grandchild,
  `.cmd` shim argument quoting, portal restore-token rotation against a fake D-Bus portal.
- **CI runners:** `ubuntu-24.04` runs `go test ./...` plus an X11 integration job under `Xvfb`
  with a GStreamer software encoder (the encoder probe picks OpenH264 there, or a test-only x264
  build that is never shipped) and checks a real stream reaches the viewer, like
  `internal/stream/desktop_darwin_test.go`. `windows-2022` runs unit tests. Hosted Windows
  runners have a session but no GPU, so DXGI duplication and the software MFT are tested where
  the runner allows *(to verify)*. Wayland needs a headless mutter/weston in CI. That is
  optional; manual testing comes first.
- **Manual matrix** (extend `docs/office-smoke-checklist.md`): Ubuntu 24.04 GNOME Wayland (Intel
  VA-API, NVIDIA proprietary), Ubuntu 22.04 GNOME X11 and Wayland, Kubuntu/KDE 6 Wayland,
  Windows 11 23H2+ and Windows 10 22H2 (Intel, NVIDIA, AMD), HiDPI 150%/200% and fractional
  125%, lock screen, display sleep, logoff/logon, RDP disconnect (Windows), elevated window
  (UIPI), mobd restart without a new portal dialog.
- **stream-probe:** `go run ./cmd/stream-probe -url … -token …` from another machine against each
  target, with `-ws`, `-quality low/high`, `-pli`, `-click`, `-type`, recording first-picture time,
  bitrate and fps against the macOS baseline.

## Phases

Rough effort assumes one engineer familiar with the codebase.

| Phase | Scope | Effort |
| --- | --- | --- |
| **0. Portable core** | `filelock`, control/pairing per OS, Job Objects, service `Manager` interface, `config.Dir()`, neutral errors, generalized permissions/health, per-OS MCP text, shared Annex B + delivery, `proxyBackend` and the `/internal/tasks/{id}/desktop` routes. Result: mobd **runs runners** on Linux and Windows (no desktop), and `GOOS=windows` builds. | 1.5–2 weeks |
| **1. Linux X11** | xgb capture/input/windows, GStreamer encode with VA-API/OpenH264, keep-awake, state, systemd user unit, tarball. Validates the encode shim and stream contract on Linux with the fewest unknowns. | 2–3 weeks |
| **2. Linux Wayland (GNOME)** | portal session + restore token, PipeWire into GStreamer, D-Bus input, clipboard, AT-SPI best-effort windows, NVENC path, `.deb`, install.sh Linux branch, CI matrix. Ubuntu 24.04 first; decide 22.04 Wayland after checking persistence. | 3–4 weeks |
| **3. Windows** | DXGI duplication, MF encoder (hardware + software), SendInput, DPI, clipboard/windows/apps, WTS/power, Scheduled Task, AF_UNIX + ACL, firewall, signed zip + install.ps1, CI. | 4–6 weeks |
| **4. Hardening** | KDE Wayland, libei, Windows.Graphics.Capture, cursor shape to clients, `.rpm`, MSI, arm64 Windows, service + per-session helper for the Windows lock screen. | as needed |

**Recommendation: Linux first** (phases 0, 1, 2, then 3), because:

- Linux is closer to the current code: Unix sockets, `flock`, process groups and file modes all
  carry over, so phase 0 is mostly Windows work that can trail.
- X11 is a low-risk first backend that proves the GStreamer shim and the stream contract on Linux.
  Wayland then only replaces capture/input. On Windows, capture, encode, input, the session
  model and signing are all new at once.
- The lock screen behaves like macOS on GNOME (to verify), so the live view keeps its unlock
  story. On Windows it cannot, which needs a product decision first.
- Release cost is lower: no paid code-signing certificate or SmartScreen reputation is needed to
  start.

Choose Windows first instead if most target office PCs run Windows. That is a market question
for the owner, not a technical one.

## Risks

1. **Wayland unattended operation:** if RemoteDesktop restore tokens do not persist (Ubuntu 22.04,
   some portals), every mobd restart or login needs someone to click a dialog. That breaks the
   remote office use case. Mitigations: X11 session, GNOME private API, or uinput (opt-in).
2. **Windows lock screen and UAC are unreachable** from the user session, so the live view
   cannot unlock the PC or answer UAC. Fixing it needs a SYSTEM service + session helper, which
   is a significant security surface.
3. **Encoder variability:** VA-API driver differences (Intel iHD vs i965, Mesa AMD, NVIDIA
   needing nvh264enc), vendor MF MFT quirks and dynamic bitrate support. Probing each encoder
   at start and falling back to software is essential.
4. **H.264 licensing:** OpenH264 distribution terms on Linux (do not bundle). Microsoft MFT is
   fine; N editions need the Media Feature Pack.
5. **Wayland window list** has no portable API, so `list_windows` is weaker on Wayland and
   agents must rely on screenshots.
6. **Unicode typing on Wayland** without clipboard for characters outside the keymap.
7. **`.cmd` shim quoting** for codex/claude arguments on Windows (TOML/JSON in `-c` values).
8. **CI coverage:** GPU encode and Wayland cannot be tested on hosted runners, so they need
   manual or self-hosted hardware runs.

## Open questions for the owner

1. Target order: are office PCs mostly Ubuntu or Windows? (This plan assumes Linux first.)
2. Is Ubuntu 22.04 **Wayland** required unattended, or is "22.04 uses X11, 24.04+ Wayland"
   acceptable?
3. Is the GNOME private Mutter API, or an opt-in uinput rule, acceptable for unattended
   Wayland?
4. Windows: is "no lock screen / UAC in the live view; disable auto-lock on dedicated PCs" OK for
   v1, or is the SYSTEM service + session helper required?
5. Windows signing: OV, EV, or Azure Trusted Signing? Who owns the certificate?
6. OpenH264: ship a "download Cisco's binary" step, or rely on VA-API/NVENC only and fail with a
   clear message when no hardware encoder exists?
7. Live view: should a Mac client's Cmd key map to Ctrl when the office PC is Linux/Windows
   (client setting), or stay Super/Win?
8. `/health`: keep `accessibility`/`screenRecording` keys for v3 clients and add `input`/`capture`,
   with removal in API v4?
9. Paths: keep `~/.mobd` on Linux (parity) and `%LOCALAPPDATA%\mobd` on Windows. Should
   `statePath` on Windows stay in roaming `%APPDATA%\mob`?
10. Should `mobd mcp` use the desktop proxy on macOS too (one code path), or only where required?
