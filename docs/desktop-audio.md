# Remote desktop audio

Status: stage 1 (macOS, WebRTC) implemented on `feat/remote-audio`; stages 2 and 3 planned.

A user watching the live view on Mob Desktop, iPhone, iPad or Android hears the office computer's
sound: a game's effects and music, a video, an alert. One direction only: nothing is recorded or
sent from the client. See [desktop streaming](desktop-streaming.md) for the video, input and
control channels this builds on.

Not in scope: microphone forwarding, voice chat, recording, sound in the background on phones,
surround, per-application selection, Windows (mobd has no Windows backend yet). mobd never
changes the office computer's own volume or output.

## Stages

| Stage | Office computer | Transport | Status |
| --- | --- | --- | --- |
| 1 | macOS | WebRTC | Implemented on `feat/remote-audio` |
| 2 | macOS | WebSocket fallback | Planned |
| 3 | Linux, a sound output per conversation desktop | WebRTC, then WebSocket | Planned |

Each stage ships on its own. Clients learn per stream whether sound is available (below), so a
client never needs every combination to work before it offers sound where it can.

## Capture and encoding (macOS)

`internal/desktop/audio_darwin.m` runs a ScreenCaptureKit stream with `capturesAudio` next to a
2×2 screen stream it discards (`SCStream` needs a display). It needs macOS 13 and the Screen
Recording permission mobd already has; there is no new prompt.

- **What is captured:** the computer's playback, without mobd's own
  (`excludesCurrentProcessAudio`) and without Mob Desktop's (`com.xrdavies.mob` excluded from the
  filter). Running Mob Desktop and mobd on one Mac therefore does not echo. Any other audible app
  on the office computer is shared, including notification sounds.
- **Format:** ScreenCaptureKit delivers 48 kHz stereo float; AudioToolbox's `AudioConverter`
  encodes Opus (`kAudioFormatOpus`), 20 ms packets (960 samples per channel), 96 kbit/s target.
  macOS has the encoder built in, so mobd links no codec library. Mono input plays on both sides.
- **Bounds:** at most 200 ms waits for the encoder; older samples are dropped rather than played
  late.
- **Sharing:** `desktop.AudioHub` runs one capture for every viewer. It starts with the first
  viewer that turns sound on and stops with the last that turns it off or leaves. Each viewer
  has a queue of about 200 ms (10 packets); a slow viewer loses its oldest packets.
- **Failure:** capture ending on its own (permission revoked, display gone) ends sound for its
  viewers with a reason; video keeps running.

Linux (stage 3): each conversation desktop already has its own processes (Xvfb, dbus, openbox)
and an environment for the agent's apps. Add a PulseAudio (or PipeWire-pulse) server per desktop
with a null sink, put its address (`PULSE_SERVER`) in that environment, capture the sink's
monitor with the existing GStreamer integration and `opusenc`, and tie its start, sleep and
cleanup to the desktop's lifecycle. `mobd desktops doctor` and the packages gain the sound
server. Ordinary routing keeps conversations apart; it is not a security boundary between
processes of one user.

## API

`/health` lists `streamAudio` when the backend can capture sound (macOS, shared desktop). The
stage-3 Linux backend adds it per desktop. There is no API version change; every field is
optional.

`POST /stream` takes `"audio": true` to ask for sound. The client's offer must then contain a
receive-only audio section offering Opus. The answer gains `audio`:

```json
{"status":"ready","codec":"opus","sampleRate":48000,"channels":2,"frameDurationMs":20}
```

or `{"status":"unavailable","reason":"<sentence for the user>"}` when the backend cannot capture,
the offer has no audio section, or there is no offer (WebSocket only, until stage 2). Without
`audio: true` the answer has no `audio` field and no audio track, as before.

`ready` means the Opus track is negotiated. Sound starts only when the client asks:

```json
{"t":"audio","on":true}
```

