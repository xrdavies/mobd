# Conversation history pagination and incremental sync

Status: draft

## Problem

`GET /sessions/{id}` currently returns the complete conversation on the first
open. `?since=<rev>` makes later refreshes incremental, but it does not bound
the first response. Long conversations therefore make the initial response,
JSON decoding, and client memory grow with the complete history.

The protocol needs two independent forms of incremental work:

1. history pages for older turns;
2. revision sync for changes to a conversation already held by a client.

The two cursors must not share a meaning. A history cursor identifies a place
in the ordered turn history. A revision identifies a state change in the
conversation.

## API draft

Keep the existing revision endpoint and add a page endpoint:

```text
GET /sessions/{id}/history?limit=50
GET /sessions/{id}/history?before=<opaque-cursor>&limit=50
```

The first request returns the newest page. `before` returns turns older than
the turn represented by the cursor. Pages are ordered oldest to newest for
the client, even when the store reads them backwards to find the newest page.

```json
{
  "session": {
    "id": "…",
    "title": "…",
    "status": "…",
    "messages": []
  },
  "tasks": [],
  "activities": [],
  "questions": [],
  "snapshotRev": 128,
  "before": "opaque-cursor",
  "hasMore": true
}
```

The page is divided by turn/task, not by individual messages or activities.
Each selected task carries all of its messages, steps and questions so the
existing client merge-by-task-ID logic remains valid. The cursor must be
opaque, include the session identity, and use a stable task order key rather
than a timestamp alone. The server should reject an invalid or foreign cursor
with `400`.

`GET /sessions/{id}?since=<rev>` keeps its current meaning. It returns the
current session metadata, every task whose `task.rev` is newer than `since`,
every queued or running task, and the complete data for those tasks. The
response still contains `rev` and `full`. The endpoint remains the authority
for advancing a client's revision baseline.

The first history page may establish the client's initial `syncRev` from
`snapshotRev`, because it is the client's first baseline. Later history page
requests must not advance `syncRev`: they contain only a range, not every
change since the client's previous revision. A subsequent `since` request
reconciles changes that happened while the user was paging.

## Store implementation

Add a `HistoryPage` store operation instead of calling `Session` and slicing
the result. It should:

1. take the store read lock and capture one consistent snapshot;
2. select at most `limit` task IDs from the session's stable order, before the
   cursor when present;
3. copy only those tasks, their activities, questions and messages;
4. return the session metadata, the snapshot revision and the next cursor.

The response must be ordered consistently and must not expose internal order
indexes. The current `SessionSince` behavior remains useful because a missed
sequence of revisions can be recovered by returning the current complete
state of each changed task; a per-revision event log is not required for
correctness.

Every session mutation must increment the session revision and assign that
revision to the affected task while holding the same consistency boundary as
the persisted snapshot. Turns are not deleted independently; a deleted
conversation continues to return `404`.

The first implementation can scan the session's task order and stop after the
requested page. If data shows that many other sessions make that scan costly,
add a per-session task-order index. Do not clone the full `Session.Messages`
slice before applying the page limit.

## Client contract

Clients maintain two independent pieces of state:

- `syncRev`: the last revision fully reconciled by a `since` response;
- `historyCursor`: the oldest page currently loaded in the timeline.

History pages merge by task ID. A revision response replaces every returned
task and its related messages, activities and questions, while retaining
unchanged tasks. A changed task may be older than the visible page; retain its
latest value in the local task map so a later history page cannot overwrite it
with stale data.

Live task SSE remains a low-latency transport. On reconnect, event cursor gaps,
or periodic refresh, clients call the revision endpoint and merge its result.
If a request fails, the client keeps the previous `syncRev` and retries.

## Compatibility and limits

Existing clients may continue to use the current full snapshot behavior until
they support the history endpoint. The server can advertise a
`sessionHistoryPaging` feature in `/health`; clients that do not see it fall
back to the existing request. A page limit should be capped server-side (the
initial target is 50 turns) so a client cannot request an unbounded response.

Pagination bounds the number of turns, but one individual turn can still be
large. If real data produces very large single replies, content chunking is a
separate follow-up; it should not be mixed into task-level history paging.

## Verification

- The newest page contains exactly the requested number of turns and has a
  stable cursor.
- Consecutive pages contain no duplicate or missing turns.
- Appending a new turn does not invalidate an older cursor.
- Updating an old turn appears in `since` even when that page is not loaded.
- Title-only changes advance `rev` without returning unrelated turns.
- Queued and running turns continue to appear in revision responses.
- Invalid, foreign and exhausted cursors return the documented result.
- A client can page, lose its SSE connection, reconcile with `since`, and
  keep one ordered timeline.
