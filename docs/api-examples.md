# API examples

HTTP API usage with curl. See [mobd-v3.md](mobd-v3.md) for the full API reference.


First generate an invitation with `mobd link`, then redeem it with `POST /pair`
as described in the [pairing protocol](docs/pairing.md). Set `MOBD_TOKEN` to the
returned device credential. Every example below requires it, including reads.
Do not put the credential in URLs or share it between devices.

```bash
# health
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/health | jq .

# first turn: creates a conversation and returns its sessionId
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Use mob-mcp to open Safari"}' | jq .

# follow-up: reuse the returned sessionId
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"sessionId":"SESSION_ID","prompt":"Now open example.com in that window"}' | jq .

# list conversations, then read the full user/assistant transcript and turns
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/sessions | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/sessions/SESSION_ID | jq .

# list (filters: sessionId, status, limit 1-500)
curl -H "Authorization: Bearer $MOBD_TOKEN" -s 'localhost:8080/tasks?status=running' | jq .

# get one (replace ID)
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/tasks/TASK_ID | jq .

# SSE events
curl -H "Authorization: Bearer $MOBD_TOKEN" -N localhost:8080/tasks/TASK_ID/events

# resume delivery after event 12 (Last-Event-ID takes precedence over ?after=)
curl -H "Authorization: Bearer $MOBD_TOKEN" -N -H 'Last-Event-ID: 12' localhost:8080/tasks/TASK_ID/events

# cancel
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks/TASK_ID/cancel | jq .
```

General file exchange uses [tus uploads and HTTP Range downloads](docs/file-transfers.md).
Clients show the shared folder (`filesDir`, default `~/mob-files`); uploads are checksum-verified before
publication and never overwrite an existing file.

Task options, attachments, session management and live updates
([API reference](docs/mobd-v3.md)):

```bash
# per-turn model / reasoning effort / image attachments
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/uploads -F file=@shot.png | jq .      # → {id, url, …}
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X POST localhost:8080/tasks -H 'Content-Type: application/json' \
  -d '{"prompt":"What is in this image?","effort":"high","attachments":["UPLOAD_ID"]}' | jq .

# desktop queue (one running task by default)
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/queue | jq .

# session admin
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X PATCH localhost:8080/sessions/SESSION_ID -d '{"title":"Weekly report","pinned":true}' | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X PATCH localhost:8080/sessions/SESSION_ID -d '{"archived":true}' | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s 'localhost:8080/sessions?archived=all' | jq .
curl -H "Authorization: Bearer $MOBD_TOKEN" -s -X DELETE localhost:8080/sessions/SESSION_ID -o /dev/null -w '%{http_code}\n'   # 204, 409 while active

# timeline: messages + activities (tool calls, commands, plans…) + task usage
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/sessions/SESSION_ID | jq '.activities'

# live view: POST /stream with a WebRTC offer (see docs/desktop-streaming.md);
# cmd/stream-probe is a command-line viewer for testing:
go run ./cmd/stream-probe -url http://localhost:8080 -token "$MOBD_TOKEN" -seconds 10

# global live stream (session / session.deleted / task / queue / reset)
curl -H "Authorization: Bearer $MOBD_TOKEN" -N localhost:8080/events

# desktop snapshot; tool screenshots and uploads
curl -H "Authorization: Bearer $MOBD_TOKEN" -s 'localhost:8080/screen?maxWidth=720' -o screen.jpg
curl -H "Authorization: Bearer $MOBD_TOKEN" -s localhost:8080/artifacts/ARTIFACT_ID -o artifact.png
```
