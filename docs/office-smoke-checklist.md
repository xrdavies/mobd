# Office Mac smoke checklist

Verify the installed mobd, one configured agent runner, and the home client's live
view on the real office desktop.

For separate home/office networks, complete the [remote-access setup](remote-access.md)
and test from the actual home network. The release download domain distributes files; it is
not the task API address. See [security](security.md) for the current access controls.

## 1. Install and configure mobd

On a macOS 13+ Apple Silicon office Mac, as the normal logged-in user:

```bash
curl -fsSL https://mob-releases.0xd.app/mobd/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
mobd --version
mobd init
```

For an upgrade, finish active tasks and stop the old service first. See
[installation and upgrades](releases.md#4-安装和升级). Developers may instead use
`make build` and `./bin/mobd` from this repository.

Edit `~/.mobd/config.json` before starting:

- Set `workdir` to an existing absolute directory.
- Install and authenticate Codex or Claude Code as this user; set its executable
  path if it will not be found on the service's PATH. Enable the intended runners
  and choose `defaultRunner` ([runner configuration](runners.md)).
- `mobd init` sets `listen` to `0.0.0.0:8080` for direct LAN/VPN clients. Select a
  private interface to narrow the bind, or `127.0.0.1:8080` for access only through
  Tailscale Serve or SSH tunnels.
- Keep `maxRunning: 1` on a shared desktop.
- Set `codexPermissions` / `claudePermissions` to `approve` if provider approvals
  are required; the default is `bypass`. Mandatory pairing behavior is in [security](security.md).

## 2. Start and check readiness

1. Start mobd once. If Accessibility or Screen Recording is missing, mobd asks
   macOS to show the corresponding authorization prompt. Approve it for the
   app that launches mobd (Terminal/iTerm for a shell launch, or mobd under
   launchd). You can review the grants in System Settings → Privacy & Security;
   restart mobd after changing them.
2. Screen Sharing is not needed: mobd streams the desktop itself, with the
   Screen Recording and Accessibility grants above. For a dedicated office Mac, turn
   off automatic screen locking so snapshots keep working (the live view works at
   the lock screen).
3. Start the daemon and inspect readiness:

   ```bash
   mobd start
   mobd status
   # Pair first using mobd link and POST /pair (see docs/pairing.md).
   curl -fsS -H "Authorization: Bearer $MOBD_TOKEN" http://127.0.0.1:8080/health
   ```

   Adjust the health address if listening on a different interface or port.
   `ok: true` means the API is serving; inspect `checks` and `runners` for agent,
   macOS permissions and display state (`display`: locked or asleep). Use `mobd logs` to investigate failures.

`mobd mcp` is built into the same binary. mobd injects its configuration into each
agent turn; no separate MCP installation or global agent configuration is needed.

## 3. Submit a task through mobd

From the mob client, add the office Mac's mobd URL and start a new chat with an
available runner. Ask it to use mob-mcp to open TextEdit and type a unique smoke
marker without saving a file. Alternatively, submit a local request:

```bash
curl -fsS -H "Authorization: Bearer $MOBD_TOKEN" http://127.0.0.1:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Use mob-mcp to open TextEdit and type MOB smoke check in a new unsaved document. Do not save it."}'
```

Every request requires a paired device credential, including reads and streams.
Desktop/mobile integration must be completed before running the client checks. Use the returned task ID with `GET /tasks/{id}` and
`GET /tasks/{id}/events`, or watch the client's timeline.

**Pass:** the real desktop changes, tool activity is visible, and the task succeeds.
A text-only description without tool execution does not pass. Submit a follow-up
in the same session to verify conversation continuation. For approval mode, answer
any presented question/approval card and confirm the turn resumes.

## 4. Verify the home view

1. In the mob client, connect the live view. Its readout shows the transport
   (`WebRTC · LAN` or `Tailscale`; `WebSocket` through Funnel or an HTTPS gateway).
2. Watch the smoke task in the small preview, which only watches. Mouse and keyboard
   input must stay off until the user switches the large view to Control.
3. If testing Control mode, finish the agent task first: human takeover does not
   automatically pause the agent.

Without a client, `go run ./cmd/stream-probe -url … -token …` (add `-ws` for the
WebSocket) reports first-picture time, bitrate and frame rate.

## 5. Acceptance and service lifecycle

| Check | Criterion |
| --- | --- |
| Install | `mobd --version` matches the selected published release. |
| Readiness | Configured runner and permission checks are ready; `features` include `stream`. |
| Actuation | A task submitted through mobd visibly changes the office UI. |
| View | Home preview shows the same desktop with input disabled by default. |
| Persistence | After completed tasks, `mobd restart` retains conversations and configuration. |
| Upgrade | Stop, install and start preserves configuration and conversations. |

Use `mobd start --login` when login startup is desired;
`mobd stop` removes that registration.
