# Windows (draft plan, not implemented)

> **Status: draft for review.** Nothing here is implemented. Today only macOS has a desktop
> backend; other systems compile `unsupported` (`internal/desktop/backend.go`,
> `backend_other.go`) and the API answers 501 for `/screen` and `/stream`. mobd does not even build
> for Windows yet (see [Portable core](linux.md#portable-core-phase-0-shared-with-windows)).
> Items marked *(to verify)* are OS behaviour we have not confirmed on real machines.
> The Linux plan is separate: [Linux](linux.md).

Windows keeps the macOS shape: a per-user mobd inside the logged-in user's interactive session,
driving that **single real desktop**, shared by all tasks and serialized by the FIFO desktop queue
(`maxRunning`, default 1). The HTTP API, the stream protocol and the `Backend` interface's meaning
do not change.

## Summary

| | Windows |
| --- | --- |
| Session model | per-user logon Scheduled Task (no Windows service: services run in session 0) |
| Desktop | the logged-in user's interactive desktop, one for all tasks |
| Capture | DXGI Desktop Duplication (Windows.Graphics.Capture later) |
| Encode | Media Foundation H.264 MFT (hardware, then Microsoft software MFT) |
| Input | `SendInput` (scan codes, `KEYEVENTF_UNICODE`) |
| Permissions | none beyond integrity level (UIPI) |
| Control socket | AF_UNIX socket file + ACL, `LockFileEx` (named pipe rejected, see below) |
| Packages | signed zip + PowerShell installer; MSI later |
| Order | after Linux phase 0 (shared [portable core](linux.md#portable-core-phase-0-shared-with-windows)) |

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
  `/usr/bin/tail`). Today `internal/service/service.go` fails unless `GOOS == darwin` and uses
  launchd; the Scheduled Task is one implementation of the shared `Manager` interface.
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

`mobd mcp` is its own process and calls `internal/desktop` directly (`hostDesktop`,
`internal/mcpserver/server.go`). On Windows it would hit the same UIPI and session limits as a
separate process, so it uses the shared `proxyBackend` that forwards desktop calls to
`POST /internal/tasks/{id}/desktop/{op}` on mobd serve, authenticated with the task token like
`ask` (`internal/mcpserver/ask.go`). mobd serve owns the only backend instance. Details in
[Portable core](linux.md#portable-core-phase-0-shared-with-windows).

MCP instructions and tool descriptions lose "office Mac", "Quartz", "pasteboard + Cmd+V" and
"bundle id" on Windows and tell agents to use `ctrl+c`/`ctrl+v`; `command` means Win and
`option` means Alt in hotkeys.

## Control socket

**Recommend AF_UNIX** (Windows 10 1803+, supported by Go's `net` package) over named pipes: the
code (`net.Listen("unix")`, `control.Call` dialing `unix`) stays unchanged, and the client side
stays standard library (named pipes would need `go-winio`). Per-OS pieces move behind
`control_unix.go` / `control_windows.go`:

- **Ownership/mode check:** replace the `Stat_t` uid + 0700 check (`internal/control/control.go`)
  with "the directory's owner is the current user SID and its DACL grants only the user (and
  SYSTEM)". Create `%LOCALAPPDATA%\mobd\run` with an explicit protected DACL
  (`SetNamedSecurityInfo`, using `golang.org/x/sys/windows`). AF_UNIX on Windows checks the socket
  file's ACL on connect, so the directory ACL is the access control.
- **Lock:** `LockFileEx` instead of `syscall.Flock`, for both control (`control.sock.lock`) and
  the pairing store (`internal/pairing/store.go`), behind one small `internal/filelock` package.
- **Stale socket:** AF_UNIX files on Windows are reparse points. `os.Lstat` does not report
  `ModeSocket` reliably *(to verify on current Go)*, so detect by dialing, then remove.
- Named pipes stay the fallback only if AF_UNIX proves unreliable (e.g. with some security
  products).

## Paths and runners

- **Paths:** everything in **`%LOCALAPPDATA%\mobd\`** (`config.json`, `service.json`,
  `mobd.log`, `run\`). Roaming `%APPDATA%` is wrong for a machine-bound daemon.
  `config.DefaultPath`, `control.DefaultPath` and the service `Dir` move to one `config.Dir()` per
  OS. `statePath`'s default (`os.UserConfigDir()` = `%APPDATA%\mob\state.json`,
  `internal/config/config.go`) is kept for consistency or moved; to decide.
- **Codex/Claude:** npm installs `codex.cmd`/`claude.cmd` shims. `exec.LookPath("codex")` finds
  them via `PATHEXT`, and Go can run `.cmd` files (through `cmd.exe`, with Go's argument escaping
  rules since 1.22). Long prompts are passed on stdin already (`internal/runner/codex/runner.go`),
  avoiding the `cmd.exe` 8191-char limit. `MCPEnv` and JSON args with quotes need testing through
  `.cmd` quoting *(to verify)*. Prefer resolving to the real `node.exe` + script, or the native
  binary where one is shipped, if quoting breaks.
- **PATH capture:** the Scheduled Task gets the user's environment from the registry, which
  includes npm's `%APPDATA%\npm`. Still record absolute paths at `start` as on macOS.
- **Process tree:** replace `Setpgid` + `syscall.Kill(-pid)` (`internal/runner/cli/cli.go`) with a
  **Job Object** per turn (`CreateJobObject`, `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, start
  suspended via `CREATE_SUSPENDED` and assign before resuming to avoid the race), and
  `TerminateJobObject` on cancel. `KILL_ON_JOB_CLOSE` also covers the daemon crash case that the
  `// shortcut:` comment in `cli.go` leaves open on Unix. Files: `cli/proc_unix.go`,
  `cli/proc_windows.go`.
- **Shells:** codex and claude choose their own shell (PowerShell/`cmd` on Windows; Git Bash for
  Claude Code). `runner/activity.go`'s `shellWrapper` regex only strips `zsh/bash/sh -lc`. Extend
  it for `powershell.exe -Command`/`pwsh -c`/`cmd /c`.
- **Signals:** `signal.NotifyContext(..., os.Interrupt, syscall.SIGTERM)` compiles on Windows.
  `schtasks /End` terminates the process without a signal, so `stop` should first ask the daemon
  to exit over the control socket (new `POST /shutdown`) for a graceful runner shutdown.

## Health

The macOS TCC checks (`accessibility`, `screenRecording` in `internal/api/health.go`) become
per-backend checks. Windows reports `input` (UIPI detail), `capture` (Desktop Duplication works),
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
internal/filelock/           flock / LockFileEx
internal/control/            control_unix.go, control_windows.go (ownership, ACL)
internal/runner/cli/         proc_unix.go (Setpgid), proc_windows.go (Job Object)
internal/service/            shared Manager + schtasks_windows.go
```

`CGO_ENABLED=0` builds keep working: without cgo, streaming is `unsupported` but input and
snapshots stay available where pure Go allows.

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

## Phases

Rough effort assumes one engineer familiar with the codebase.

| Phase | Scope | Effort |
| --- | --- | --- |
| **0. Portable core** | shared with Linux: see [Linux phase 0](linux.md#portable-core-phase-0-shared-with-windows). The Windows parts (`LockFileEx`, ACL, Job Objects, Scheduled Task) can trail the Linux work. | (in Linux phase 0) |
| **1. Windows** | DXGI duplication, MF encoder (hardware + software), SendInput, DPI, clipboard/windows/apps, WTS/power, Scheduled Task, AF_UNIX + ACL, firewall, signed zip + `install.ps1`, CI. | 4–6 weeks |
| **2. Hardening** | Windows.Graphics.Capture, cursor shape to clients, MSI, arm64, SYSTEM service + per-session helper for the lock screen. | as needed |

Windows comes after Linux unless most target office PCs run Windows; that is a market question
for the owner. On Windows capture, encode, input, the session model and signing are all new at
once, and the lock-screen limitation needs a product decision first.

## Risks

1. **Lock screen and UAC are unreachable** from the user session, so the live view cannot unlock
   the PC or answer UAC. Fixing it needs a SYSTEM service + session helper, a significant security
   surface.
2. **Encoder variability:** vendor MF MFT quirks and dynamic bitrate support. Probe each encoder at
   start and fall back to the software MFT.
3. **N editions** need the Media Feature Pack.
4. **`.cmd` shim quoting** for codex/claude arguments (TOML/JSON in `-c` values).
5. **Code signing and SmartScreen:** a paid certificate and reputation building before installs
   look trustworthy.
6. **CI coverage:** GPU encode and real capture cannot be fully tested on hosted runners.
7. **One shared desktop:** like macOS, tasks wait in the FIFO desktop queue; see
   [Why not one desktop per agent](#why-not-one-desktop-per-agent-on-windows).

## Open questions for the owner

1. Is "no lock screen / UAC in the live view; disable auto-lock on dedicated PCs" OK for v1, or is
   the SYSTEM service + session helper required?
2. Signing: OV, EV, or Azure Trusted Signing? Who owns the certificate?
3. Should mobd offer `uiAccess=true` (signed, in Program Files) to drive elevated windows?
4. Should the live view map a Mac client's Cmd key to Ctrl (client setting), or stay Win?
5. Paths: `%LOCALAPPDATA%\mobd` for mobd; should `statePath` stay in roaming `%APPDATA%\mob`?
6. Is a per-agent desktop on an RDS host worth planning later, given the licensing cost?
