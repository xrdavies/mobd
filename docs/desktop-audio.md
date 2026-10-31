# Remote desktop audio

Status: draft, 2026-11-01. This document proposes a protocol and implementation;
it does not describe a released feature. The current live view carries video
and input only. See [desktop streaming](desktop-streaming.md) for that protocol.

## Goal and scope

A user testing a game on the office computer should hear its effects and music
while watching the live view on Mob Desktop, iOS or Android. Deliver the remote
computer's playback audio in one direction. Reuse the paired stream session,
WebRTC connection and WebSocket fallback.

The complete feature includes both transports on supported office backends;
WebRTC-only audio is an implementation milestone, not completion. macOS and
Linux are the initial capture targets. Windows capture uses the same contract
when the Windows desktop backend is implemented. Windows as a Mob Desktop
client is part of the initial client scope.

Microphone forwarding, voice chat, recording, audio-only background playback,
surround sound and selecting individual applications are outside this draft.
Do not change the office computer's speaker volume or mute its physical output.
macOS initially shares desktop system audio, including other audible apps;
Linux routes each conversation's applications to that desktop's audio output.

## Existing code and change boundaries

| Area | Current behavior | Planned change |
| --- | --- | --- |
| `internal/desktop/backend.go`, `stream_darwin.*` | H.264 access units from ScreenCaptureKit and VideoToolbox | Add optional system-audio capture and encoded audio units |
| `internal/desktop/manager/`, `internal/desktop/x11/`, `internal/desktop/gst/` | Per-conversation X11 desktops; no managed sound server | Desktop audio routing, monitor capture and Opus encoding |
| `internal/stream/stream.go`, `input.go`, `ws.go`, `rate.go` | Video track, video frames, input and transport race | Audio negotiation, transport, shared clock and bounded queues |
| `internal/api/stream.go`, `health.go` | Paired stream and feature discovery | Optional audio request/result and capability |
| Build/release packaging | Video dependencies | Bundle or declare the chosen audio encoder and Linux sound-service dependencies |
| Store and runner data | Chat history and agent execution | No audio persistence or chat protocol change |

Keep video usable if audio permission, the output device or an encoder fails.
Extend the current capture/session types only as needed; do not introduce a
second authentication or remote media service.

## Capture and encoding

| Office backend | Plan |
| --- | --- |
| macOS | Enable ScreenCaptureKit `capturesAudio` and register an audio output alongside the screen output. Convert sample buffers to the encoder's PCM format on a separate queue. Inspect the installed OS's capture authorization and report missing permission without repeatedly prompting. |
| Linux | Give each managed desktop a named virtual output and capture its monitor. Configure application/agent environment before launch so audio reaches that output. Use the existing GStreamer integration for capture, conversion/resampling and `opusenc`. |
| Windows, later | Capture the playback endpoint with WASAPI loopback and feed the same encoder contract. Handle output-device changes; shared-mode loopback does not promise capture of exclusive-mode or protected content. |

For the first Linux implementation, prefer a managed PulseAudio service with a
null sink for each desktop, reachable through its configured socket/output.
PipeWire's PulseAudio-compatible service is an alternative when explicitly
supported and verified; do not require a logged-in physical desktop or silently
capture the machine's default output. Ordinary app routing must not cross
conversations. This is not a new security boundary between same-user processes.
The virtual output must exist while applications run, even with no viewer;
start monitor capture/encoding only for an audio-enabled stream. Tie service,
sink, sleep/wake and cleanup to the desktop manager lifecycle.

Use standard Opus, initially 48 kHz stereo, 20 ms per packet (960 samples per
channel), targeting 96 kbit/s. These are proposed starting settings, not measured
quality or bandwidth guarantees. Use music/general-audio encoding and preserve
level dynamics; do not add voice AGC, noise suppression or echo cancellation.
Send ordinary encoded silence initially rather than introducing DTX handling.

Pion transports encoded media; codec registration does not encode PCM. Use
`libopus` for a native encoder where the platform integration needs one and the
GStreamer Opus plugin on Linux. Pin and package the selected implementation
during implementation, including licenses; do not require users to install an
ad hoc command-line recorder or implement a codec in Go.

## Negotiation and compatibility

Proposed `/health` feature: `streamAudio`. Advertise it only once the backend
implements this contract over WebRTC and WebSocket. Per-session readiness still
depends on permission and the selected desktop's audio service.

