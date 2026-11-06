# Configuration reference

Every mobd configuration setting, with its default value and environment variable.


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
| `filesDir` | `MOBD_FILES_DIR` | `~/mob-files` | Folder shared with paired devices for file transfers; unfinished uploads in its hidden `.uploads/` |
| `corsOrigins` | `MOBD_CORS_ORIGINS` | `http://localhost:1420,http://127.0.0.1:1420,tauri://localhost,http://tauri.localhost` | Comma-separated exact origins; a nonempty value replaces the defaults |
| `questionTimeout` | `MOBD_QUESTION_TIMEOUT` | `30m` | How long a turn waits for your answer to the agent's question or approval request |
| `runners` | `MOBD_RUNNERS` | `codex,claude` | Enabled runners |
| `codexMode` | `MOBD_CODEX_MODE` | `app-server` | `app-server` (approvals, steering, streaming) or `exec` |
| `claudeSettingSources` | `MOBD_CLAUDE_SETTING_SOURCES` | `project` | Claude settings loaded; `all` includes your personal `~/.claude` (with `project`, only its `env` and `apiKeyHelper` are used, for logging in) |
| `claudeBin` | `MOBD_CLAUDE_BIN` | `claude` | Claude Code executable |
| `claudeWorkdir` | `MOBD_CLAUDE_WORKDIR` | Codex workdir | Claude working directory |
| `piBin` | `MOBD_PI_BIN` | `pi` | Pi CLI (tested with 1.4.0; requires 1.4.x or later 1.x); uses `workdir` |

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

