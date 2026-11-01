# Agent communication and collaboration

Status: draft COLLAB-1, 2026-11-02. Not implemented or released. This document
owns the proposed shared contract; routes, fields and tools below are design
names, not available API claims. Read [agent concepts](agent-concepts.md) first.

## Outcome and first delivery

A user in agent A's conversation can select existing agent B by name or short
ID, ask A to collaborate with it, observe the exchange, and receive A's continued
work after B replies. B retains its own conversation and framework. Desktop,
iOS and Android see the same durable collaboration, including after reconnect.

The first delivery supports existing sessions on **one mobd**, cross-framework
messages, requests, clarification/answer exchanges, final results, cancellation
and bounded recovery. Each dispatched turn has one primary collaboration
context. A user turn may originate several independent requests to selected
peers; waiting for one does not lock the conversation.

Automatic agent creation, delegation trees, cross-host routing, group chat,
shared transcripts, automatic code merging and live injection into busy turns
are later work. Keep the existing runner and desktop scheduling contracts.
Use an in-process service and the existing store; no new broker or external
agent protocol is required for this same-host scope.

## Current implementation and extension points

| Foundation | Current behavior | Required extension |
| --- | --- | --- |
| [Session and Task](../internal/task/task.go) | Stable session, framework resume ID, one active turn | Collaboration records and provenance, without a duplicate agent entity |
| [CreateTurnWith](../internal/task/store.go) | Rejects a busy session; appends a user message | Atomic assignment of inbox messages to a new turn with agent-origin metadata |
| [Scheduler and runTask](../internal/api/server.go) | Reserves slots/desktops; releases resources after runner completion | Admit mailbox work at turn boundaries and reconcile durable completion before further scheduling |
| [MCP registration](../internal/mcpserver/server.go) | Desktop and user question/approval tools | Agent discovery, messaging, replying and bounded request reads |
| [Task authentication](../internal/api/internal_desktop.go) | Validates a per-turn token | Derive sender session and authorized request context; do not accept caller-supplied sender identity |
| [Runner options](../internal/api/media.go) | Injects MCP environment and instructions | Per-turn identity, selected peers and collaboration instructions |
| [History](../internal/task/history.go) | Turn pages and bounded revision sync | Provenance on materialized turns plus an independent collaboration snapshot |
| [Persistence](../internal/task/persistence.go) | Atomic JSON snapshot; interrupted turns fail on restart | Durable message/request transitions, idempotent assignment and conservative recovery |

`Runner.Start` executes one turn. Codex app-server and Pi expose `Steerer`;
Claude and Codex exec do not provide that common capability. COLLAB-1 delivers
at turn boundaries for all frameworks. Existing user steering remains available;
structured agent references must survive that path too.

## Addressing and user intent

The client resolves selected mentions to `{serverId, sessionId, displayName}`;
`displayName` is a label snapshot. mobd validates the IDs against its own server
identity and live sessions. No machine URL or bearer token enters the prompt.

Name search may be fuzzy for suggestions, but dispatch always uses a full ID.
Duplicate names or ambiguous ID prefixes require selection; never pick the first
match. Titles containing spaces or non-Latin characters work through the picker.
Ordinary text, code blocks, email addresses and quoted `@` strings are not parsed
as dispatch commands. A selection remains valid after a rename and invalid after
deletion; there is no name-based fallback.

Add optional structured `agentRefs` to both `POST /tasks` and the supported user
steer input. This supplies reference context to the current agent; the server
does not broadcast the user's entire prompt. A selects the appropriate peers
and sends focused messages through tools. Clients retain references in drafts
and preserve them on failed submission. The composer can use reference chips
alongside text; no cross-platform character-offset protocol is needed initially.

An intentional reference permits scoped communication with that peer for this
user instruction. The admitted turn records the selected peer IDs. A request
records its two participants, and its continuation turns can reply within that
request. Listing agents does not grant permission to contact every listed agent.
Adding a new peer requires a user reference; the first delivery has no automatic
expansion into a delegation tree. Reference-only instructions remain reference
only: tools must follow the user's actual request, not treat selection as a command.

## Records and delivery semantics

Store three related records within the existing atomic state boundary:

| Record | Required information |
| --- | --- |
| Message | `messageId`, `kind`, sender/recipient session IDs, sender turn ID, optional `requestId` and `inReplyTo`, bounded body, creation time, per-inbox sequence, delivery state and assigned turn ID |
| Request | `requestId`, requester/responder session IDs, originating turn ID, optional prior-request link, root budget ID, outcome state/reason, current revision, pending question ID, latest progress/result references, automatic-continuation flag, deadline and remaining exchange budget |
| Assignment | Message-to-turn linkage and primary request context, committed with turn creation; one message has at most one execution assignment |

