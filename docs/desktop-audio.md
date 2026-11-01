# Remote desktop audio

Status: implemented for macOS and, on `feat/linux-audio`, for Linux conversation desktops (WebRTC). Windows waits for a Windows desktop backend.

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
| 1 | macOS | WebRTC | Implemented |
| 2 | Linux, a sound output per conversation desktop | WebRTC | Implemented on `feat/linux-audio` |
| Later | Windows (WASAPI loopback) | WebRTC | With the Windows desktop backend ([windows.md](windows.md)) |

Each stage ships on its own. Clients learn per stream whether sound is available (below), so a
client never needs every combination to work before it offers sound where it can.

Sound is WebRTC only, by decision: there is no sound over the WebSocket fallback (see below).

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

## Capture and encoding (Linux)

`internal/desktop/manager/sound.go`. Each conversation desktop runs its own sound server next to
its Xvfb, dbus and openbox, so conversations never hear each other:

- **Server:** `pulseaudio -n` with a generated `default.pa`: a null sink `mob` (48 kHz stereo,
  the default sink) and a native-protocol socket in the desktop's runtime directory (mode 0700,
  so no other authentication). It starts in the desktop's `session/` cgroup before openbox, with
  its runtime, state and cookie in that directory, never `~/.config`. It sleeps and wakes with
  the desktop (cgroup freezer) and stops with it.
- **Routing:** the agent's and openbox's environment get `PULSE_SERVER=unix:<socket>`, so apps
  the agent or the user starts play into that desktop's output. Ordinary routing keeps
  conversations apart; it is not a security boundary between processes of one user.
- **Capture:** GStreamer `pulsesrc` on `mob.monitor` → `opusenc` (96 kbit/s, 20 ms, general
  audio) → appsink, shared by the desktop's viewers through `desktop.AudioHub` as on macOS. PTS
  counts packets, so a dropped packet leaves a gap. The first packet arrives about 0.4 s after
  the capture starts.
- **Failure:** the server exiting is restarted at the same socket, at most 5 times a minute;
  then the desktop runs silent until it restarts. A server that does not start leaves the
  desktop working and silent.
- **Optional:** without `pulseaudio` (or GStreamer's `pulsesrc`/`opusenc`) desktops work as
  before and `/health` does not list `streamAudio`. `mobd desktops doctor` has a `sound` check
  with the fix. The `.deb` recommends `gstreamer1.0-pulseaudio` and suggests `pulseaudio` rather
  than depending on it: on a PC whose own sound is PipeWire, the package's per-user service
  would compete with it ([linux-build.md](linux-build.md), notes).

## API

`/health` lists `streamAudio` when the backend can capture sound: macOS, and Linux with
conversation desktops when PulseAudio and GStreamer's audio capture are installed. On Linux the
answer to `audio: true` is `unavailable` for a desktop without a running sound server. There is no API version change; every field is
optional.

`POST /stream` takes `"audio": true` to ask for sound. The client's offer must then contain a
receive-only audio section offering Opus. The answer gains `audio`:

```json
{"status":"ready","codec":"opus","sampleRate":48000,"channels":2,"frameDurationMs":20}
```

or `{"status":"unavailable","reason":"<sentence for the user>"}` when the backend cannot capture,
the offer has no audio section, or there is no offer (WebSocket only). Without
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

## No sound over the WebSocket

Decided 2026-11-01: when the WebSocket carries the live view (no UDP path: Cloudflare Tunnel,
Funnel, Mob relay today, networks that block UDP, the Linux webview without GStreamer's WebRTC),
there is no sound. The answer says `unavailable` with that reason, and the apps show it.

TCP delivers in order: a lost packet stalls everything until it is resent, and audio queued
behind a video frame waits for it. A picture that lags is acceptable for watching and controlling
a desktop; sound that stutters or drops out is not. Rather than ship sound clearly worse than the
picture, Mob offers sound only where WebRTC carries the session. The way to sound on those
networks is a better path for WebRTC (STUN and TURN in Mob relay, planned there), not audio over
TCP.

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
- `internal/desktop/manager/sound_test.go`: the server starts in `session/` with its config, the
  agent and openbox get `PULSE_SERVER`, restarts and giving up, a desktop without `pulseaudio`.
- `internal/desktop/manager/sound_e2e_test.go` (`MOBD_DESKTOP_TEST=1`, real desktops): a tone
  played on one of two desktops is in every packet of its capture (50 a second) and in none of
  the other's.
- By hand: the three apps against mobd on this Mac and on the office Mac with a game's sound.
  On Ubuntu 24.04 (arm64) a Linux mobd whose agent played a tone on its conversation desktop:
  `stream-probe -session … -audio` received 50 stereo packets a second, all with sound, the
  first 55 ms after sound on.

Measurements to record for stage 1: time from sound on to first sound, audible delay against
video, CPU of the capture. Bluetooth output adds its own delay; report it separately.

## References

- [ScreenCaptureKit](https://developer.apple.com/documentation/screencapturekit): `capturesAudio`, `excludesCurrentProcessAudio`.
- [Pion WebRTC](https://github.com/pion/webrtc), [RFC 7587](https://www.rfc-editor.org/rfc/rfc7587) (Opus RTP, `stereo`/`sprop-stereo`).
- [PulseAudio modules](https://www.freedesktop.org/wiki/Software/PulseAudio/Documentation/User/Modules/), [GStreamer opusenc](https://gstreamer.freedesktop.org/documentation/opus/opusenc.html) (stage 2).
