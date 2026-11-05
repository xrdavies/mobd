# Investigation: iPhone live view fails on cellular

**Status:** open, root cause not confirmed. No code has changed.
**Date:** 2026-11-05. Times are the office Mac's local time.

## Symptom

mob-ios connects to the live view on Wi‑Fi but not on cellular. Tailscale stays on on the iPhone
throughout, and the Wi‑Fi is not the office LAN. So neither case is a LAN connection.

## Setup

- mobd runs on a Mac mini with no `relay` in its config, so WebRTC has host candidates only
  ([signaling](desktop-streaming.md#signaling)), and `GET /stream/ice` answers an empty list.
- The app reaches mobd through Tailscale Serve (HTTPS on 443, proxied to `127.0.0.1:8080`).
  Media takes WebRTC to UDP 9527, or the WebSocket through Serve.
- The office network has two NAT layers: an Asus router, then an upstream router.
- The Mac's nearest DERP region is San Francisco, about 156 ms away.

## What the logs show

Sources: `~/.mobd/mobd.log` and `/opt/homebrew/var/log/tailscaled.log` on the Mac. There are no
app logs or packet captures for these attempts.

Seven sessions from one client opened between 08:50 and 08:53. None connected over WebRTC:

| Opened | Closed | Result |
| --- | --- | --- |
| 08:50:21 | 08:50:33 | closed, no transport |
| 08:50:35 | 08:50:49 | WebSocket connected at 08:50:48, closed 1 s later |
| 08:50:57 | 08:51:12 | closed, no transport |
| 08:51:12 | 08:51:29 | closed, no transport |
| 08:51:32 | 08:52:02 | `peer did not connect within 30s` |
| 08:52:05 | 08:52:24 | closed, no transport |
| 08:52:31 | 08:53:01 | `peer did not connect within 30s` |

Signaling reached mobd every time, so the phone had a working TCP path. Then at 09:09:36 a session
opened and logged `connected over webrtc` at 09:09:39. It closed at 09:09:42.

Tailscale's log for the same period:

- Disco contact from the phone arrived `via=derp` from 08:39 onward.
- The only path change in the window is at 09:10:34, when the Mac moved to a direct endpoint.
  After that, `tailscale ping` answered directly in 10 ms.
- NAT mapping varied: `NetInfo` flipped between `varies=true` and `varies=false` several times
  between 08:55 and 09:51. A single `tailscale netcheck` reported `MappingVariesByDestIP: false`.
- 7 inbound connections to the Serve port were refused today, between 08:39:38 and 09:29:36
  (`CreateEndpoint error … :443: connection was refused`), and the proxy logged
  `context canceled` around the failed attempts. No Serve change appears in that window.

## Findings

**The failed attempts ran over DERP.** The phone had no direct path to the Mac during any of the
seven sessions.

**DERP alone does not explain the failure.** The 09:09 WebRTC session connected in about 3 s, about
a minute *before* the direct path appeared. Going by the log, that session also ran over DERP, or
over some path the log does not show. So "cellular fails because it is relayed; Wi‑Fi works because
it is direct" does not hold as stated. Which network the phone was on at 09:09 was not recorded;
that it was Wi‑Fi is an assumption.

**The app's timeout can end the fallback (likely).** `connectTimeout` in mob-ios
(`mob-ios/Stream/StreamSession.swift`) is 15 s from the tap. It includes WebRTC's 5 s head start and
is not extended once the WebSocket starts. The 08:50:35 session fits this: mobd saw the WebSocket
13 s in and the close 1 s later. The app's log would confirm it. Closes at 17 s and 19 s after
opening are also consistent, if the `DELETE` was slow to arrive, but that is not shown.

## Open questions

1. Does the iPhone offer a Tailscale candidate on cellular? If not, ICE has no usable pair there,
   whatever the path quality. Nothing recorded so far can show this.
2. Do STUN checks from the phone reach UDP 9527? This separates question 1 from packet loss on the
   way.
3. Why were connections to the Serve port refused, when Serve was configured? This could affect
   signaling and the WebSocket, and it may be a separate problem.
4. Was DERP slow or lossy at the time? The 13 s to the WebSocket suggests so, but it was not
   measured. Whether carrier NAT prevents a direct path is also unmeasured.

## Next steps

Test on cellular, with the time noted, collecting all of these from one attempt:

- The app's `live view: …` log lines, especially `answered in … ms`, and the offer's candidates.
- On the Mac, `sudo tcpdump -ni <tailscale utun> udp port 9527`. This needs a password.
- Alternatively, Pion ICE debug logs (`PION_LOG_DEBUG=ice`). This needs a mobd restart.
- `tailscale ping <phone>` during the attempt, for the path and latency.

## Candidate fixes

None is verified against this failure.

1. **mob-ios:** extend or restart the connect timeout once the WebSocket starts, so a slow path can
   reach a first picture. This is the one fix backed by the evidence above, and it is independent
   of the network.
2. **Direct Tailscale path:** forward a fixed UDP port to the Mac on the upstream router, or remove
   one NAT layer. Worth trying for quality, but it might not fix the failure, given the 09:09
   session.
3. **A closer relay:** the [`relay.turn`](desktop-streaming.md#through-a-mob-relay-stun-and-turn)
   support with mob-relay, or a Tailscale peer relay. This gives WebRTC a path that does not depend
   on the phone's Tailscale candidate, which also covers open question 1.