Assignment can be fields on the records rather than a separate database table.
Display name snapshots are optional metadata, never addresses. The sender's
identity is derived from its authenticated running turn, and bound to the
message before acknowledgement.

Message delivery states are `queued`, `assigned`, `handled`, `failed`, `cancelled`,
`expired`.
`queued` means durable receipt. `assigned` means durably attached to a turn;
consult that turn for queued/desktop-starting/running status. `handled` means the
bound turn finished normally, not that the model understood it or the request
was answered. A failed/cancelled assigned turn makes that delivery failed/cancelled.

The request outcome is independent:

| Request state | Meaning |
| --- | --- |
| `open` | Waiting for initial handling, clarification, progress or a final result |
| `answered` | A valid final result was committed for this request |
| `needs_attention` | Execution was interrupted, produced no result or explicit wait, or exhausted its exchange budget; automatic continuation is paused |
| `failed` | It cannot proceed, for example its target was deleted or required resume context is unavailable |
| `cancelled` | Explicitly ended by the user/requester; automatic continuation is disabled |
| `expired` | Its deadline passed; automatic continuation is disabled |

The client derives “Queued”, “Processing”, “Waiting for clarification” and
“Waiting for collaborator” from request, question and assignment data. An open
request alone must not make the current turn appear to be running.

`answered`, `failed`, `cancelled` and `expired` are terminal for that request ID.
Retry after `needs_attention` is an explicit action, preserves the prior attempt,
and requires reconciling the original execution; it is never a resend loop.
A follow-up after a terminal outcome creates a new request referring to the old
one. If created automatically from a continuation, it inherits the root budget
and deadline; a fresh request ID cannot reset either. Only a new explicit user
instruction can authorize a fresh budget. A transport retry is different: it
retrieves the original operation receipt.

### Idempotency and atomicity

Every mutating tool operation carries an `idempotencyKey`. Scope it to the sender
session, retain it with the operation, and compare the normalized payload. A
repeat with the same payload returns the same IDs/receipt; a different payload
with the same key conflicts. Persist before acknowledging. After a lost response,
the caller reuses that key, including if its execution turn was subsequently
cancelled. An expired task token remains rejected; a new authenticated turn in
the same authorized session, or an authenticated client, can retrieve the saved
receipt. Retrieval never authorizes a new mutation from the expired turn.

Create the request, first message and continuation intent in one store commit.
Assign a queued message and create its target turn in one commit. On completion,
save the outcome/result, reply delivery and affected revisions atomically. Persist
results before emitting SSE; event delivery never owns the only copy.

Do not promise exactly-once external execution. A crash between a framework side
effect and its durable acknowledgement can leave an uncertain outcome. Record
that uncertainty and pause; do not run the same action again to guess its result.

## Tools and internal routes

All tools use the existing task token path, not a paired client's bearer token.
Suggested internal routes are under `/internal/tasks/{id}/collaboration/` and
validate that the authenticated task is still admitted for the requested action.

| Tool | Proposed inputs | Result / behavior |
| --- | --- | --- |
| `list_agents` | `query?`, `cursor?`, `limit?` | Session IDs, names, framework, archive/busy state; bounded metadata, no transcripts or resume handles |
| `send_agent_message` | `targetSessionId`, `body`, `expectReply`, `idempotencyKey` | Persist a notice or request and immediately return IDs and receipt; validates selected peer scope |
| `reply_agent_request` | `requestId`, `mode`, `body`, `inReplyTo?`, `idempotencyKey` | Add progress, a question, an answer or a final result in an authorized request |
| `get_agent_request` | `requestId`, `cursor?`, `limit?` | Bounded request state and message history, for recovery or necessary detail; no blocking wait |

Reply `mode` is `progress`, `question`, `answer` or `result`. Both participants
may ask/answer a clarification; only the responder may submit `result`. A question
and its answer use exact message IDs. Initially allow one unanswered question per
request; trying another returns the pending question, avoiding ambiguous answers.
An answer closes that question, not the overall request. Progress does not imply
that the sender yielded or the recipient must respond.

Reject self-addressed requests. The daemon derives any automatic follow-up's
prior-request link and root budget from the admitted continuation, so the model
cannot evade the limit by omitting a correlation field.

