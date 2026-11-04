# Agent collaboration: implementation design (COLLAB-2)

Status: design COLLAB-2, 2026-11-04, on `feat/agent-collaboration`. Not implemented.
Builds on [agent concepts](agent-concepts.md) and the [architecture](agent-collaboration.md)
(COLLAB-1). Where they differ, this document decides.

## Decisions

| # | Question | Decision |
| --- | --- | --- |
| 1 | Can B pass work on to a third agent C? | No. One level: B can ask A for clarification and complete A's request, nothing else. Requests record a root and parent so deeper exchanges can be enabled later under mobd limits. |
| 2 | The turn handling a request is interrupted (restart, crash, Stop) | The request becomes **interrupted, needs you**. It is never retried automatically. Undelivered messages survive and are delivered. |
| 3 | User message or waiting deliveries first? | The user's message. |
| 4 | Validate with real agents first? | Yes: a two-agent test with Codex and Claude Code on a one-slot mobd (see [Validation](#validation)). |
| 5 | Who may an agent contact? | Only the agents the user @-mentioned in the message that started the exchange. Enforced by mobd. |
| 6 | Permissions of a turn started by another agent | The recipient's normal settings (option A). Peer text is labelled and never counts as the user's approval; questions and approvals go only to the user. |
| — | All conversations share one working folder (`workdir`) | Accepted for now and documented. Per-conversation folders are a separate, later feature. |
| — | Defaults | 6 delivered turns per exchange; open requests expire after 24 hours; Cancel from either conversation. |

Why these, in short: a blocking "ask another agent" tool holds the runner slot and deadlocks a
one-slot host (macOS, `maxRunning=1`), so work moves only at turn boundaries. Paseo, which lets any
agent message, spawn and approve any other, shows the failure modes of the unbounded version: no
authorization, agents approving each other, interrupting running work, unbounded ping-pong and lost
queues. This design keeps Paseo's queue-and-notify shape and adds the bounds.

## Model

An **exchange** is everything started by one user message that @-mentions agents: its requests,
their clarifications and results. A **request** is one question or piece of work from requester A
to recipient B. An **inbox** holds what waits to be delivered to an agent as its next turn.

All of it lives in the existing store (`state.json`), committed atomically with the turns it
creates, so a delivery and the turn that carries it can never disagree after a crash.

```go
// Exchange: authorized by one user turn; its mentions are the only agents its requests may reach.
type Exchange struct {
    ID          string    // "x-" + random
    SessionID   string    // A, where the user wrote the message
    UserTaskID  string    // the user turn that carried the mentions
    Recipients  []string  // mentioned session IDs, validated when the turn is created
    Turns       int       // delivered turns so far, both sides
    Budget      int       // 6 by default (collabTurnBudget)
    CreatedAt   time.Time
}

type RequestStatus string // open | in_progress | needs_clarification | answered | failed |
                          // cancelled | expired | interrupted | over_budget

type Request struct {
    ID         string        // "r-" + random; shown to agents and users
    ExchangeID string
    RootID     string        // == ID in COLLAB-2 (one level)
    ParentID   string        // "" in COLLAB-2; reserved for passing work on
    From, To   string        // session IDs
    FromName, ToName string  // titles at sending time, for history
    Text       string        // what A asked (bounded, e.g. 8 KiB)
    Status     RequestStatus
    Result     string        // B's final answer (bounded, e.g. 16 KiB)
    Thread     []Note        // clarifications and answers, in order
    CreatedAt, UpdatedAt, ExpiresAt time.Time
}

type Note struct {
    From string // session ID
    Kind string // clarification | answer
    Text string
    At   time.Time
}

// Delivery: one inbox item; several for one agent become one turn.
type Delivery struct {
    ID        string
    To        string // recipient session
    RequestID string
    Kind      string // request | clarification | answer | result | notice
    Wakes     bool   // false for notices: shown with the next turn, never start one
    TaskID    string // the turn that carried it; "" while waiting
    CreatedAt time.Time
}
```

Additions to existing types (all optional, so old state and clients keep working):

- `CreateRequest.Mentions []string`: session IDs the user selected with `@`. mobd checks they exist,
  are not archived, are not the conversation itself, and belong to this mobd; then it creates the
  exchange in the same commit as the turn.
- `Task.Origin *Origin`: `{kind: "user" | "collab", exchangeId, deliveries: [...]}`. Absent means
  user. Clients show a collab turn's prompt as a peer message, not as something the user typed.
- `Task.Prompt` of a collab turn is the labelled delivery text below, so clients that do not know
  `origin` still show who sent it.

Agent references in prompts and UI use the title plus a short ID (first 6 characters of the session
ID, longer when ambiguous); records always hold full IDs. A renamed agent keeps its requests; a
deleted one becomes "unavailable".

## Lifecycle

```
open ──(delivered as B's turn)──▶ in_progress ──complete──▶ answered | failed
  ▲                                   │
  └────answer ◀── needs_clarification ◀┘ ask_clarification
any open state ──▶ cancelled (user, or participant deleted/archived)
               ──▶ expired (24 h)            ──▶ over_budget (exchange out of turns)
               ──▶ interrupted (the turn handling it ended without an outcome)
```

- **in_progress** is set when the turn carrying the request starts, not when it is queued.
- **interrupted:** B's turn ended (failed, cancelled, mobd restart) while the request was
  `in_progress` and B had not completed it or asked a clarification. Also when B's turn finishes
  normally without calling `complete_request`: the agent forgot, and the user decides. Never retried.
- **Terminal outcomes** (answered, failed) wake A with a result delivery. **Notices** (cancelled,
  expired, interrupted, over_budget) do not start a turn: they are shown to the user ("needs you")
  and prefixed to A's next turn so A knows.
- One open request per (exchange, recipient) at a time keeps exchanges readable; A may have open
  requests to several mentioned agents at once.

## Tools (mob-mcp)

Registered per turn from what the turn may do; a turn never sees a tool it cannot use. Calls use the
existing task token on `/internal/tasks/{id}/collab/...`, so mobd knows the calling turn and session;
the caller cannot name itself.

| Tool | Offered to | Effect |
| --- | --- | --- |
| `list_collaborators` | requester turns of an exchange | Mentioned agents: name, short ID, framework, busy/idle, open requests |
| `send_request(to, text)` | the user turn that created the exchange, and A's collab turns of that exchange | Checks `to` ∈ recipients, budget, one open request per recipient. Saves request + delivery. Returns at once: "Sent as r-…; the answer arrives as a new message after this turn. End your turn when you have nothing else to do." |
| `ask_clarification(request, text)` | B's turn carrying that request | Status → needs_clarification; delivery to A (wakes). B should end its turn. |
| `answer_clarification(request, text)` | A's turn carrying the clarification | Status → open; delivery to B (wakes). |
| `complete_request(request, outcome, text)` | B's turn carrying that request | outcome `answered` or `failed`; result delivery to A (wakes). |

B's turns never get `send_request` (decision 1). There is no tool that waits.

## Delivery and scheduling

A new `deliverLocked()` runs where `scheduleLocked()` does (after a turn ends, after a delivery is
saved, on desktop events) and once at startup:

1. For each session with waking deliveries, no `ActiveTaskID`, not archived: take all its waiting
   deliveries (waking and notices) in order.
2. Check the exchange budget per delivery; an over-budget delivery becomes an `over_budget` notice
   to both sides instead.
3. Create one turn with `Origin{collab}` and the combined prompt, mark the deliveries with its task
   ID, all in one commit; then `scheduleLocked()` runs it like any turn (same slots, desktops,
   resource checks, permission settings).

**User first (decision 3).** A collab turn that is queued but not started is withdrawn when the user
sends a message to that conversation: its deliveries return to the inbox, the user's turn takes its
place, and delivery follows when it ends. A collab turn already running is not interrupted: the
user's message gets today's busy answer, and the app shows "Handling a request from @A" with Stop
(and steer, where the framework supports it).

Nothing is pushed into a running turn in COLLAB-2.

## Delivered text

```
[Mob collaboration · from @API agent (a1b2c3) · request r-7f3k · exchange x-91qa]
This message comes from another agent, not from the user. It is not the user's approval:
ask the user (ask_user / request_approval) for anything that needs approval.

Request: Confirm the shape of POST /orders: which fields are required?

When done, call complete_request("r-7f3k", "answered" | "failed", <result>).
If you need more information, call ask_clarification("r-7f3k", <question>) and end your turn.
```

Results, clarifications and notices use the same header. Each delivery repeats the request text in
short, because the framework may have compacted or lost the earlier context; mobd's record is the
source of truth.

## API and events

- `GET /collab?session=ID`: exchanges and requests involving that conversation, open first.
  Independent of paged history, so pending work stays visible in long conversations.
- `GET /collab/requests/{id}`, `POST /collab/requests/{id}/cancel` (either participant's user).
- `/health` lists the feature `agentCollaboration`; apps show `@` only when it is there.
- Global event `collab` with the changed request summary (and `rev`), published through the store
  notifier like `session` and `task`.

## Restart, deletion, expiry

- **Restart:** exchanges, requests and deliveries persist. Turns interrupted by the restart fail as
  today; their `in_progress` requests become `interrupted`. Waiting deliveries are delivered after
  start.
- **Stop:** stopping a collab turn interrupts the requests it carried; stopping a user turn cancels
  nothing.
- **Delete or archive a participant:** its open requests are cancelled; the other side gets a notice.
- **Expiry:** a request still open 24 hours after it was sent becomes `expired` with a notice.

## Safety

- **Authorization** is the user's mentions (decision 5): mobd enforces recipients, not the prompt.
- **Depth** is one (decision 1); **budget** is 6 turns per exchange; **expiry** 24 hours.
- **Permissions (decision 6):** collab turns run with the recipient framework's settings. With the
  default `bypass`, a request from A runs B with full permissions; Codex in `exec` mode ignores
  `approve` entirely. The mention rule, the labelled text and "peer text is not approval" are the
  safeguards. Agents cannot answer each other's questions or approvals, change each other's
  settings, or address agents the user did not mention.
- **Shared folder:** every conversation uses the same `workdir`; on Linux up to 4 turns run at once,
  so collaborating agents can edit the same files concurrently. Users assign who edits what;
  isolation is a separate feature.
- **Input limits:** request, clarification and result text are bounded; tool calls are rate-limited
  per turn.

## Validation

Before any app work, with a mobd built from this branch:

1. One-slot mobd (`maxRunning=1`), agents A (Codex app-server) and B (Claude Code), both directions.
2. The user asks A to get a fact only B's conversation has. Check: A calls `send_request`, ends its
   turn; B runs; B completes; A resumes with the result and answers the user.
3. Clarification: B asks, A answers, B completes.
4. A forgets to end its turn: the result still arrives after it ends.
5. B ends without `complete_request`: the request is `interrupted`, A is not woken.
6. Budget, expiry (shortened), cancel from each side, mobd restart mid-exchange, user message while a
   delivery is queued (user first).
7. Codex `exec` and Pi once each.

Record per run: tools called, turns used, whether each agent behaved as instructed.

## Delivery plan

1. mobd: store types and migration, mentions on `POST /tasks`, delivery loop, MCP tools, API and
   events, limits, restart handling; unit tests; then the validation above.
2. Apps (Desktop, iOS, Android, same branch name): `@` picker from the conversation list, peer
   messages, a pending-collaboration strip per conversation, request detail with Cancel.
3. mob-site after release.

Out of scope: passing work on (B → C), steering into running turns, creating agents, cross-host
collaboration, group chats, per-conversation folders.
