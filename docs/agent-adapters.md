# Agent adapters: one shape for every agent framework

Status: mobd implemented 2026-11-22 (phases 1 and 3); apps in progress. ACP status checked 2026-11-21 (see [ACP status](#acp-status)).

This document has three parts: [the feature](#part-1-feature), [the decisions](#part-2-decisions) and
[the design](#part-3-design).

## Part 1: Feature

### Problem

mobd runs three agent frameworks: Claude Code, Codex and Pi. Each has its own adapter (`internal/runner/claude`,
`codex`, `pi`) behind the `Runner` interface (`Info`, `Check`, `Start`), so running a turn and its events are
already uniform. Everything around it is not:

- **Configuration is per agent.** `config.json` has flat keys for each one: `claudeBin`, `codexBin`, `piBin`,
  `claudeWorkdir`, `claudeSettingSources`, `codexMode`. Every new agent adds keys to the config code, `/config`,
  its validation and the docs.
- **The apps know each agent by name.** Desktop builds `${name}Bin`, shows a Claude-only "settings it loads" row and
  has names and colors for `claude`, `codex` and `pi`; the phone apps have the same names in their strings.
  Adding an agent means a mobd release **and** three app releases.
- **Approvals and Mob's tools are wired differently in each adapter.** Codex asks inside its app-server, Claude
  through Mob's MCP tool `approve_tool`; each passes the Mob MCP server in its own way.
- **Every agent costs a Go adapter.** OpenCode, DeepSeek Harness (`dsh`) and others each need translating their
  own output format, which is the most expensive part to write and maintain.

### What the feature does

**One adapter shape in mobd, one way to show agents in the apps.**

1. Each adapter declares, besides what it can do (today's `Info`), the **options it takes**: key, label, type,
   choices, whether a change needs a restart, a help line. The apps draw an agent's settings from that list,
   with no agent-specific code.
2. `config.json` gets **one section per agent** (`"agents": {"claude": {...}}`); the flat keys keep working.
3. Approvals, Mob's tools and readiness become **named parts of the adapter**, so a new adapter fills in clear
   slots.
4. A **generic ACP adapter** runs any agent that speaks the
   [Agent Client Protocol](https://agentclientprotocol.com). OpenCode and DeepSeek Harness come in this way, as
   configuration, without Go code of their own.
5. Claude Code, Codex and Pi keep **mobd's own adapters**.

### User stories

1. I add OpenCode on the office computer and turn it on in Mob; it appears in every app with its settings, without
   an app update.
2. I change an agent's program, folder or its own options (Claude's settings source, Codex's mode) from Desktop;
   every agent's page looks the same.
3. The phone shows a new agent with its name and state, and a neutral avatar until the app knows its color.
4. An agent that can't steer, resume or report cost says so, and the apps hide what it doesn't support.

### Scope

In: the adapter structure in mobd, per-agent options and config sections, the generic ACP adapter, OpenCode and
DeepSeek Harness as its first agents, the API that publishes options, the apps drawing agents generically.

Out: rewriting the Claude Code, Codex and Pi adapters on ACP; ACP's remote (HTTP) agents; mobd acting as an ACP
agent itself for editors (possible later); installing agents for the user (mobd keeps showing the install command).

### Platforms

macOS and Linux, wherever mobd runs. ACP agents run as local processes like the native ones, with the same
environment, folder and (on Linux) the conversation's desktop.

## Part 2: Decisions

### D1. Keep the Runner interface; widen it, don't replace it

- **Options:** (a) replace the adapters with one protocol for all; (b) keep `Runner` and add what is per agent
  today (options, approvals, tools, readiness).
- **Choice:** (b).
- **Reason:** running turns and translating events already work through `Runner`; the per-agent parts are
  around it. A replacement would risk features the native adapters have today (Codex steering and in-process
  approvals, Claude's login handling).

### D2. Each adapter declares its options; the apps draw them

- **Options:** (a) keep per-agent keys and teach the apps each agent; (b) a schema per adapter, rendered generically.
- **Choice:** (b). Types: `bool`, `choice`, `path`, `text`, `number`. Each option says whether it needs a restart.
- **Reason:** the apps stop knowing agents, so a new agent needs no app release. The same list drives `/config`
  validation, which moves into the adapter.
- Names, labels and capabilities already come from mobd. Avatar colors stay in the apps for agents they know;
  others get a neutral avatar with the label's initial.

### D3. Config moves to per-agent sections, old keys still read

- **Choice:** `config.json` gets `"agents": {name: {bin, workdir, ...options}}`. The flat keys are read as before
  and map onto the sections (`claudeBin` → `agents.claude.bin`, `codexMode` → `agents.codex.mode`). A key set both
  ways is an error naming both. Environment variables (`MOBD_CLAUDE_BIN` …) keep working. Writes from the apps go to
  the sections; mobd never rewrites a hand-written flat key on its own.
- **Reason:** no config breaks on upgrade, and new agents never add top-level keys.

### D4. ACP for new agents; native adapters for Claude Code, Codex and Pi

- **Options:** (a) a Go adapter per agent, as today; (b) every agent through ACP, including Claude Code and Codex
  via their ACP bridges; (c) native adapters for the three we have, ACP for new ones.
- **Choice:** (c).
- **Reason:**
  - ACP v1 is stable and widely adopted (see [ACP status](#acp-status)); OpenCode and DeepSeek Harness speak it
    natively, so one adapter covers them and the agents after them.
  - Claude Code and Codex speak ACP only through separate bridge packages. mobd's native adapters do more than the
    bridges give us today: Codex app-server steering and approvals, Claude's settings-source and login handling,
    exact usage and cost. A bridge would be one more package to install and keep in step.
  - Pi has no native ACP mode (a PR adding one was declined); community `pi-acp` exists. mobd already speaks
    Pi's RPC directly.
  - Revisit per agent if its ACP support becomes native and complete; moving one is a config change plus deleting
    its Go adapter.

| Agent | Adapter | Why |
| --- | --- | --- |
| Claude Code | mobd's own (`internal/runner/claude`) | Only via a bridge (`claude-agent-acp`); native handles settings sources, logins, cost |
| Codex | mobd's own (`internal/runner/codex`) | Only via a bridge (`codex-acp`); native app-server gives steering and in-process approvals |
| Pi | mobd's own (`internal/runner/pi`) | No native ACP; mobd speaks its RPC |
| OpenCode | ACP (`opencode acp`) | Native ACP |
| DeepSeek Harness (`dsh`) | ACP (`dsh --profile acp`) | Native ACP (release candidate) |
| Others in the ACP Registry (Gemini CLI, Copilot CLI, goose, Qwen Code …) | ACP, when someone adds them | Configuration only; not tested or promised in this phase |

### D5. mobd is an ACP client with agent-side tools only

- **Choice:** mobd advertises no client file-system or terminal capabilities (`fs/*`, `terminal/*`). Agents use
  their own tools in the conversation's folder, as the native adapters do.
- **Reason:** ACP sets no limits on where client-side file writes go (noted in a security review of agent
  protocols); offering them would make mobd responsible for sandboxing. Keeping tools on the agent side matches
  how mobd runs every agent today.

### D6. Mob's tools and approvals go through the same paths for every adapter

- **Choice:** Mob's MCP server (`mobd mcp`: ask the user, screen tools) is given to each agent through the
  adapter's `Tools` slot; ACP agents get it in `session/new` as a stdio MCP server, which every ACP agent must
  accept. Approvals ("Ask before commands and edits") become `Questions` like today: ACP's
  `session/request_permission` maps to an approval question; "allow always" and "reject always" are not offered,
  because settings.json is the one place permissions are set.
- **Reason:** one path for the apps, whatever the agent.

### D7. Capabilities are honest, not forced equal

- **Choice:** each adapter reports what it supports; ACP agents report from the `initialize` handshake
  (`loadSession`, `resume`, image prompts) and from what they send (cost only if `usage_update` carries it).
  Steering is not in ACP v1, so ACP agents report `steer: false`.
- **Reason:** the apps already hide what an agent doesn't support; pretending would break turns.

### D8. Target ACP v1 now; handle v2 at the handshake

- **Choice:** the adapter negotiates the protocol version in `initialize` and supports v1. v2 (draft) changes
  are isolated in the adapter (e.g. `session/load` → `session/resume`, renamed `config_option_update` fields)
  and added when v2 is stable and an agent we support needs it.
- **Reason:** v1 is what OpenCode and DeepSeek Harness ship; v2 is still a draft.

### D9. An ACP agent's model and effort come from the agent

- **Choice:** ACP agents publish their settings as config options (`model`, reasoning level …). mobd maps the
  options it recognizes to settings.json's model and effort, so the composer's pickers work as for any agent, and
  sets them per turn with `session/set_config_option`. Other config options appear as the agent's options (D2).
- **Reason:** no model lists to maintain for agents mobd doesn't know.

## Part 3: Design

### ACP status

Checked 2026-11-21 from the spec site and secondary sources; recheck before implementing.

| Item | Status |
| --- | --- |
| Protocol | JSON-RPC 2.0 over stdio for local agents; HTTP/WebSocket for remote agents is work in progress. Started by Zed (August 2025), Apache 2.0 |
| Versions | v1 stable; v2 first draft July 2026. Version agreed in `initialize` |
| Adoption | Zed (headline of Zed 1.0, April 2026), JetBrains, Neovim, Emacs, 100+ other clients; ACP Registry (with JetBrains, since January 2026) lists about 42 agents |
| SDKs | Official: TypeScript, Python, Rust, Kotlin, Java. Go: `github.com/coder/acp-go-sdk` |
| Native agents | OpenCode (`opencode acp`), DeepSeek Harness (`dsh --profile acp`, RC), Gemini CLI (`gemini --acp`), GitHub Copilot CLI, others |
| Bridged agents | Claude Code (`@agentclientprotocol/claude-agent-acp`), Codex (`@agentclientprotocol/codex-acp`), Pi (community `pi-acp`) |
| Known gaps | No steering of a running turn in v1; cost optional in `usage_update`; no limits on client-side file writes (D5) |

What mobd uses, by method:

| mobd needs | ACP v1 |
| --- | --- |
| Start a conversation in its folder | `session/new` with `cwd` (absolute) and `mcpServers` |
| Continue a conversation | `session/load` (capability `loadSession`) or `session/resume` (capability `sessionCapabilities.resume`) with the stored session ID |
| Send a turn | `session/prompt` (text; images when the agent accepts them) |
| Steps, replies, plans, diffs | `session/update` notifications (`agent_message_chunk`, tool calls and their updates, plans) |
| Approvals | `session/request_permission` (agent → mobd) |
| Model, effort, other settings | config options; `session/set_config_option`; `config_option_update` |
| Usage | `usage_update` (tokens in context; cost when sent) |
| Stop a turn | `session/cancel` |

### The adapter

`internal/runner` keeps `Runner` (`Info`, `Check`, `Start`) and the optional `Steerer`, and adds:

```go
// Option is one setting an adapter takes in config.json (agents.<name>.<key>).
type Option struct {
	Key, Label, Help string
	Type             string   // bool, choice, path, text, number
	Choices          []Choice // for choice
	Default          any
	Restart          bool     // applies after mobd restarts
}

// Configurable adapters declare their options and check values before they are saved.
type Configurable interface {
	Options() []Option
	Validate(values map[string]any) map[string]string // key → message
}
```

- Every adapter has `bin` (path) and `workdir` (path, default: the shared `workdir`); adapters add their own
  (`claude`: `settingSources`; `codex`: `mode`).
- Adapters receive their section (`config.Agent`) instead of the whole `config.Config`.
- `Check` returns the readiness the apps already show (installed, version, signed in, how to fix); unchanged in
  shape, documented as part of the adapter.
- Approvals and Mob's tools are passed through `Options` as today (`Ask`, `MCPEnv`, `Permissions`); each adapter
  documents how it uses them. The ACP adapter maps them as in D6.

### The ACP adapter

`internal/runner/acp`, one instance per configured ACP agent.

- **Process:** one agent process per conversation turn, like the CLI adapters; started with `bin` and `args`
  in the conversation's folder and environment (desktop, cgroup on Linux). A later version may keep a process per
  conversation if agents start slowly.
- **Turn:** `initialize` (protocol v1, client capabilities: none for `fs` and `terminal`) → `session/load` or
  `session/resume` with the stored ID, else `session/new` (the new ID becomes the task's resume ID) →
  `session/set_config_option` for model and effort if they changed → `session/prompt` → translate
  `session/update` into task events until the prompt response → stop the process.
- **Events:** message chunks → messages; tool calls → activities (kind from the tool call's kind: execute →
  command, edit → file_change, read/search → tool, fetch → web_search); plans → plan; `usage_update` → usage.
- **Approvals:** `session/request_permission` → an approval question; the answer picks the agent's `allow_once` or
  `reject_once` option. With permissions "bypass", mobd answers allow at once and records the step.
- **Cancel:** `session/cancel`, then the process is stopped after a grace period.
- **Readiness:** `Check` runs `initialize` without a session (with a timeout) and reports the agent's name and
  version from it; a missing program or a failed handshake is "not working" with the error.
- **Capabilities:** from `initialize`: `resume` if `loadSession` or resume is offered; `images` if image prompts
  are; `cost` once the agent has sent a cost; `steer` false.

### config.json

```json
{
  "runners": ["claude", "codex", "opencode"],
  "agents": {
    "claude":   { "bin": "/Users/ana/.local/bin/claude", "settingSources": "project" },
    "codex":    { "mode": "app-server" },
    "opencode": { "protocol": "acp", "label": "OpenCode", "bin": "opencode", "args": ["acp"] },
    "dsh":      { "protocol": "acp", "label": "DeepSeek Harness", "bin": "dsh", "args": ["--profile", "acp"] }
  }
}
```

- `protocol: "acp"` makes an agent use the ACP adapter; its keys are `label`, `bin`, `args`, `workdir` (default:
  `workdir`), `env` and `install`. Model and effort come from the agent per session (D9); its other session config
  options are not shown yet.
- mobd ships presets for OpenCode and DeepSeek Harness (label, default command, install command), so turning them
  on needs no section at all.
- Flat keys (`claudeBin` …) are still read (D3); setting a key both ways is an error. A section key written through
  `PUT /config` replaces its flat key in the file (the diff shows both).

### API

- `/health` `agents[]` (and `/settings` `installed[]`, the same list) gain `protocol` (`native` or `acp`) and
  `options`: `{key, label, help?, type, choices?, value, restart, overridden?}`, `type` one of `bool`, `choice`,
  `path`, `text`, `number`; `overridden` when a flag or environment variable sets it on the computer (read-only).
  Agents not turned on are listed too, so the apps can show their options before enabling them.
- `GET /config` / `PUT /config` take `agents.<name>.<key>` keys; validation comes from the adapter. Remote
  maintenance's editable rules apply per option (`args` and `env` of ACP agents are local-only, like `mcpCommand`:
  they choose what program runs).
- New feature flag `agentOptions`; apps without it keep today's per-agent screens.

### Apps

- Desktop, iOS and Android draw an agent's page from `options`: one row per option by type, restart-needed options
  saved through `/config` with Restart to apply, the others through `/settings`.
- The agent list comes from mobd, including agents the app has never heard of. Avatars: known colors, otherwise a
  neutral color with the label's initial.
- The hard-coded `${name}Bin` and the Claude-only row go away once the mobd they talk to has `agentOptions`.

### Phases

1. mobd: `Option`/`Configurable`, per-agent config sections with flat-key mapping, `options` in the API, the three
   native adapters declaring their options. Feature `agentOptions`.
2. Desktop: agent pages from `options`; then iOS and Android.
3. mobd: the ACP adapter (`coder/acp-go-sdk` or a small client of our own), presets for OpenCode and DeepSeek
   Harness, tests against a fake ACP agent.
4. Real-agent checks: OpenCode and DeepSeek Harness on macOS and Linux (approvals, resume, model and effort,
   Mob's MCP tools, cancel).

### Acceptance

1. A config with only flat keys behaves exactly as before; the same config written as sections does too.
2. Desktop and the phone apps show every agent's options from mobd, including Claude's settings source and Codex's
   mode, without agent names in their code.
3. With OpenCode installed, `"runners": [..., "opencode"]` makes it usable in every app: turns, steps, approvals,
   resume, model choice, Mob's tools, cancel.
4. DeepSeek Harness works the same way.
5. An ACP agent reports no steering, and the apps hide it.
6. mobd never offers ACP client file or terminal methods.

### Sources

- [Agent Client Protocol](https://agentclientprotocol.com/overview/introduction) ·
  [Session setup (v1)](https://agentclientprotocol.com/protocol/v1/session-setup) ·
  [Schema (v1)](https://agentclientprotocol.com/protocol/v1/schema) ·
  [Migrating from v1](https://agentclientprotocol.com/protocol/v2/migration) ·
  [acp-go-sdk](https://pkg.go.dev/github.com/coder/acp-go-sdk)
- [Which coding agents support ACP](https://agentrq.com/blog/which-coding-agents-support-acp-agent-client-protocol) ·
  [OpenHands: ACP agents](https://docs.openhands.dev/openhands/usage/agent-canvas/acp-agents) ·
  [ACP explained (morphllm)](https://www.morphllm.com/agent-client-protocol)
- [DeepSeek Harness acp-app](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/bundle/acp-app/README.md) ·
  [Paperclip #17549: DeepSeek Harness over ACP](https://github.com/paperclipai/paperclip/issues/17549)
- [Pi: Supporting ACP (discussion #4444)](https://github.com/earendil-works/pi/discussions/4444)
- [AgentRFC: security of agent protocols](https://arxiv.org/pdf/2603.23801)