on the control channel, and stops with `"on":false`. mobd answers each with
`{"t":"audio","state":"on"}` or `{"t":"audio","state":"off"}`, or
`{"t":"audio","state":"unavailable","message":"…"}` (capture failed, the session was opened
without sound, or the WebSocket carries it). If capture ends on its own mobd sends
`{"t":"audio","state":"error","message":"…"}`. Video and input are never affected.

Turning sound on and off needs no reconnect, and off really stops capture and sending. Muting
in the client only changes local playback.

## WebRTC

The Opus track (`sound`) shares the video's stream ID (`mobd`), so receivers keep the two in sync
using the RTCP sender reports Pion's default interceptors send. Pion copies the offer's Opus
parameters into the answer, and browsers offer mono, so mobd adds `stereo=1;sprop-stereo=1` to
the answer's Opus `fmtp` line (`stereoOpus`); without it receivers decode to mono. Opus is also
registered with stereo before the default codecs.

Each packet's RTP duration is the gap since the previous packet, so dropped packets leave a gap
instead of shifting later sound. The receiver's jitter buffer and decoder do the rest.

## WebSocket (stage 2, planned)

Binary type `2` next to video's `1`: byte 0 = `2`, bytes 1–8 = capture PTS in microseconds (big
endian, the video's host clock), bytes 9 onward = one raw 20 ms Opus packet (at most 4096 bytes).
Unknown types are ignored, never handed to the video decoder. Audio is queued ahead of the next
video frame, with the same 200 ms bound; under pressure old audio is dropped.

Clients play audio as it arrives with a small buffer (target 60 ms) and show video as they do
today. They do not retime video against the audio clock: for controlling a remote desktop, low
delay matters more than lip sync, and the desktop app's MSE path deliberately compresses idle
time. Decoders: WebCodecs `AudioDecoder` where available (desktop), AudioConverter (iOS), MediaCodec
(Android).

## Clients

New live views start with sound off. A Sound button appears when `/health` has `streamAudio`;
every live view then asks for `audio: true`, so the button only sends the control message. Sound
stays on through reconnects within one view and stops when the view closes, the app goes to the
background (phones), the machine or conversation changes, or the device is unpaired. Only one
view plays at a time. Client plans: [desktop](https://github.com/xrdavies/mob/blob/main/docs/desktop-audio.md),
[iOS](https://github.com/xrdavies/mob-ios/blob/main/docs/desktop-audio.md),
[Android](https://github.com/xrdavies/mob-android/blob/main/docs/desktop-audio.md).

## Verification

- `internal/desktop/audio_test.go`: one shared capture, stopped with the last viewer, capture errors
  reach viewers, slow viewers drop old packets.
- `internal/desktop/audio_darwin_test.go` (needs Screen Recording): real capture while a system
  sound plays: about 50 stereo packets a second, larger while something is audible.
- `internal/stream/audio_test.go`: a Pion viewer gets a stereo Opus answer, no packets until it
  turns sound on, packets after, capture stopped by off and by closing; unavailable when the
  backend, the offer or the transport cannot carry sound; no `audio` field when not asked for.
- By hand: the three apps against mobd on this Mac and on the office Mac with a game's sound.

Measurements to record for stage 1: time from sound on to first sound, audible delay against
video, CPU of the capture. Bluetooth output adds its own delay; report it separately.

## References

- [ScreenCaptureKit](https://developer.apple.com/documentation/screencapturekit): `capturesAudio`, `excludesCurrentProcessAudio`.
- [Pion WebRTC](https://github.com/pion/webrtc), [RFC 7587](https://www.rfc-editor.org/rfc/rfc7587) (Opus RTP, `stereo`/`sprop-stereo`).
- [PulseAudio modules](https://www.freedesktop.org/wiki/Software/PulseAudio/Documentation/User/Modules/), [GStreamer opusenc](https://gstreamer.freedesktop.org/documentation/opus/opusenc.html) (stage 3).