Extend `POST /stream` with an optional boolean `audio`, default `false`:

```json
{"offer":"<SDP>","maxWidth":1600,"maxHeight":1000,"quality":"auto","audio":true}
```

The example is for a shared desktop; Linux still requires the existing
`sessionId`. An audio-enabled WebRTC offer includes a receive-only audio media
section offering Opus. A WebSocket-only request may omit `offer` as today.
An offer without a usable audio section may keep video working but must not be
reported as WebRTC audio-ready.

Add an `audio` result to the existing response. An enabled result is:

```json
{"status":"enabled","codec":"opus","sampleRate":48000,"channels":2,"frameDurationMs":20}
```

Other results are `{"status":"disabled"}` or
`{"status":"unavailable","reason":"<actionable explanation>"}`. Enabled
means the audio path was prepared, not proof that audible samples have played.
For a runtime audio-only failure, use the existing control error shape,
`{"t":"error","about":"audio","message":"…"}`, and stop audio while
keeping video/input alive. Retrying audio can reopen the stream.

Old clients omit `audio` and receive no audio track or binary audio packets.
New clients on an old server keep video and explain that sound needs a newer
mobd; they do not infer audio from a successful video connection. There is no
API-version bump for these optional fields. Define the request/response models
in every client together with their feature checks.

## WebRTC

Add an Opus send-only track to the existing PeerConnection when audio is
negotiated. Use the same media-stream identity as video, negotiate stereo, and
consume the audio sender's RTCP separately from video keyframe feedback.
libwebrtc/browser receivers supply their audio decoder and jitter buffer.

Keep the current transport winner policy. Only the winning transport may be
audible; preparing a receiver must not play the losing transport. Video startup
must not wait for non-silent audio, and a silent application is not a connection
failure. Share the session's close, revoke, replacement and cancellation paths.

## WebSocket fallback

Carry Opus packets over the existing authenticated socket. Preserve binary
video type `1` and add type `2` only for audio-enabled sessions:

| Bytes | Proposed audio meaning |
| --- | --- |
| 0 | `2`, raw Opus packet |
| 1–8 | First sample capture PTS, microseconds, unsigned big endian, same host clock as video |
| 9 | Flags: bit 0 marks a discontinuity; other bits reserved |
| 10 onward | One complete 20 ms stereo Opus packet, without Ogg framing |

Use the negotiated format in the stream response to configure decoders before
audio consumption. Bound packet size (initial limit 4096 payload bytes), validate
duration/channel configuration and reject malformed frames before decoding.
Ignore unknown binary types without treating them as H.264. The first packet
and the first packet after capture restart or an application-level drop mark a
discontinuity. Receivers flush outdated audio and re-establish playback timing;
they must not play a gap's worth of old sound. Final codec-delay/priming handling
must be demonstrated by the cross-client fixture before freezing this format.

Keep the existing video acknowledgement. Add `{"t":"audioAck","pts":N}`
for the latest consumed network audio packet, independent of whether sound is
muted. It acknowledges receipt, not speaker playback. Account for audio bytes
and oldest outstanding age even on a static screen. Video frame rates/loss and
keyframe logic must not count audio packets as video frames.

Use bounded queues, with audio/control selected ahead of the next unsent video
frame. Initial audio queue target is 60 ms, with a 200 ms hard limit on pending
unsent or unplayed audio. Under pressure discard stale data and resynchronize;
do not grow memory or catch up by playing old sound. Limit video backlog as well:
priority cannot bypass a video frame already written into TCP. If transport
backlog persists, surface degraded audio and reconnect rather than accumulating
seconds of latency. These budgets are not guarantees about TCP or device delay.

TURN can later reduce dependence on WebSocket, but requires an actual relay and
deployment plan. This feature must work on the current HTTP/WebSocket route
without requiring TURN or a new public port.

## Clocks and synchronization

Audio continues when the picture is unchanged. Its timestamp derives from sample
count and the capture clock, never from video frame count or send time. Map both
capture streams onto one host monotonic timebase and account for encoder delay.

For WebRTC, map the 48 kHz audio and 90 kHz video RTP clocks to that shared
timebase using consistent RTCP sender reports/CNAME. Adding two independent
`WriteSample` loops does not itself establish correct audio/video alignment.
Verify the existing variable/idle video timestamps as part of the change.

