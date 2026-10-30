# File transfers

Status: implementation on `feat/file-transfers`. This document is the shared contract for
mobd, mob, mob-ios and mob-android. The website documents the same user workflow.

## User workflow

Each paired machine has a **Files** screen. Pick a local file to upload, or pick a remote
file to download and save using the operating system's file picker. Show bytes transferred,
pause/continue, failure/retry, and cancel. Transfers and their staged files survive app
restarts. A retry continues the existing transfer; it does not silently overwrite a file.

The shared remote directory is mobd's `filesDir`, by default `~/mob-files` (`MOBD_FILES_DIR`):
a visible folder without spaces, outside the folders macOS protects. The screen displays its absolute
path so it can be copied into a conversation. Agents can read uploaded files there and copy
files from other locations into it. The directory is machine-wide, shared by paired clients
and agents, and independent of conversation deletion. Agent prompt changes are deferred.
Only regular files immediately in this directory are listed; archive folders before transfer.
Names starting with `.` are never listed or accepted (`.uploads`, Finder's `.DS_Store`).

First release: foreground transfers, including recovery after a network interruption or
app restart. Mobile apps pause when backgrounded. Continuous background transfer, folder
sync, remote file deletion, deduplication and parallel parts of one file are outside scope.

## Uploads: tus 1.0.0

Use tusd in mobd, tus-js-client on desktop, TUSKit on iOS, and tus-java-client on Android.
Pin released versions; do not enable the experimental IETF protocol. Existing image
attachments (`POST /uploads`) remain unchanged.

* `POST /file-uploads/`: tus creation, known `Upload-Length` (maximum 20 GiB).
  `Upload-Metadata` must include base64-encoded `filename` and lowercase hex `sha256`.
  The client first stages a private, immutable copy and hashes it with bounded memory.
* `HEAD /file-uploads/{id}`: recover the server's offset. `PATCH` appends at that offset.
* `DELETE /file-uploads/{id}`: cancel and remove an unfinished upload.
* `POST /file-uploads/{id}/complete`: after tus finishes, verify the complete file's SHA-256
  and publish it into the shared directory. Returns `{"name":...,"size":...,"sha256":...}`.
  This step is idempotent: persist the upload URL before calling it, and retry after a lost
  response. A completed byte transfer is not reported as success until publication succeeds.

Partial uploads live in `<filesDir>/.uploads/`: hidden from the listing, and on the same file
system as the shared folder, so publication is an atomic link. Publish
without overwriting: keep the requested name if available, otherwise add the upload ID
before its extension. Publication must be atomic. Cancel removes the partial upload; pause
keeps it. There is no timed expiry in this release, so abandoned uploads retain disk space
until canceled. Published files are never automatically removed.

## Listing and downloads: HTTP

* `GET /files`: `{"directory": "absolute path", "maxUploadSize": 21474836480,
  "files": [{"name": "report.pdf", "size": 123, "modified": "RFC3339"}]}`.
* `HEAD /files/{encoded-name}`: `Content-Length`, strong `ETag`, and `X-Content-SHA256`.
* `GET /files/{encoded-name}`: attachment response with HTTP Range support. Clients use
  bounded ranges and `If-Match: <ETag>`; a changed file returns 412. Never append a 200
  response to an existing partial download. Validate Content-Range and byte count.
* Persist the remote name, machine/address, ETag, checksum and local partial file. Resume
  from the actual local file length. Verify the final SHA-256 before exposing it for Save.
  A changed remote file requires a fresh download; never combine different versions.

## Boundaries and compatibility

Advertise `fileTransfers` in `/health.features`. Older servers show an upgrade explanation.
All transfer requests use the existing paired-device Bearer authentication, including HEAD,
PATCH, completion and Range requests. Never put credentials in URLs or persisted task JSON.
Do not follow cross-origin redirects with credentials; accept upload locations only within the selected
server's transfer route. A new address or machine must not inherit another transfer's URL.

Reject unsafe names, path traversal and symlinks. Download as `application/octet-stream`
with attachment disposition and `nosniff`; do not gzip file bodies. CORS exposes tus and
download headers and allows HEAD, Range and validators. Disk errors remain visible and
leave resumable partial data. Local staging uses app-private storage and is cleaned after
successful upload, saved download, or explicit cancellation.

## Acceptance

Verify actual tus SDK interoperability with mobd and byte-for-byte download equality;
interrupt uploads and downloads, reconstruct clients/server, and finish the same tasks.
Cover empty files, Unicode names, duplicate names, invalid names/symlinks, wrong upload
offsets, checksum mismatch, changed remote files, unauthorized requests, CORS and cancel.
Build each client and the site. Record simulator/emulator and real-device coverage separately;
do not claim background or weak-network hardware acceptance from unit tests.
