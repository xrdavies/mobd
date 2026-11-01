# Agent concepts and terminology

Status: draft COLLAB-1, 2026-11-02. This is the shared vocabulary for the Mob
repositories. The [collaboration design](agent-collaboration.md) is proposed,
not implemented. Existing API names remain compatible.

## Vocabulary

| Term | Meaning in Mob | Existing representation |
| --- | --- | --- |
| Agent framework | Software that runs an agent, manages model context and executes tools: Codex, Claude Code or Pi | Framework identifier such as `codex`, `claude`, `pi` |
| Runner | mobd's adapter to an agent framework | `internal/runner.Runner`; `session.runner` selects it |
| Model | The model used by the framework for a turn | `model`, together with `effort` where supported |
| Agent | A named, persistent collaborator with its own identity, context and work history | One existing `Session` |
| Session / conversation | The persisted context, configuration and history of that agent; the chat is its client view | `task.Session`, `/sessions/{id}` |
| Turn | One bounded invocation of a runner, from input to completion, failure or cancellation | `task.Task`, `/tasks/{id}`, `session.activeTaskId` |
| Message | Content from a user, an agent or mobd; a message need not start a turn | Existing chat messages plus proposed collaboration messages |
| Collaboration request | An attributable request for an answer or work, with its own outcome and reply chain; it can span multiple turns | Proposed request record, identified by `requestId` |
| Inbox | Durable messages awaiting delivery to an agent, independently of its active turn | Proposed mailbox records in the mobd store |
| Framework session handle | The framework's opaque context-resumption handle | `resumeId`; never the public agent address |
| Office / host | The mobd instance owning these agents and their runtime resources | Pairing `serverId` |
| Desktop | A GUI resource used by a turn: shared on macOS, per conversation in Linux session mode | `internal/desktop` reservation and lifecycle |

An agent and a session are **one-to-one in COLLAB-1**. Agent describes the actor;
session describes its persisted context. Do not create a second agent database,
invent an unrelated `agentId`, or make the client show two lists for the same
objects. The future ability to reset/fork context is outside this mapping.

An agent survives the end of a CLI process or a daemon restart. Its framework
is fixed by the existing session/runner compatibility rule; a model setting is
not an agent identity. A framework's native subagent is not automatically a Mob
agent: it needs an independently registered Mob session to be addressable here.

## Identity and names

- Within one mobd, an agent's canonical identifier is the full `sessionId`.
- Across the clients' saved offices, an agent reference is
  `{serverId, sessionId}`. Use the pairing server identity, not a client-local
  machine row ID, a hostname, a URL or a credential. Auth reset creates a new
  server identity; old references must not silently retarget to it.
- `Session.title` is the editable display name. Rename does not change identity;
  duplicate names are allowed and require disambiguation when selected.
- Display a short, unambiguous ID alongside the name. A short prefix is a lookup
  convenience; persisted references always contain the full ID. Do not use the
  list position or a turn ID as an agent number. A separate numeric numbering
  service is outside the first delivery.
- Preserve the name at sending time for history, while links resolve by ID.
  A deleted target becomes an unavailable reference, never a new same-name agent.

`@name` or `@short-id` selects an agent reference. Selecting a reference does not
itself send a prompt to that agent. A user can refer to someone's work without
asking for new work. A collaboration tool call is the attributable dispatch
action; merely printing `@name` in an agent response has no dispatch effect.

## Three independent lifecycles

| Lifecycle | What it answers | What it does not imply |
| --- | --- | --- |
| Agent/session | Which collaborator exists, its name/configuration, whether it is archived | An active process, or that all its work is complete |
| Turn | Whether this execution is queued, running, succeeded, failed or cancelled | Completion of every collaboration request involving the agent |
| Collaboration request | Whether this specific question/work item is open, answered, failed, cancelled or expired | That either participant's unrelated work should stop |

Use **Answered** for a request with a final result; it does not certify that the
result is correct or that the requesting user's overall goal is finished.
The requester may review the result and create a follow-up request.

Waiting for a collaborator is derived from open requests while no turn is
executing. It is not the current `Task.Waiting` flag (which means a running turn
is waiting for user input). It holds neither an execution slot nor an agent
lock. The agent can answer a clarification while its own request remains open.

## Compatibility names and client language

| Existing name | Meaning to document now | Wording for new client UI |
| --- | --- | --- |
| `settings.agents`, `settings.Agent` | Framework-level model, effort and permission settings | Agent frameworks |
| `settings.defaultAgent` | Default framework for a newly created agent/session | Default framework |
| `/health.agents` | Framework installation/readiness checks | Framework availability |
| `/health.runners`, `runner` | Adapter/framework identifiers and supported capabilities | Framework, with capability details where useful |
| `Session`, conversation, chat | The persistent agent and its conversation view | Agent name; Open conversation |
| `Task`, `/tasks`, task ID | An execution turn | Running / Stop current turn |

Keep existing JSON keys, routes, resume handles and saved client data working.
Apply clearer UI labels and comments when touching the relevant feature. A bulk
rename of public fields or a new API major version is not required for this
design. Do not reuse `/health.agents` as the new agent directory: that directory
projects real sessions and must expose session IDs.

The client can keep **Chats** / **New chat** as navigation language. The picker
on a new chat chooses an **Agent framework**; the `@` picker chooses an **Agent**.
“Stop current turn” and “Cancel collaboration” are separate actions.

## Ownership

mobd owns this vocabulary, identity rules and the collaboration contract.
Desktop, iOS and Android own the presentation and local cache implementations.
The website explains released behavior and uses the same terms. Client companion
plans reference this revision rather than redefining request states or IDs.

Current foundations: [session types](../internal/task/task.go),
[runner interface](../internal/runner/runner.go),
[framework settings](../internal/settings/settings.go),
[pairing identity](../internal/pairing/store.go), and
[paged history](conversation-history.md).