Wake policy:

- A request, clarification question, clarification answer or final result can
  schedule the recipient at its next eligible turn boundary.
- A notice or progress update is visible immediately to clients and is included
  in bounded context on the recipient's next naturally scheduled turn; it does
  not independently start a model run. This avoids acknowledgement loops.
- A result creates one durable continuation for the requester if that request
  still permits it. The continuation itself expects no automatic reply to B.

There is no blocking `wait_agent` tool. Repeated polling to simulate waiting is
outside the tool contract. Receipt status cannot be interpreted as work completion.

## Turn-boundary scheduling

1. Accept incoming messages into the session's inbox even while it has an active
   turn. Do not eagerly create a second `Task` or occupy `ActiveTaskID`.
2. When the session is eligible, under the scheduler/store synchronization rules,
   select a bounded input and atomically create/associate one turn. Recheck busy,
   archive, cancellation and request deadline at assignment time.
3. Run that turn with the session's existing runner, settings and `resumeId`.
   Format the input with its verified source and request context. Transporting it
   through a framework's user-input channel does not make it a human instruction
   in Mob's stored history or grant it higher instruction priority.
4. After the runner and owned child work have ended, reconcile delivery and
   request outcomes durably, then release its slot/desktop and schedule again.
   A failed store commit prevents further automatic dispatch for that context;
   do not lose the only result by treating a log line as recovery.

Initially process one primary collaboration item/request per turn. Collect
bounded informational notices with it. Within an agent, preserve FIFO among
ordinary requests; replies that unblock an existing exchange precede unrelated
new requests. User input already accepted for the session takes precedence over
pending automatic work. Use bounded admission into the existing global queue so
one agent cannot pre-create an arbitrarily long queue of turns. Desktop resource
limits and `desktopStarting` retain their current meanings.

### Yielding and resuming with one slot

Shared desktop mode normally has `maxRunning: 1`. The requester must not occupy
that slot inside a synchronous wait for its peer. After sending it may do
independent work; when it needs the answer, it ends the current turn normally.
The request remains open in mobd. Only actual runner/process completion releases
the slot; setting a waiting flag or returning from the MCP call is insufficient.

```mermaid
sequenceDiagram
    participant A as Agent A
    participant D as mobd
    participant B as Agent B
    A->>D: Send request R
    D-->>A: Durable receipt
    A->>D: End current turn normally
    Note over D: Release A slot; R remains open
    D->>B: Resume B with request R
    B->>D: Submit final result R and finish turn
    D->>A: Resume A with result R
```

Waiting is request-level state. A remains eligible to answer B's clarification;
otherwise a one-slot exchange would still deadlock. A successful turn without a
final result is a valid wait only if mobd has a recorded unanswered question or
outgoing request explaining that wait. If the responder simply stops without
either a result or a recorded wait, mark the request `needs_attention` instead
of guessing its last unrelated assistant message was the result.

For requests handled by B, COLLAB-1 supports clarification within R; automatic
delegation from that handler to a third agent is deferred. Reject such nested
request creation with an actionable tool response. Multiple independent user-
originated requests remain supported. This avoids introducing an implicit
dependency graph before cancellation and cycle semantics are implemented.

If a framework keeps working after being told to yield, display the actual busy
turn and queue reason. The daemon does not pretend that the slot is free, spin
up B beyond the limit, or kill/replay A to manufacture a handoff. Validate normal
yield behavior for every supported runner; apply the request deadline and user
Stop controls if it cannot progress. Low-latency steering can be added later
behind a capability; an unavailable steer must not become an implicit interrupt.

### Completion races and user control

A final-result tool call records a proposed result. Publish it as `answered`
and queue the requester continuation only after that handling turn finishes
normally; the tool receipt describes it as recorded/pending finalization. This
prevents A from consuming a “done” result while B's owned work still runs. If the
turn fails or is interrupted, preserve the proposed result for inspection and
use `needs_attention`. It is not an automatically retryable success.

Cancel, expiry, finalization and continuation assignment share an atomic check.
If cancellation/expiry wins, a late result is retained as late information and
cannot reopen the request or schedule A. If finalization wins, a later cancel of
that already answered request does not retroactively undo it; pending/running
continuation work remains separately stoppable.

