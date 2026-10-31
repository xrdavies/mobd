# Conversation history: paging, bounded sync and local copies

Status: in progress on `feat/conversation-history`

## Problem

`GET /sessions/{id}` returns the whole conversation on first open, and `?since=<rev>` returns it
again whenever `since` is 0 or ahead of mobd's revision. Long conversations make the first
response, JSON decoding and client memory grow with the complete history, and clients keep no
copy across restarts, so every launch starts over.

The fix has two parts:

1. mobd serves history in pages of whole turns, and changes since a revision with a bound.
2. Clients keep a local copy of each conversation (see the client plans in mob, mob-ios and
   mob-android), show it at once, and sync it in the background.

## Turn order: `seq`

Each turn gets `seq`, its 1-based position in the conversation, assigned when it is created and
saved with it. `Session.turns` is the number of turns, so the newest has `seq == turns`. Turns are
only removed with their conversation, so `seq` never changes and has no gaps. State written before
`seq` is numbered on load in the existing turn order. Conversations saved with `rev: 0` get
`rev: 1` on load, so `rev` is never 0 for a conversation with turns.

Clients sort by `seq` and use it to tell whether a turn lies inside the range they hold.

## API

```text
GET /sessions/{id}/history?limit=20                  newest page
GET /sessions/{id}/history?before=<cursor>&limit=20  the page before the cursor
GET /sessions/{id}/history?since=<rev>&limit=50      changes after a revision
```

`limit` defaults to 20 and must be 1–50. `before` and `since` together are a `400`.
A missing conversation is a `404`.

### Pages

A page is a range of whole turns, oldest first, each with all its messages, steps and questions.
It holds at most `limit` turns and stops early once it reaches about 2 MB of JSON, but always
contains at least one turn.

```json
{
  "session": { "id": "…", "title": "…", "turns": 120, "rev": 731, "messages": [] },
  "tasks": [ { "id": "…", "seq": 101, "rev": 640 } ],
  "activities": [],
  "questions": [],
  "rev": 731,
  "hasMore": true,
  "before": "opaque-cursor"
}
```

`session.messages` holds only the messages of the returned turns. `before` is present when
`hasMore` is true and names the oldest returned turn; pass it back for the next older page.
Cursors are opaque, bound to the conversation (another conversation's cursor is a `400`), and
stay valid when new turns are added.

`rev` is the revision the page was read at, in the same snapshot. The newest page sets a client's
first sync baseline. Older pages never move the baseline: they cover a range, not every change.

### Changes since a revision

`since` returns every turn changed after that revision, plus every queued or running turn, with
all their messages, steps and questions, and the current session metadata:

```json
{ "session": { … }, "tasks": [], "activities": [], "questions": [], "rev": 740, "reset": false }
```

The answer is bounded like a page. When it cannot be answered within the bound, or `since` is 0
or ahead of mobd's revision (state restored from an older copy), mobd answers `"reset": true`
with the session metadata and no turns: the client drops its copy of that conversation's turns
and loads the newest page again. A title-only change returns the session with no turns.

`GET /sessions/{id}` and its `?since=` keep their current meaning for clients without this
feature. mobd advertises `sessionHistory` in `/health.features`.

## Store

`HistoryPage` and `HistorySince` read one snapshot under the store's read lock. A page walks the
turn order from the newest end and stops after `limit` turns of the conversation, without copying
the full message list. Every change to a conversation already raises its revision and the changed
turn's `rev` in the same commit, which is what `since` relies on.

The scan over the global turn order is linear in all turns. If it shows up with many large
conversations, add a per-conversation index.

## Client contract (summary)

Clients keep, per conversation: the turns they hold (by task ID, with `seq` and `rev`), the
oldest `seq` held, whether older turns exist, and `syncRev`.

- Merge by task ID and keep the copy with the higher `rev`.
- Show only turns from the oldest held `seq` up. A changed turn older than that is not shown and
  not added to the end; it is stored, or dropped and fetched again with its page.
- A `reset` answer drops the conversation's turns and loads the newest page.
- Live task SSE stays the low-latency path. On reconnect, a cursor gap, returning to the
  foreground, or opening a saved conversation, clients call `since` and merge it.
- If a request fails, the client keeps its `syncRev` and cursor and retries.

## Verification

- Pages: newest page, consecutive pages without duplicates or gaps, the size bound with at least
  one turn, `limit` bounds, foreign and invalid cursors, a cursor still valid after a new turn.
- `since`: only changed turns plus queued and running ones, title-only changes, `reset` on 0,
  ahead of mobd, and over the bound.
- Load: legacy state gets `seq`, `turns` and `rev` without changing the turn order.
- `/health` advertises `sessionHistory`; the route needs authentication.

## Follow-ups (not in this change)

mobd keeps all history in memory and rewrites `state.json` on every change. Paging bounds what
clients receive, not mobd's own cost; that needs its own change. One very large turn is still sent
whole; splitting a turn's content is separate from turn paging.
