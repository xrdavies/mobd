# Research: per-agent independent desktop (Tart / UTM planning)

**Date:** 2026-10-13 (Asia/Shanghai)
**Repo:** [xrdavies/mob](https://github.com/xrdavies/mob)
**Status:** planning only — **no product code** in this note. MVP remains a **single shared desktop + global queue**.

**Question:** How can MOB approximate Grok Bot’s model — **one office Mac, shared filesystem, independent desktop per agent** — without agents fighting over the same cursor and key window?

**Related:** [architecture.md](architecture.md) (control / view / actuation split), [apps-mob-ui.md](https://github.com/xrdavies/mob/blob/main/docs/ui.md) (machine → session IA), [research-computer-use.md](research-computer-use.md) (local MCP actuation), [ADR 0001](adr/0001-screen-view-vnc-svnc.md) (human view vs actuation).

---

## 1. Goal

Target product shape (aligned with how Grok Bot agents share a box but keep separate desktops):

| Property | Intent |
| --- | --- |
| Hardware | One (or few) office Mac host(s) |
| Filesystem | Shared project / workspace tree across agents |
| Desktop / GUI | **Independent** per agent — no shared cursor, key window, or TCC session |
| Client UX | Left: machines → agents/sessions; center: chat; right: live preview of the **selected** agent’s desktop |
| Control plane | `mobd` still owns tasks, queue, sessions; view stays VNC/svnc (or later `DesktopView`) |

MVP today already has the **information architecture** for machines and sessions ([apps-mob-ui.md](https://github.com/xrdavies/mob/blob/main/docs/ui.md)). What it does **not** yet have is a second (or Nth) real GUI session on the office host.

---

## 2. Why the same desktop interferes

On macOS there is typically **one interactive WindowServer session per logged-in GUI user** that owns:

- a single cursor and keyboard focus (key window)
- Spaces / Mission Control layout for that user
- Accessibility / Screen Recording TCC grants scoped to that user context

Consequences for parallel GUI agents on **one** desktop:

- Clicks and typing race for the same focus.
- Screenshots and AX queries see each other’s windows.
- “Separate Spaces” or “tile windows” does **not** isolate input; it only rearranges chrome.

Therefore: **same-desktop parallel GUI actuation is not a viable product path.** Isolation must be at the **GUI session** (or stronger) boundary.

---

## 3. Options (brief)

| Option | Isolation | Shared FS | Parallel GUI | Effort / cost | Role for MOB |
| --- | --- | --- | --- | --- | --- |
| **A. Serial queue (MVP)** | Time-sliced on one desktop | Native | No (one driver at a time) | Already in `mobd` v3 global desktop queue | **Current default** |
| **B. Multi-user Screen Sharing** | Per macOS user GUI (“Log In”, not Share Display) | `/Users/Shared`, NFS, or ACL’d project dir | Yes, within host memory limits | Medium: user provisioning, per-user TCC, VNC per user | Strong **native** parallel path |
| **C. Tart / UTM VM per agent** | Full guest OS + desktop | virtio-fs / shared dir / NFS / host mount into guests | Yes | Higher: RAM, disk, images, Apple Silicon licensing | Strong **isolation / reset** path |
| **D. Multiple physical Macs** | Physical | Network FS | Yes | Highest ops | Scale-out later |

Spaces / virtual displays under one user are **not** listed as options: they do not give a second cursor/key-window domain.

**Do not** fork Tart, and **do not** build a custom hypervisor. Use upstream Tart (Apple Silicon preferred) or UTM as black-box runtimes; MOB only orchestrates.

---

## 4. Tart / UTM path (recommended packaging)

### 4.1 Principles

1. **Upstream as-is** — install Tart or UTM from their releases; pin versions in office runbooks. No fork, no rebranded hypervisor.
2. **MOB orchestrates, guests run the stack** — host scripts (later: `mobd` or a thin office agent) start/stop VMs, attach shared storage, and advertise per-VM view endpoints. Each guest runs its own `mobd` (or a slim runner + MCP) and VNC/Screen Sharing for that guest desktop.
3. **View stays separate from actuation** — human preview is still VNC → (optional websockify / mobd `/vnc` relay) → svnc `viewOnly`; agent hands stay local MCP / Accessibility inside the guest ([ADR 0001](adr/0001-screen-view-vnc-svnc.md), [ADR 0003](adr/0003-actuation-local-mcp.md)).
4. **Client model** — `machine` (host) → `agent` / desktop slot (VM or user session) → `session` (chat/task). Preview binds to the selected slot’s `VNC_WS_URL` (or mobd-relay URL).

### 4.2 Host responsibilities (sketch)

- Image bake: macOS guest with TCC grants pre-staged for `mobd` / Codex where possible, VNC or Screen Sharing enabled, MOB tooling installed.
- Lifecycle: `tart run` / UTM start-stop; health probes; reclaim idle VMs.
- Networking: host-only or bridged; expose per-VM `mobd` HTTP and VNC (or one host reverse-proxy that routes by agent id).
- Registry: map `agentId` → `{ vmName, mobdUrl, vncWsUrl }`.

### 4.3 Guest responsibilities

- Run Codex (or other runners) + `mobd mcp` actuation against **this** desktop only.
- Report task/SSE status to the control plane (guest `mobd` or host aggregator — TBD in a later design).
- Optional: human Control mode in the App still targets that guest’s VNC only.

### 4.4 Tart vs UTM (planning note)

| | **Tart** | **UTM** |
| --- | --- | --- |
| Sweet spot | Apple Silicon, CLI/`tart` automation, CI-friendly | Broader GUI management; Intel + AS depending on setup |
| MOB fit | Prefer for scripted office host automation | Prefer if operators want a GUI console on the host |
| Shared FS | Documented directory mounts / virtio-fs style sharing (verify against pinned Tart version) | Shared directories via VirtioFS / SPICE-style shares (verify against pinned UTM version) |

Exact mount flags and macOS guest license constraints must be re-checked against the pinned tool version before implementation; this doc does not freeze CLI syntax.

---

## 5. Shared filesystem approaches

Goal: agents edit the **same** project tree (or a well-defined shared workspace) while keeping desktops apart.

| Pattern | Where it fits | Notes |
| --- | --- | --- |
| **Host directory → guest mount (virtio-fs / Tart disk mount)** | VM path | Best default: host holds canonical `~/Projects/...`; each VM mounts read-write or read-mostly + per-agent scratch |
| **NFS / SMB from host or NAS** | VM or multi-user | Good when multiple hosts; watch lock files and editor race semantics |
| **`/Users/Shared` + POSIX/ACLs** | Multi-user Screen Sharing | Native macOS; simple for option B |
| **Per-agent clone + sync** | Either | Weaker “shared FS” story; only if mount races are unacceptable |
| **Git remote as source of truth** | Either | Complements mounts; not a substitute for a live shared working tree |

**Concurrency warning:** shared working trees still need social/technical rules (one agent per repo branch, or directory leases). Desktop isolation removes GUI races; it does **not** remove git/index races.

---

## 6. Pros / cons vs multi-user; when to choose VM

### Multi-user Screen Sharing (option B)

| Pros | Cons |
| --- | --- |
| No VM RAM tax; native apps and GPU path | Per-user TCC and login choreography (“Log In” vs Share Display) |
| Natural shared FS via `/Users/Shared` | Weaker blast-radius isolation (same kernel) |
| Closer to “one Mac, many desktops” metaphor | Harder to wipe a poisoned desktop cleanly |

### Tart / UTM (option C)

| Pros | Cons |
| --- | --- |
| Strong isolation; snapshot / clone / reset | RAM and disk per guest; image maintenance |
| Clean per-agent VNC and TCC inside guest | macOS guest licensing and IP compliance |
| Easier “cattle” ops (recreate agent desktop) | Slightly farther from bare-metal feel |

**Choose VM when:** you need parallel GUI **and** resettable sandboxes, or multi-user Screen Sharing fails smoke tests (Login persistence, TCC, svnc auth).
**Choose multi-user when:** you want maximum host efficiency and native feel, and can accept shared-kernel risk.
**Stay on serial queue when:** proving dispatch → actuation → preview; parallel desktops are not an MVP exit criterion.

---

## 7. Fit to MOB architecture

```text
Home App (apps/mob)
  machine (office host)
    agent slot A ──► guest/user A desktop ──► VNC_WS_A ──► preview
    agent slot B ──► guest/user B desktop ──► VNC_WS_B ──► preview
  chats/sessions attach to one agent slot
```

| Layer | Today (MVP) | Later with VM / multi-user |
| --- | --- | --- |
| Client IA | Machines → sessions; one VNC per machine | Machines → **agents/slots** → sessions; VNC per slot |
| `mobd` scheduler | Global desktop queue (one GUI driver) | Per-slot queue (or host orchestrator assigning slots); cross-slot parallelism OK |
| Actuation | Host `mobd mcp` on the shared desktop | Per-guest (or per-user) `mobd mcp` |
| View | One VNC / mobd `/vnc` relay | N relays or N upstream VNC targets; svnc still view/control separated |
| Packaging | websockify OK for MVP | Stable release may fold relay/deps ([ADR 0010](adr/0010-vnc-relay-in-mobd.md) direction); hypervisor stays **external** |

MOB continues to **not** own a fork of Tart/UTM/svnc’s core; it integrates and orchestrates.

---

## 8. Suggested later phases / smoke experiments

Explicit: **do not block Phase 1/2 exit criteria on this.** MVP = single desktop + queue.

### Phase 0 — document & model (this note)

- [x] Record goal, options, non-goals (no fork / no custom VM).
- [ ] Keep client IA ready for `machine → agent → session` without requiring multiple VNC backends yet.

### Phase 1 — multi-user smoke (office Mac, manual)

- [ ] Create users `mob-a` / `mob-b`; enable Screen Sharing.
- [ ] From a client, **Log In** (not Share Display) to each user; confirm two persistent desktops.
- [ ] Per-user TCC for Accessibility + Screen Recording for `mobd`.
- [ ] Two VNC/WS endpoints; App (or svnc) switches preview by URL.
- [ ] Shared folder under `/Users/Shared/mob-workspace` visible to both.

### Phase 2 — Tart/UTM spike (Apple Silicon host preferred)

- [ ] Pin Tart (or UTM) version; bake one macOS guest image with VNC + MOB tooling.
- [ ] `tart run` two clones; mount the same host project directory into both.
- [ ] Reachability: per-VM HTTP `mobd` `/health` + VNC/WS preview.
- [ ] Parallel GUI: two agents click different apps without focus fights.
- [ ] Reset drill: delete/reclone one VM; shared host files survive.
- [ ] Resource envelope: measure RAM/CPU for 2 and 4 concurrent guests on the office Mac.

### Phase 3 — orchestration design (docs → optional ADR)

- [ ] Host-side API or scripts: create/stop/assign agent slots.
- [ ] How chat sessions bind to slots; idle eviction; secret injection.
- [ ] Whether control plane is one host `mobd` multiplexing runners, or one `mobd` per guest.
- [ ] Product packaging: keep Tart/UTM as **documented dependencies**, not vendored hypervisors.

---

## 9. Decisions / non-goals

| Decision | |
| --- | --- |
| MVP desktop model | **Single desktop + global queue** (already in mobd v3) |
| Parallel GUI later | Multi-user Screen Sharing **or** Tart/UTM — pick after smoke tests |
| Fork Tart / build hypervisor | **No** |
| Custom websockify in MVP | **No** (fold relays later if packaging needs it) |
| Same-desktop “soft” isolation (Spaces only) | **Rejected** for parallel agents |

---

## 10. Open questions

1. One host-level `mobd` dispatching into guests vs `mobd` per guest (API surface and auth).
2. Whether agent slots are first-class in Settings UI before any second desktop exists.
3. macOS guest image distribution and update channel for a small team.
4. File locking / editor conventions on the shared mount.
5. Whether iPhone client needs slot switching in the same milestone as home Mac.

---

## 11. References

- [architecture.md](architecture.md) — control / view / actuation
- [roadmap.md](roadmap.md) — phased delivery; VM work is post-MVP planning
- [apps-mob-ui.md](https://github.com/xrdavies/mob/blob/main/docs/ui.md) — machine / session / preview IA
- [vnc-svnc-setup.md](vnc-svnc-setup.md) — office VNC + svnc
- [adr/0010-vnc-relay-in-mobd.md](adr/0010-vnc-relay-in-mobd.md) — VNC WebSocket relay in mobd
- Upstream: [cirruslabs/tart](https://github.com/cirruslabs/tart), [utmapp/UTM](https://github.com/utmapp/UTM) (use as dependencies; do not fork for MOB core)