“Stop current turn” retains its current process-cancellation behavior. For a
stopped turn, disable automatic continuation of the collaboration contexts it
owns until an explicit user continuation. Cancelling request R only cancels an
assigned turn whose primary work is R, never an unrelated user turn in B. Keep
completed messages and show partial/late results with their state. Archiving a
participant suspends new assignments and automatic continuations; pending work
requires explicit resumption after unarchive. Deletion fails open requests and
leaves an unavailable reference in the surviving participant's history.

User follow-up messages enter the existing session context. Automatic result
delivery is a new input with provenance, not replay of the user's old prompt.
Switching chats, closing an app or losing SSE does not stop daemon work.

## History, synchronization and proposed client API

Advertise `agentCollaboration` in `/health.features` only after this contract is
implemented. Keep `apiVersion: 3` and existing route/field meanings. Unknown
features disable collaboration controls; plain chats continue to work.

| Proposed paired-client surface | Purpose |
| --- | --- |
| `GET /agents?query=...&cursor=...&limit=...` | Agent/session directory, distinct from framework readiness |
| `POST /tasks`, supported user `/tasks/{id}/steer` with `agentRefs` | Submit reference context with a user instruction |
| `GET /sessions/{id}/collaboration` | Paginated collaboration summaries, pending inbox and durable changes |
| `GET /collaboration/requests/{id}` | Request detail and bounded message pages |
| `POST /collaboration/requests/{id}/cancel` | Idempotent cancellation of this request |
| `POST /collaboration/requests/{id}/continuation/pause` | Suppress automatic result delivery to the requester; stop only a turn assigned to that continuation, without changing an already answered outcome |
| `POST /collaboration/requests/{id}/resume` | Explicit resumption of eligible paused work with expected revision; never replay an uncertain turn blindly |

The exact wire schema is to be finalized with contract tests before coding
clients; its semantics are fixed here. Reads return stable IDs, revisions and
opaque cursors. Paginated snapshots share a snapshot revision; completing all
pages establishes `collaborationRev`. Bounded `since` changes include tombstones;
a gap/ahead revision/overflow returns `reset` and triggers a fresh snapshot.
Cursor and revision scopes include server and session/request. Ordinary history's
`syncRev` remains separate. Failed reads never advance either baseline.

Use global SSE only to invalidate/refresh the affected session's collaboration
view. Its payload need not contain bodies. After reconnect, foregrounding or an
event gap, bounded HTTP reads recover state; correctness cannot depend on replay
of a live notification. These routes use the same paired authentication as chat.

Existing history pages are whole turns. Pending inbox entries stay in the
collaboration view, not fabricated turns. When messages are materialized into
turns, add source metadata and links to request/message IDs. Preserve compatible
role fields with an explicit `origin` discriminator; rendering must use origin
to distinguish a peer from the human. Update the owning turn/session revision
when its collaboration card changes. References can point to older turns without
making clients fetch the full transcript. Do not pass agent input through the
current `AddUserMessage` steering path without its provenance extension.

Keep unanswered requests visible independently of the loaded history range.
Clients persist pending summaries and detail pages separately from their bounded
turn cache. Read-only caches never schedule work on launch. Clear local copies
and machine removal affect only the device; server cancellation is explicit.

## Persistence, recovery and bounds

Extend the state snapshot atomically with mailbox/request/assignment records;
do not add a separately written JSON mailbox that can diverge from turn creation.
Build in-memory indexes by recipient and request rather than scanning all chat
messages for each dispatch. Only successful durable commits produce receipts.

Recovery policy:

| State at restart | Action |
| --- | --- |
| Message durably queued, no assigned turn | Remains visible; may be assigned after startup reconciliation if active, authorized and unexpired |
| Assigned turn queued/running at restart | Preserve current interrupted-turn failure policy; request needs attention, no automatic rerun |
| Successful handling turn and final result committed | Reconstruct any missing continuation idempotently from durable state; never generate a second result |
| Result continuation already assigned, then interrupted | Needs explicit user recovery; an `answered` request remains answered, its delivery failed |
| Archived, stopped, cancelled, expired or deleted context | Respect its suppression state; no automatic wake |

Reuse the existing supervisor requirement for terminating orphan framework
processes before resuming interrupted contexts. Do not claim that a JSON recovery
routine can undo an external action or stop a process surviving a daemon crash.

Proposed initial bounds, centralized in mobd: text at most 16 KiB per message;
at most 50 unassigned messages per recipient; at most 16 open requests originated
by one agent; at most 32 automatic processing/continuation turns per root request
chain; request and notice deadline 24 hours. Return a structured capacity error before accepting
excess work. Reserve admission for replies/results to already accepted requests
at both endpoints, within the inbox bound, before admitting a new request. Keep
one next-response reservation per open exchange; coalesce pending progress for
that request. Unrelated work cannot consume its reserved capacity. The exchange budget moves
with assignments, survives restart and pauses at `needs_attention` when depleted.
Expiry never extends merely because progress or transport retries arrive.