For WebSocket, clients map host PTS to one local playback clock, using rendered
audio position while audio is active. Hold the last picture through static
intervals; schedule later pictures against capture time. Bound late video
buffering and correct clock drift through the playback/resampling layer. Do not
wait indefinitely for a missing picture or stretch audio to match frame arrival.
The desktop MSE path currently compresses idle video time: it must preserve a
mapping to the audio clock rather than treating that compressed timeline as
capture time. This is an explicit implementation and test requirement.

## User controls and lifecycle

Each newly opened live view starts with sound off. The explicit Sound action
requests `audio:true`; if the existing session is video-only, reopen it once
using the current reconnect path, releasing held input and preserving view
geometry. Re-enabling sound on that same audio-enabled session is local.
Mute and volume are client playback controls: mute does not promise that the
remote capture or bandwidth stops. Stopping/replacing the stream does.

Only the current live view owns playback; previews cannot create duplicate
players. Close, machine/conversation switch, mobile backgrounding and credential
revocation stop/flush playback. A transient reconnect within that view may
retain the user's sound choice, but starts at live time with no replay backlog.
No microphone track or recording permission is needed by the product flow.

## Implementation order and acceptance

1. Build a macOS capture/Opus/WebRTC prototype against all three client families;
   verify receive-only audio, stereo, common timestamps and no microphone prompt.
2. Add WebSocket audio to all clients, including the desktop MSE video path;
   settle decoder initialization/priming and packet limits with shared fixtures.
3. Add Linux desktop audio service/routing, lifecycle and packaging. Validate two
   simultaneous desktops, including headless start without physical sound hardware.
4. Advertise the feature and update current API/user docs only after the supported
   matrix passes. Record unsupported combinations and measured results honestly.

Extend store-free stream tests and `cmd/stream-probe` with synthetic stereo tones
and an accompanying flash/click fixture. No model request is needed. Test:

- Omitted/false audio, new/old clients and unavailable capture preserve video.
- Left/right tones survive encoding and playback without unintended mono mixing.
- A static image with sound keeps producing audio; silence does not time out.
- Both transports play on desktop/macOS, desktop/Windows, desktop/Linux, iOS and
  Android; forced UDP failure and remembered transport fallback remain usable.
- First sound, output-device changes, 30-minute drift, mute/unmute, session close,
  credential revocation, reconnect and duplicate-view prevention.
- Truncated/oversized packets, bad format, slow receivers and capture failure do
  not crash or leave queues growing.
- Two Linux desktops play distinct tones with no accidental cross-routing.

Measure capture-to-speaker delay, audio/video skew, packet loss, queue age, CPU
and bandwidth on LAN, tailnet and the real WebSocket gateway. Proposed initial
LAN targets are audio latency p95 <= 150 ms and absolute A/V skew p95 <= 80 ms
over 30 minutes using built-in/wired outputs; these are acceptance targets,
not existing results. Report Bluetooth and gateway measurements separately,
including any target misses. Decoder counters alone are not proof of audible
or synchronized playback.

## References and client plans

- [Apple ScreenCaptureKit example](https://developer.apple.com/documentation/screencapturekit/capturing-screen-content-in-macos): system-audio sample buffers alongside video.
- [PulseAudio modules](https://www.freedesktop.org/wiki/Software/PulseAudio/Documentation/User/Modules/) and [PipeWire null sink](https://docs.pipewire.org/page_pulse_module_null_sink.html): virtual playback output.
- [WASAPI loopback](https://learn.microsoft.com/en-us/windows/win32/coreaudio/loopback-recording): Windows playback capture and limits.
- [Pion](https://github.com/pion/webrtc), [libopus](https://opus-codec.org/docs/) and [GStreamer opusenc](https://gstreamer.freedesktop.org/documentation/opus/opusenc.html): transport and encoding building blocks.
- [RFC 7874](https://www.rfc-editor.org/rfc/rfc7874.html): WebRTC Opus interoperability and desktop-audio processing guidance.
- Client drafts: [Desktop](https://github.com/xrdavies/mob/blob/main/docs/desktop-audio.md), [iOS](https://github.com/xrdavies/mob-ios/blob/main/docs/desktop-audio.md), [Android](https://github.com/xrdavies/mob-android/blob/main/docs/desktop-audio.md).
