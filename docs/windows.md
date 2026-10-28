# Windows (plan, not started)

> **Status:** planned, waiting for a Windows test machine ([Prerequisites](#prerequisites)).
> The portable groundwork is done and `GOOS=windows go build ./...` succeeds (checked in CI), but
> nothing Windows-specific beyond that is implemented: there is no service manager, desktop
> backend, installer or release. Items marked *(to verify)* are OS behaviour not yet confirmed on
> a real machine. The Linux plan is separate: [Linux](linux.md).

Windows keeps the macOS shape: a per-user mobd inside the logged-in user's interactive session,
driving that **single real desktop**, shared by all tasks and serialized by the FIFO desktop queue
(`maxRunning`, default 1). The HTTP API, the stream protocol and the `Backend` interface's meaning
do not change, so Mob Desktop and Mob Mobile need no Windows-specific work.

## Current state

| Piece | State | Where |
| --- | --- | --- |
| Build for `windows/amd64` (`CGO_ENABLED=0`) | ✅ done, checked by CI's build job | `.github/workflows/ci.yml` |
| Config and data folders | ✅ `%LOCALAPPDATA%\mobd` (config, log, `run\`) | `internal/config/dir.go` |
| File lock | ✅ `LockFileEx` | `internal/filelock/filelock_windows.go` |
| Agent process tree | ✅ Job Object with `KILL_ON_JOB_CLOSE` (children started in the first moments can escape: no suspended start) | `internal/proc/proc_windows.go` |
| Control socket | ✅ AF_UNIX file in a directory with a protected DACL (user + SYSTEM) | `internal/control/control_windows.go` |
| Agent desktop calls through `mobd serve` | ✅ every turn uses the proxy (`MOB_DESKTOP_PROXY=1`), so only mobd serve touches the desktop | `internal/mcpserver/proxy.go` |
| Per-backend health checks | ✅ interface exists (`desktop.Checker`), used by Linux | `internal/desktop/checks.go` |
| Service manager (`mobd start/stop/status`) | ❌ `New()` returns "not supported; use mobd serve" | `internal/service/new_other.go` |
| Desktop backend (screenshots, input, live view) | ❌ `unsupported`; `/screen` and `/stream` answer 501 | `internal/desktop/backend_other.go` |
| Agent launch through `.cmd` shims | ❓ untested | `internal/runner/*` |
| `mobd remote` (Tailscale, Cloudflare) | ❌ macOS/Linux commands (`sudo`, Homebrew, apt) | `cmd/mobd/remote.go` |
| Installer, release, tests on Windows | ❌ none; CI only builds | |

## Stages

Work starts when a Windows machine is available. Each stage is released on its own.

### Stage 1: mobd without the desktop (about 1–1.5 weeks)

The agents work on the Windows PC and everything except the screen works from Mob: pairing,
conversations, Codex, Claude Code and Pi, questions and approvals, notifications, remote access.
Screenshots, the live view and the agent's click/type tools report "not available on Windows yet";
the apps already show that state.

1. **Service manager:** a per-user Scheduled Task
   ([Process and session model](#process-and-session-model)) behind the shared `service.Manager`: `start`, `start --login`, `stop`, `restart`, `status`,
   `logs` (Go-side tail), `uninstall`. A new control-socket `POST /shutdown` so `stop` lets turns end
   cleanly before `schtasks /End`. Golden tests for the task XML like `service_test.go`.
2. **Agents:** run Codex, Claude Code and Pi through their npm `.cmd` shims or native `.exe`, and
   verify argument quoting for MCP arguments, `-c` TOML values and environment
   ([Paths and runners](#paths-and-runners)). Extend `shellWrapper` (`internal/runner/activity.go`)
   for `powershell.exe -Command`, `pwsh -c` and `cmd /c`, so activity shows the command.
3. **Texts:** `ComputerName`/`OSName` and the MCP instructions name Windows; with no backend the
   desktop tools say so instead of failing obscurely.
4. **Health:** Windows checks for the agents and the firewall rule; the desktop items report
   "not available on Windows yet" rather than macOS permissions.
5. **`mobd link` and `mobd remote`:** the LAN address already works (first private IPv4). `remote`
   learns Windows: `tailscale.exe` in `Program Files\Tailscale`, an elevated prompt instead of
   `sudo`, `cloudflared` from `winget` and `cloudflared service install`.
6. **Firewall:** the installer adds the Defender rule ([Networking](#networking)).
7. **Install and release:** `mobd_<v>_windows_amd64.zip` and `install.ps1`, a `windows` job in
   `release.yml` (pure Go, `CGO_ENABLED=0`), `windows-x86_64` in `release.json`, signed
   ([Packaging and release](#packaging-and-release)). The website's download card and Get started.
8. **CI:** a `windows-2022` job runs `go test ./...` (today Windows is only built).

### Stage 2: the desktop (about 4–6 weeks)

DXGI capture, Media Foundation encoding (hardware, then software), `SendInput` with scan codes,
DPI, clipboard, windows and apps, lock and display state, keep-awake; snapshots and the live view.
Needs cgo with mingw-w64 for capture and encode. Designed below:
[Capture and encode](#capture-and-encode), [Input](#input),
[Windows, apps, display state](#windows-apps-display-state). Tested on the
[manual matrix](#testing).

### Stage 3: hardening (as needed)

Windows.Graphics.Capture, cursor shape to clients, MSI, arm64, and a SYSTEM service with a
per-session helper so the live view reaches the lock screen and UAC prompts.

## Prerequisites

Before stage 1:

- **A Windows PC:** Windows 11 (or 10 22H2), x64, the kind of machine users would run mobd on. A
  Windows VM on an Apple Silicon Mac is ARM Windows without a hardware encoder: fine for stage 1,
  not for stage 2's encoders.
- **Reachable for development:** OpenSSH Server enabled (Settings → System → Optional features),
  on the office network or Tailscale, so builds and tests run remotely like on the office Mac.
- **A user that stays logged in,** with automatic lock off, and Codex, Claude Code and Pi installed
  and signed in under that user. Git and Go (and for stage 2, mingw-w64 or MSYS2's
  `ucrt64` toolchain).
- **For stage 2:** ideally a second PC or GPU from another vendor (Intel, NVIDIA, AMD) and a
  high-DPI display (125–200 %).

Before the first public release: the [signing decision](#open-questions-for-the-owner).

## Why not one desktop per agent on Windows

Linux gives each conversation its own virtual display ([Linux](linux.md)). Windows has no
equivalent of Xvfb: a separate interactive desktop that can be captured and driven needs a
separate logon session, i.e. a separate Windows user account in its own RDP session. On client
Windows only one interactive session is allowed at a time; several need Windows Server with
Remote Desktop Services (RDS CALs per user) or unsupported multi-session patches. `CreateDesktop`
desktops in the same session cannot be captured with Desktop Duplication and many apps misbehave
on them. A pool of per-agent accounts on an RDS host is a **possible later option, not planned**.

## Process and session model

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
  `/usr/bin/tail`). `internal/service` already has a shared `Manager` with launchd (macOS) and
  systemd (Linux); the Scheduled Task is its Windows implementation (`schtasks_windows.go`,
  stage 1).
- **Later:** a LocalSystem service that launches a per-session helper (`WTSQueryUserToken` +
  `CreateProcessAsUser`) would reach the logon/lock screen (Winlogon desktop) and survive logoff.
  This is the analogue of the macOS privileged helper. It is a security-sensitive design and not
  in this plan.

## Capture and encode

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
  `eAVEncH264VProfile_ConstrainedBase` (the stream service signals `profile-level-id=42e01f`,
  `internal/stream/stream.go`), `AVEncCommonRateControlMode` CBR or low-delay VBR,
  `AVLowLatencyMode`, `AVEncMPVDefaultBPictureCount=0`, a huge `AVEncMPVGOPSize`,
  `AVEncCommonMeanBitRate` (changeable while running), `AVEncVideoForceKeyFrame`. `SetSize`
  resets the MFT's media types or recreates the MFT inside the same `Stream` and emits a keyframe.
  The shared Go normalizer adds the AUD and ensures SPS/PPS on IDR.
  - The Microsoft H.264 encoder is licensed with Windows, so no OpenH264 is needed. Windows N/KN
    editions lack Media Foundation until the Media Feature Pack is installed, and the setup check
    says so.
  - Vendor MFT quirks (async events, minimum bitrate, resolution limits, dynamic bitrate support)
    are the main encode risk. Test on Intel, NVIDIA and AMD.
  - Direct NVENC/AMF/QSV SDKs are not planned. MF covers them.
- **Snapshot:** one duplicated frame, copied to a staging texture, encoded in Go via
  `desktop.Encode`, or with WIC for large PNGs.

## Input

- `SendInput`: mouse with `MOUSEEVENTF_ABSOLUTE` (0–65535 over the primary monitor), buttons,
  `MOUSEEVENTF_WHEEL`/`HWHEEL`. Pixel deltas convert to wheel units: 120 per notch ≈ 100 px
  *(to tune)*. Keys use scan codes (`KEYEVENTF_SCANCODE`, plus `KEYEVENTF_EXTENDEDKEY` for
  arrows, Home/End, right Ctrl/Alt, NumpadEnter), mapped from `KeyboardEvent.code`, so they stay
  layout-independent. Unicode uses `KEYEVENTF_UNICODE` per UTF-16 unit. Agent `key("a")` resolves
  the character through the layout (`VkKeyScanExW`), so it types "a" on AZERTY too.
- **UIPI:** a medium-integrity mobd cannot drive elevated (admin) windows; `SendInput` silently
  drops events. Report this in `/health` `input` detail and in `ListWindows` (mark elevated
  windows). Running mobd elevated or with `uiAccess=true` (needs a signed binary in Program
  Files) is not the default. `uiAccess` is an open question.
- **Unreachable in v1:** UAC prompts and the lock screen run on the secure desktop (Winlogon).
  There capture fails (`ACCESS_LOST`) and input does nothing. Ctrl+Alt+Del cannot be synthesized
  (`SendSAS` needs a service and policy). **Unlike macOS, the live view cannot unlock Windows.**
  For a dedicated office PC the docs say: disable automatic lock, or use auto-logon.
- **Paste:** `OpenClipboard` + `SetClipboardData(CF_UNICODETEXT)` + Ctrl+V (parity with macOS).
  `Remote().Unicode` uses `KEYEVENTF_UNICODE`.

## Windows, apps, display state

- **ListWindows:** `EnumWindows` filtered by `IsWindowVisible`, not cloaked
  (`DWMWA_CLOAKED`), no owner/tool windows. Title from `GetWindowTextW`, PID from
  `GetWindowThreadProcessId`, `App` from `QueryFullProcessImageNameW` (file description if
  available), bounds from `DWMWA_EXTENDED_FRAME_BOUNDS` converted to points. On-screen means
  visible and not minimized (`IsIconic`).
- **OpenApp:** try an App Paths name (`ShellExecuteExW` with the name, e.g. `chrome`, `notepad`),
  then a Start Menu shortcut whose name matches, then an AppUserModelID
  (`shell:AppsFolder\<AUMID>`) for Store apps. The MCP argument `name_or_bundle_id` keeps its
  name; its description says "AppUserModelID" on Windows.
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

## Agent desktop calls

Done. `mobd mcp` runs per turn and forwards every desktop call to
`POST /internal/tasks/{id}/desktop/{op}` on mobd serve, authenticated with the task token
(`internal/mcpserver/proxy.go`), so only mobd serve owns a backend; on Windows it would otherwise
hit the same UIPI and session limits as a separate process.

MCP instructions and tool descriptions lose "office Mac", "Quartz", "pasteboard + Cmd+V" and
"bundle id" on Windows and tell agents to use `ctrl+c`/`ctrl+v`; `command` means Win and
`option` means Alt in hotkeys.

## Control socket

Done: **AF_UNIX** (Windows 10 1803+, Go's `net` package), so `control.Call` is unchanged and no
named-pipe library is needed. `%LOCALAPPDATA%\mobd\run` gets a protected DACL granting only the
user and SYSTEM (`internal/control/control_windows.go`); the socket file inherits it, and AF_UNIX
on Windows checks that ACL on connect. The lock is `LockFileEx`.

Still to verify on a real machine: AF_UNIX files are reparse points, and `os.Lstat` may not report
`ModeSocket`, so a stale socket is detected by dialing, then removed *(to verify)*. Named pipes
remain the fallback only if AF_UNIX proves unreliable (e.g. with some security products).

## Paths and runners

- **Paths:** done. Everything is in **`%LOCALAPPDATA%\mobd\`** (`config.json`, `service.json`,
  `mobd.log`, `run\`) through `config.Dir()`; roaming `%APPDATA%` is wrong for a machine-bound
  daemon. Conversations stay in `os.UserConfigDir()` (`%APPDATA%\mob`) unless decided otherwise
  (open question).
- **Codex, Claude Code, Pi:** npm installs `codex.cmd` and `pi.cmd` shims; Claude Code's installer
  ships `claude.exe`. `exec.LookPath` finds shims via `PATHEXT`, and Go runs `.cmd` files through
  `cmd.exe` with its argument escaping. Prompts already go over stdin, avoiding `cmd.exe`'s
  8191-character limit. MCP arguments, JSON and TOML values with quotes need testing through the
  shims *(to verify)*; if quoting breaks, resolve the shim to `node.exe` plus its script.
- **PATH capture:** the Scheduled Task gets the user's environment from the registry, including
  npm's `%APPDATA%\npm`. Still record absolute paths at `start`, as on macOS.
- **Process tree:** done: a **Job Object** per turn with `KILL_ON_JOB_CLOSE`, which also cleans
  up after a crashed mobd. A process the agent starts in its first moments can escape the job,
  because Go cannot start a process suspended; runners do not start children that fast. Revisit
  with `CREATE_SUSPENDED` if it shows up.
- **Shells:** Codex and Claude Code choose their own shell (PowerShell or `cmd`; Git Bash for
  Claude Code). `shellWrapper` in `internal/runner/activity.go` only strips `zsh/bash/sh -lc`;
  extend it for `powershell.exe -Command`, `pwsh -c` and `cmd /c` (stage 1).
- **Signals:** `signal.NotifyContext(..., os.Interrupt, syscall.SIGTERM)` compiles on Windows.
  `schtasks /End` terminates without a signal, so `stop` first asks mobd to exit over the control
  socket (new `POST /shutdown`, stage 1).

## Health

Per-backend checks go through `desktop.Checker` (`internal/desktop/checks.go`), as Linux does.
Stage 1 reports the agents and the firewall rule, and the desktop as "not available on Windows
yet". From stage 2 Windows reports `input` (UIPI detail), `capture` (Desktop Duplication works),
`encoder` (which MFT, or the missing Media Feature Pack) and `display` (locked, asleep, RDP
disconnected). The old keys stay for API v3 clients until a later API version.

## Networking

The installer adds a Defender Firewall rule for `mobd.exe` (TCP listen port, UDP `streamPort`
9527) on the Private and Domain profiles (`New-NetFirewallRule`, needs one UAC prompt at
install). Otherwise the first listen pops the "allow access" dialog, which shows only for
interactive users and is easy to dismiss wrongly. Tailscale on Windows works the same; its
interface is on the Private profile *(to verify)*.

## Packaging and release

Keep the R2 layout (`mobd/vX.Y.Z/…`, `latest.json`, `releases.json`) and add `windows-x86_64` to
`release.json` next to `darwin-aarch64`.

- **Artifact:** `mobd_<v>_windows_amd64.zip` with `mobd.exe`, LICENSE, README, `install.ps1`.
  Authenticode-signed (OV/EV or Azure Trusted Signing) and timestamped.
- **Build:** `windows-2022` runner. MF, D3D11 and DXGI are system DLLs, so the shim is C/C++
  compiled with mingw-w64 (cgo needs gcc/clang, not MSVC). Pure-Go `syscall`/COM vtables via
  `golang.org/x/sys/windows` cover the simpler parts (SendInput, clipboard, windows, power, WTS)
  and keep cgo to capture + encode. Sign with `signtool` (or Azure Trusted Signing in CI). New
  certificates start with SmartScreen warnings until they build reputation; EV no longer bypasses
  this *(to verify current policy)*.
- **Installer:** `install.ps1` (`irm …/install.ps1 | iex`) verifies SHA256 from `release.json` and
  the Authenticode signer (`Get-AuthenticodeSignature`), installs to
  `%LOCALAPPDATA%\Programs\mobd`, adds it to the user `PATH`, and adds the firewall rule (elevated
  step). MSI (WiX) only if IT departments ask for it.
- **CI:** `release.yml` (one macOS arm64 job today) grows a `windows-2022` entry; the draft-release
  job collects all assets into one `SHA256SUMS`; `publish-r2.yml` and
  `.github/release/release.mjs` learn the new key.

## Build structure

```text
internal/desktop/
  backend_windows.go         SendInput, clipboard, windows, apps, WTS, power (x/sys/windows)
  stream_windows.go/.cpp/.h  DXGI duplication + MF encoder (cgo, mingw)
  keymap_windows.go          web code -> scan code + extended flag
  backend_other.go           //go:build !darwin && !linux && !windows
internal/service/            schtasks_windows.go next to launchd.go and systemd.go (stage 1)
cmd/mobd/remote.go           Windows paths for tailscale.exe and cloudflared (stage 1)
.github/release/install.ps1  installer (stage 1)

Done: internal/filelock (LockFileEx), internal/control (DACL), internal/proc (Job Object),
internal/config/dir.go (%LOCALAPPDATA%), internal/mcpserver/proxy.go (desktop proxy).
```

Stage 1 is pure Go (`CGO_ENABLED=0`). From stage 2, `CGO_ENABLED=0` builds keep working: without
cgo, streaming is `unsupported` but input and snapshots stay available where pure Go allows.

## Testing

- **Unit:** scan-code table (every `KeyboardEvent.code` of the macOS table has an entry),
  hotkey modifiers, task XML generation (golden files like `service_test.go`), control socket ACL,
  Job Object kill of a grandchild, `.cmd` shim argument quoting.
- **CI:** `windows-2022` runs `go test ./...`. Hosted Windows runners have a session but no GPU,
  so DXGI duplication and the software MFT are tested where the runner allows *(to verify)*.
- **Manual matrix** (extend `docs/office-smoke-checklist.md`): Windows 11 23H2+ and Windows 10
  22H2 on Intel, NVIDIA and AMD; HiDPI 150%/200% and 125%; lock screen; display sleep;
  logoff/logon; RDP disconnect; an elevated window (UIPI); N edition without the Media Feature
  Pack.
- **stream-probe:** `go run ./cmd/stream-probe -url … -token …` from another machine, with `-ws`,
  `-quality low/high`, `-pli`, `-click`, `-type`, against the macOS baseline.

## Risks

1. **Lock screen and UAC are unreachable** from the user session, so the live view cannot unlock
   the PC or answer UAC. Fixing it needs a SYSTEM service + session helper, a significant security
   surface.
2. **Encoder variability:** vendor MF MFT quirks and dynamic bitrate support. Probe each encoder at
   start and fall back to the software MFT.
3. **N editions** need the Media Feature Pack.
4. **`.cmd` shim quoting** for Codex and Pi arguments (TOML/JSON in `-c` values, MCP arguments).
5. **Code signing and SmartScreen:** a paid certificate and reputation building before installs
   look trustworthy.
6. **CI coverage:** GPU encode and real capture cannot be fully tested on hosted runners.
7. **One shared desktop:** like macOS, tasks wait in the FIFO desktop queue; see
   [Why not one desktop per agent](#why-not-one-desktop-per-agent-on-windows).

## Open questions for the owner

Before stage 1:

1. **Lock screen and UAC:** is "the live view cannot reach the lock screen or UAC prompts; turn off
   automatic lock on a dedicated PC" acceptable for the first releases, with the SYSTEM service +
   session helper later (stage 3)? The plan assumes yes.
2. **The test machine:** which PC (CPU/GPU, Windows edition), and how it is reached (OpenSSH over
   the office network or Tailscale)? See [Prerequisites](#prerequisites).
3. **Order against Linux:** mobd for Linux is implemented and needs only packaging and a GPU smoke
   test to ship. Which comes first depends on what office computers users have.

Before the first public release:

4. **Signing:** Azure Trusted Signing or an OV certificate, and in whose name? Without it,
   SmartScreen warns on every install.

Later (stage 2–3):

5. Should mobd offer `uiAccess=true` (signed, in Program Files) to drive elevated windows?
6. Should the live view map a Mac client's Cmd key to Ctrl (client setting), or stay Win?
7. Should conversations stay in roaming `%APPDATA%\mob`, or move next to mobd in `%LOCALAPPDATA%`?
8. Is a per-agent desktop on an RDS host worth planning later, given the licensing cost?