Bound pages to 50 records and approximately 256 KiB, truncating display summaries
with explicit detail references. Use artifact/file references for large content;
do not copy transcripts or binary data into messages. Informational notices can
expire without ever starting a turn; expose that expiry to the sender.

Pending work cannot be evicted as disposable chat cache. Retain idempotency and
assignment evidence while its request/message record exists. Preserve terminal
records with the related conversation history, compact indexes at startup, and
measure growth. The existing full JSON rewrite remains a server scaling limit:
benchmark long histories plus simultaneous inbox writes before release. A future
incremental store must migrate turn and collaboration records together; client
paging alone does not fix server write amplification.

## Instructions, permissions and workspace ownership

Inject concise Mob instructions through existing `Options.Instructions`: the
current session identity, selected peers, primary request context, how to send a
focused request, how to reply/finalize, and when to end the turn while waiting.
On resumed turns include only the bounded relevant exchange and current state.
Never label a peer message as a new system instruction or human approval.

The current instruction asks for approval before sending messages externally.
The implementation must explicitly distinguish user-authorized internal Mob
collaboration from email/third-party messages, so every clarification does not
ask the user again. Existing consequential-action approvals remain in force.
An agent cannot approve another agent's pending permission request. Receiving
a message does not widen the recipient's framework permissions or authorize it
to send to new peers. Task token checks bind API identity; they are not a sandbox
against an agent already able to read the same OS user's files or run shell code.

Agents on the same host may share a working directory. Assign file ownership for
parallel edits; isolated worktrees require a separate per-session workspace
design because current working-directory configuration is runner-level. No
automatic merge or file isolation is promised here. Desktop reservations stay
authoritative even when messages can flow between many agents.

## Rollout, ownership and acceptance

mobd owns the vocabulary, record/state rules, tool and HTTP contracts, prompt
injection and scheduler. Desktop, iOS and Android own reference selection,
timeline/request cards, draft preservation, navigation and their local caches.
mob-site owns release wording and eventual guides. No relay changes are required
for same-mobd collaboration; client reconnection continues over its existing path.

Implementation order:

1. Define typed records, provenance and atomic store operations. Test deduplication,
   failure to save, cancellation races and restart reconstruction.
2. Add task-authenticated tools, admitted peer scopes and turn-boundary scheduling
   using fake runners. Lock the wire schemas and feature flag with contract tests.
3. Implement the three clients against identical fixtures, including old-server
   fallback and independent collaboration/history revisions.
4. Exercise installed frameworks and long histories before enabling the feature
   in a release; only then publish user-facing website claims.

Required acceptance scenarios:

- Name/ID lookup: duplicate names, rename after selection, ambiguous prefix,
  archived/deleted target, quoted mentions, wrong server identity and stale drafts.
- One slot: A requests B, ends normally, B asks A a clarification, A answers,
  B finalizes and A continues; no occupied-slot wait or session-level lock.
- Busy B: receive while it executes unrelated user work; no second active turn,
  interrupt, wrong result capture or cancellation of unrelated work.
- Cross-framework matrix: Codex app-server, Codex exec, Claude and Pi resume and
  preserve sender identity; no requirement for native framework subagents/steer.
- Lost receipts and concurrent sends: one durable operation/assignment, stable
  ordering, bounded backlog and capacity reserved for finishing accepted requests.
- Completion races: final result before turn exit, failure after proposed result,
  cancel/expire versus assignment/finalization, late reply and budget exhaustion.
- Restart at receipt, assignment, framework execution, result commit and result
  continuation boundaries; no blind replay of uncertain external effects.
- User Stop, archive/unarchive, delete, new user instructions, and permission
  prompts while collaboration messages arrive.
- All clients disconnected, then reconnect from different devices; no duplicate
  runs and equal request state. Mobile suspension is not daemon cancellation.
- Long history: an old request remains visible with newest-page-only chat,
  bounded detail/context reads, stable scroll anchors, bounded reset recovery,
  and measured state-file latency/memory under concurrent collaboration.

This draft adds no running tools, API routes, feature flag, storage migration,
new permissions or published feature claim. The checks above are future
implementation acceptance criteria, not results of this documentation change.
