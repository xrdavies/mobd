# Runners

A **runner** connects mobd to an agent CLI. Codex, Claude Code and Pi are supported.
Each runner translates its provider's output into shared events used by the API,
store and App.

Related: [API reference](mobd-v3.md) and [architecture](architecture.md)
(desktop tools via `mobd mcp`).

Install mobd with the [published installer](releases.md#4-安装和升级), then install
and authenticate the agent CLI separately as the same office user. The archive
contains mobd and its documentation; install the agent CLI separately.
Set executable paths and working directories in `~/.mobd/config.json`, then
`mobd restart` after configuration changes. `/health` reports each runner's availability.

## Contract (`internal/runner`)

```go
type Runner interface {
	Info() Info                       // static: name, label, models, efforts, images, resume, cost
	Check(ctx context.Context) Status // installed? version? (shown on /health)
	Start(ctx context.Context, t *task.Task, opts Options, emit EmitFunc) error
}
```

- `Start` runs **one turn** and blocks until it ends. Cancelling `ctx` must stop the agent and its children and return `context.Canceled`.
- The prompt is `t.Prompt`; `t.Model` / `t.Effort` are already validated against `Info()`; `opts.Images` are files to attach; `opts.SaveImage` stores screenshots from tool results.
- `t.ResumeID` is empty for the first turn of a conversation. Otherwise the runner continues that agent conversation.
- A **Registry** holds the enabled runners and the default (`MOBD_DEFAULT_RUNNER`, default `codex`).

### Events a runner emits

| Event | Fields | Meaning |
| --- | --- | --- |
| `resume` | `resumeId` | The agent's opaque handle for continuing this conversation. Emitted once per turn; mobd stores it on the session. |
| `message` | `messageId`, `message` | A finished assistant reply (upserted by ID). |
| `activity` | `activity` | A step: tool call, command, reasoning, plan, file change, web search, notice. Upserted by `id`; `running` → `completed`/`failed`. |
| `usage` | `usage` | Tokens for the turn, plus `costUsd` if the agent reports a price. |
| `log` | `message` | Debug line (memory-only, shown in Settings → Log). |

Returning an error fails the turn with that message.

### Sessions are pinned to a runner

A conversation's `resumeId` only means something to the runner that created it. The first turn records `session.runner`; later turns use the same runner, and asking for another one returns **409** ("start a new chat to switch").

### Shared pieces

- `runner/cli`: runs an agent CLI for one turn (process group, kill on cancel, draining output, line reading). A runner supplies the command line and a `Parser` (`Line`, `Finish`).
- `runner` helpers: `ToolTitle` ("Click (412, 230)"), `CommandTitle`, `SplitMCPName` (`mcp__mob-mcp__click` → `mob-mcp`, `click`), so every agent's steps look the same in the App.
- `runner/runnertest`: the **contract suite**. Each runner's test gives fake-agent scripts in its native format (reply, failure, hang) and the suite checks: info, missing binary, resume/message/step events, prompt delivery, resume and model passed to the agent, failure message, cancel.

## Asking the user (`ask_user`, `request_approval`)

`mobd mcp` offers two tools next to the desktop tools, for every runner:

- `ask_user(question, options?, allow_text?)` returns the user's answer.
- `request_approval(action, details?)` returns approved or denied (with an optional note). Unanswered means **denied**.

How a call travels:

1. When mobd dispatches a turn it creates a per-task secret, writes it to a 0600 file in a new 0700 directory under the data directory (removed when the turn ends), and passes `MOB_API`, `MOB_TASK_ID` and `MOB_TASK_TOKEN_FILE` to the runner as `Options.MCPEnv`. The runner hands them to the mob-mcp server's environment. The secret itself never goes into `MCPEnv`: agent CLIs only take an MCP server's environment on their command line, which `ps` shows and mobd logs. `mobd mcp` still accepts `MOB_TASK_TOKEN` from older mobd.
2. The tool calls `POST /internal/tasks/{id}/ask` on mobd (token in `X-Mob-Task-Token`). mobd stores a **question** on the turn, marks the turn and session `waiting`, and publishes a `question` event.
3. The App shows a card; `POST /tasks/{id}/questions/{qid}/answer` with `{answer}` or `{approved, answer?}` settles it and the blocked tool call returns.
4. The question becomes `expired` after `MOBD_QUESTION_TIMEOUT` (default 30m), `cancelled` if the turn is cancelled or the agent drops the call, and `expired` on a mobd restart. The turn keeps the desktop while it waits.

Runners also receive `Options.Instructions` (when to ask, which actions need approval) and `Options.ToolTimeout` (question timeout + 1 minute) so the agent's MCP call is not cut off while the user decides.

| Runner | Instructions | Long tool calls | mob-mcp tools allowed without prompts |
| --- | --- | --- | --- |
| `codex` | `-c developer_instructions=…` | `mcp_servers.mob-mcp.tool_timeout_sec` | `mcp_servers.mob-mcp.default_tools_approval_mode="approve"` |
| `claude` | `--append-system-prompt …` | `MCP_TOOL_TIMEOUT` (ms) | `--permission-mode bypassPermissions`, or `--allowedTools mcp__mob-mcp,…` in approve mode |
| `pi` | `--append-system-prompt …` | MCP server `timeout` (seconds) | Bundled MOB extension allows desktop/ask tools; `approve` gates other mutations |

For Codex, `MOBD_CODEX_PERMISSIONS=approve` needs app-server mode, where it is a hard gate (below); mobd refuses to start with `exec` and `approve`, because `codex exec` cannot ask before running a command. In `exec` mode the only gate is the instruction, sent to every runner, to call `request_approval` before risky actions.

**Claude Code hard gate** (`MOBD_CLAUDE_PERMISSIONS=approve`): Claude runs in `manual` permission mode with `--permission-prompt-tool mcp__mob-mcp__approve_tool`. Any tool call Claude would normally ask about (shell commands, edits outside its working directory, …) becomes an approval card "Allow the agent to use Bash?" with the tool input; deny or no answer blocks it. Desktop tools, ask tools, reading and searching are pre-allowed. Claude's own rules still apply: simple writes inside its working directory are allowed without asking.

## Codex app-server mode (default)

`MOBD_CODEX_MODE=app-server` (default; `exec` is the fallback) runs `codex app-server`, Codex's JSON-RPC server over stdio (one message per line, no `jsonrpc` field), one process per turn: `initialize` → `initialized` → `thread/start` or `thread/resume` (cwd, `sandbox: danger-full-access`, approval policy, `developerInstructions`, model) → `turn/start` (text and `localImage` inputs, model, effort). Notifications map like the exec JSONL: `item/started`/`item/completed` → steps and replies, `thread/tokenUsage/updated` → turn usage (thread total minus what preceded the turn), `turn/completed` ends the turn and closes stdin so the server exits. mob-mcp is configured with the same `-c mcp_servers.mob-mcp.*` overrides as exec. Resume IDs are Codex thread IDs in both modes.

App-server capabilities:

- **Hard approvals.** `MOBD_CODEX_PERMISSIONS=approve` sets `approvalPolicy: "untrusted"`. Codex's `item/commandExecution/requestApproval` and `item/fileChange/requestApproval` requests become approval cards ("Allow the agent to run …?") through `Options.Ask`, answered `accept`/`decline`; no answer means decline. Default `bypass` uses `never`.
- **Interrupt.** Cancelling sends `turn/interrupt` and gives Codex 3 s to stop before the process group is killed.
- **Steering.** `POST /tasks/{id}/steer {prompt}` sends `turn/steer` to the running turn (runners with `steer: true` in `/health`); the message is saved as a user message in the conversation.
- **Streaming.** `item/agentMessage/delta` updates the reply in the timeline while Codex writes it (throttled to ~400 ms).

The protocol is marked experimental by Codex; `codex app-server generate-ts --out <dir>` prints the types for the installed version.

## Adding a runner

1. `internal/runner/<name>/`: `Info`, `Check` (usually `cli.Version`), `Start` building the command line and a parser for the agent's JSON stream.
2. Map its output to the events above. Desktop tools arrive from `mobd mcp`; use `SplitMCPName` and `ToolTitle`.
3. `contract_test.go` with `runnertest.Run`, plus parser tests against **real captured output**.
4. Register it in `cmd/mobd/main.go` and add its config (`MOBD_<NAME>_BIN`, models).

## Runners

| Name | Command | Resume | Efforts | Images | Cost |
| --- | --- | --- | --- | --- | --- |
| `codex` | `codex app-server` (default) or `codex exec --json … [resume <id>] -` | ✓ | minimal, low, medium, high, xhigh | ✓ | — |
| `claude` | `claude -p --input-format stream-json --output-format stream-json --verbose --strict-mcp-config --mcp-config … [--resume <id>]` (prompt and images as a stream-json user message) | ✓ | low, medium, high, xhigh, max | ✓ | ✓ |
| `pi` | `pi --mode rpc … [--session <file>]` | ✓ | off, minimal, low, medium, high, xhigh, max (model-dependent) | ✓ | ✓ |

Enabled runners come from `MOBD_RUNNERS` (default `codex,claude`); missing CLIs show as unavailable on `/health` and in the App.

- **Defaults:** machine settings changed in the apps come first ([mobd-v3](mobd-v3.md#machine-and-conversation-settings)). Otherwise each runner in `/health` reports `defaultModel` and `defaultEffort`, what the agent uses when a turn names none, so apps can show "Default (xhigh)". Codex reads them from `$CODEX_HOME/config.toml` (`model`, `model_reasoning_effort`, and the selected `profile`'s). Claude reads `model` and `effortLevel` from the settings files that `claudeSettingSources` lets it load (with the default `project`, personal `~/.claude/settings.json` does not count), and `ANTHROPIC_MODEL`. They are omitted when not set, which means the agent's built-in default; Pi reports none yet. They are read on every `/health`, so editing the agent's config shows without a restart.
- **Codex:** models offered to clients come from `MOBD_CODEX_MODELS` (`MOBD_MODELS` is also accepted, with lower priority).
- **Claude Code:** `MOBD_CLAUDE_BIN`, `MOBD_CLAUDE_WORKDIR` (default: Codex's), `MOBD_CLAUDE_MODELS` (default `opus,sonnet,haiku`), `MOBD_CLAUDE_PERMISSIONS` (`bypass` default, or `approve`). Only mob's MCP server is loaded, and `MOBD_CLAUDE_SETTING_SOURCES=project` (default) keeps your personal `~/.claude` hooks and settings out of office runs; `all` loads them. Output mapping: `system/init` → resume, `text` → message, `thinking` → reasoning, `tool_use`/`tool_result` → steps (`Bash` → command, `TodoWrite` → plan, `Edit`/`Write` → file change, `mcp__mob-mcp__*` → desktop tools), `result` → usage with `costUsd`.

## Pi

Tested with **`@earendil-works/pi-coding-agent@1.4.0`**. The runner accepts 1.4.x
and later 1.x versions. Older Pi releases have different MCP/lifecycle behavior;
they are rejected by the readiness check and before starting a task.

Install and authenticate Pi as the office user, then add `pi` to `runners`:

```sh
npm install -g --ignore-scripts @earendil-works/pi-coding-agent@1.4.0
pi  # use /login and /model to configure the provider
```

```json
{
  "runners": ["codex", "claude", "pi"],
  "defaultRunner": "codex",
  "piBin": "pi",
  "piPermissions": "approve",
  "piModels": []
}
```

Merge these fields into `~/.mobd/config.json`, then restart mobd. `piModels`
advertises model IDs to the clients; use exact `provider/model` IDs to avoid
ambiguous provider matches. Empty uses Pi's configured model. Image input and
thinking levels depend on the selected model. No new API version is required.

The runner uses `workdir` and one RPC process per turn. Sessions live under
`dataDir/pi/sessions`; the absolute session file is the conversation's opaque
`resumeId`. Keep those files alongside the mobd state. A missing resume file
fails the turn rather than silently starting a new conversation.

The embedded `bridge.mjs` uses Pi's exported `createMcpExtension` with an empty
configuration loader and registers only `mobd mcp`. Personal/project MCP files
cannot override the task's server. Task tokens travel in the process environment,
not CLI arguments or a permanent config file. MCP timeout follows mobd's question
timeout. Desktop tools use the existing authenticated desktop proxy, including
Linux session environments, cgroups, OOM settings and process tracking.

Discovered extensions, skills, prompt templates and themes are disabled; project
trust-gated configuration is ignored (`--no-approve`). Pi's normal provider auth,
model configuration and global settings remain available. Providers that require
a custom extension are not supported by this initial integration. The MOB extension
and its required desktop/ask tools must pass a readiness handshake before a prompt.

`piPermissions: "approve"` gates `bash`, `edit`, `write` and unknown tools through
Pi's `tool_call` hook and RPC confirmation requests. MOB shows them using the
existing approval cards; rejection, expiry, cancellation or missing approval
handler blocks the call. Read/search and MOB desktop/ask tools are pre-allowed,
matching the existing desktop policy. The injected instructions still require
`request_approval` for consequential desktop actions. This is not a sandbox.
Pi's own `--approve` flag controls project trust, not tool approval.

RPC `message_update`/`message_end` produce streamed/final replies; thinking and tool
events produce activities; image results become artifacts. Usage is the sum of
assistant responses in the current MOB turn (including retry attempts), with Pi's
estimated cost; separate compaction-model/tool usage is not included. `agent_settled`,
not `agent_end`, ends a run after retries/recovery. Steering uses `steer`; cancellation
clears queued input and sends `abort`, with a 3-second process-group kill fallback.

Local checks, including a real Pi process against a deterministic local model
server and the real `mobd mcp` implementation (no paid inference or host desktop input):

```sh
go test -race ./internal/runner/pi ./internal/config ./internal/service
MOBD_PI_TEST_BIN="$(command -v pi)" go test -race -run TestRealPi -v ./internal/runner/pi
```

The smoke test checks image input/output, MCP question routing, approval allow/deny,
bypass mode, MCP config isolation, conversation resume, steering and cancellation
while waiting for approval. Protocol tests also
cover streaming, usage, malformed/failed output, retries, cancellation and steering.
Wire references: [RPC](https://pi.dev/docs/latest/rpc),
[events](https://pi.dev/docs/latest/json), [extensions](https://pi.dev/docs/latest/extensions).
