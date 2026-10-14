# Research: office Mac computer-use / MCP for MOB

> Historical product plan from before the repository split. Current server setup is in
> [README](../README.md); desktop code and build commands are in
> [xrdavies/mob](https://github.com/xrdavies/mob).

> Current implementation (2026-10-15): `internal/actuate` and `internal/mcpserver`
> in this mobd repository provide `mobd mcp` ([ADR 0008](adr/0008-actuation-in-mobd.md)).
> The Python prototype and external-project metrics below are dated research.
> Install the current binary through the [release guide](releases.md#4-安装和升级).

**Date:** 2026-10-12 (Asia/Shanghai calendar; GitHub timestamps below are UTC)
**Repo:** [xrdavies/mob](https://github.com/xrdavies/mob) (MIT)
**Question:** How should Codex on the office Mac actuate the real desktop via MCP — without depending on unmaintained or license-hostile third-party servers?

**Outcome:** Prefer an **in-repo** thin MCP (`services/mob-mcp/`). ADR 0006 (ControlMCP-first) is **superseded** by ADR 0007. Metrics below were fetched from the GitHub API / raw LICENSE files on 2026-10-12; do not treat them as evergreen.

---

## 1. Decision criteria

| Criterion | Why it matters for MOB |
| --- | --- |
| Maintenance | Avoid repeating the ControlMCP failure mode (no stars, short burst of commits, then silence). |
| License | MOB is **MIT**. Bundling or forking **GPL-3.0** into the product tree is a compliance problem. **FSL** “Competing Use” clauses conflict with shipping MOB as a productized computer-use stack. Prefer MIT/Apache-2.0. |
| macOS quality | Office machine is a **real Mac** with real apps — not a Linux Docker desktop. Need Accessibility + Screen Recording (and typically ScreenCaptureKit / CGEvent or equivalent). |
| Codex / MCP stdio | Phase 1 runner is **Codex**. Prefer a **stdio** MCP server Codex can spawn via config (`command` + `args`). |
| Security | TCC prompts; least privilege; auditability; human viewOnly VNC stays separate ([ADR 0001](adr/0001-screen-view-vnc-svnc.md), [security](security.md)). |
| Phase 1 effort | Enough tools for a closed loop in days/weeks, not a full AX suite on day one. |

---

## 2. Option A — Build in-repo thin MCP

**Idea:** Own `services/mob-mcp/` under MOB’s MIT license. Expose a small stdio tool surface. Implement with:

- **Capture:** `screencapture` CLI and/or ScreenCaptureKit (Swift helper or PyObjC).
- **Input:** CGEvent / Accessibility (`AXUIElement`) and/or a thin wrap of mature CLIs (`cliclick`, `osascript`) behind a stable MCP facade.
- **Windows/apps:** Quartz window list, `open -a`, AppleScript as needed.

| Pros | Cons |
| --- | --- |
| License and roadmap under MOB control | We maintain TCC edge cases and Retina coordinate scaling |
| Fits Codex stdio and `mobd` config injection | Phase 1 quality will lag mature Swift projects initially |
| Can harden (confirm gates, allowlists) for office intranet | Need someone comfortable with macOS automation APIs |
| No Competing Use / GPL contamination | |

**Effort (Phase 1 MVP):** roughly **3–8 engineering days** for screenshot + click + type + key + list_windows/open_app, with smoke tests on one office Mac — assuming Python + CLI helpers first, not a full background-input ladder.

**Language recommendation for MVP:** **Python 3.11+** + official MCP Python SDK (or equivalent), subprocess to OS tools where safe; optional later **Swift** helper for ScreenCaptureKit / AX when quality demands it. Keep the MCP JSON schema stable so the implementation language can change.

---

## 3. Option B — Anthropic computer use / official patterns

**Sources (checked 2026-10-12):**

- Anthropic docs: [Computer use tool](https://docs.anthropic.com/en/docs/agents-and-tools/computer-use) / [platform computer-use tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/computer-use-tool) — model emits member tools such as `screenshot`, `left_click`, `type`; **your application executes** each call.
- Reference code: [anthropics/claude-quickstarts](https://github.com/anthropics/claude-quickstarts) — **17 899★**, MIT, last push **2026-10-08T17:34:23Z** (API). Includes `computer-use-demo` and macOS-oriented `computer-use-best-practices`.
- Claude Code: built-in `computer-use` MCP on **macOS**, Pro/Max, interactive sessions ([Claude Code computer use docs](https://code.claude.com/docs/en/computer-use)) — **not** a portable Codex dependency.

| Pros | Cons for MOB Phase 1 |
| --- | --- |
| Best-documented action vocabulary and safety guidance | Designed around **Claude** tool loops / Claude Code — not Codex-first |
| Strong reference for tool naming and confirm flows | Hosted tool schema ≠ drop-in stdio MCP for arbitrary runners |
| MIT quickstarts to learn from | Office runner decision is Codex ([architecture](architecture.md)); swapping to Claude-only CU regresses ADR direction |

**Verdict:** **Study patterns** (tool names, batching, safety). **Do not** make Anthropic computer use the Phase 1 actuation runtime for MOB.

---

## 4. Option C — Active open-source desktop MCP / computer-use (macOS)

Live GitHub API snapshot **2026-10-12**. “Stale” = no push for ~4+ months relative to that date, or zero community signal.

### 4.1 Stronger / more active (consider as reference or optional spike)

| Repo | ★ | Forks | License | Last push (UTC) | Lang | macOS / notes |
| --- | --- | --- | --- | --- | --- | --- |
| [CursorTouch/MacOS-MCP](https://github.com/CursorTouch/MacOS-MCP) | **202** | 32 | **MIT** | **2026-10-07** | Python | Local Mac automation; stdio + SSE/HTTP; Accessibility + Screen Recording; tools include Click, Type, Scroll, Move, Shortcut, App, Shell/AppleScript, Wait; Snapshot/coordinate workflow documented in README. Releases e.g. `v0.4.6` (2026-09-08). ~7 contributors (API). **Best third-party MIT local MCP found.** |
| [minghinmatthewlam/computer-use-mcp](https://github.com/minghinmatthewlam/computer-use-mcp) | **47** | 4 | **MIT** | **2026-08-17** | Swift | Explicit **Codex** `config.toml` example; ScreenCaptureKit + AX; background-safe input ladder; stdio `serve`. Release `v0.4.0` (2026-07-03). Strong engineering; single primary maintainer. |
| [AdelElo13/mac-control-mcp](https://github.com/AdelElo13/mac-control-mcp) | **16** | 3 | **MIT** | **2026-09-15** | Swift | Large tool surface; notarized `.mcpb`; CI on macos-15; release `v0.10.0`. Good quality signal; small community. |

### 4.2 Weaker / wrong shape / flag as dead or provisional

| Repo | ★ | Forks | License | Last push (UTC) | Flag |
| --- | --- | --- | --- | --- | --- |
| [nix18/ControlMCP](https://github.com/nix18/ControlMCP) | **0** | 0 | **GPL-3.0** | **2026-03-27** | **Reject as dependency.** Zero stars/forks; no GitHub Releases; ~6.5 months without push; **GPL-3.0** vs MOB MIT. Previously ADR 0006 default — superseded. |
| [YV17labs/GhostDesk](https://github.com/YV17labs/GhostDesk) | **153** | 18 | **FSL-1.1-ALv2** (`NOASSERTION` on API) | **2026-09-23** | Actively maintained; native macOS path exists. **License:** Competing Use forbids commercial products that substitute for GhostDesk — **poor fit to ship inside MOB**. Prefer not to bundle. |
| [baryhuang/mcp-remote-macos-use](https://github.com/baryhuang/mcp-remote-macos-use) | **490** | 56 | **MIT** | **2025-06-10** | Popular but **remote Screen Sharing/VNC** oriented (often Docker on the *client*). Last push **~16 months** before research date — **stale** for new adoption. Architecture overlaps MOB’s separate VNC *view* path incorrectly. |
| [antbotlab/mac-use-mcp](https://github.com/antbotlab/mac-use-mcp) | **7** | 2 | **MIT** | **2026-05-01** (repo); recent commits in sample were **2026-03** docs-heavy | Small; `npx` TypeScript; treat as **provisional**, not primary. |
| [senturySH/macos-computer-use-mcp](https://github.com/senturySH/macos-computer-use-mcp) | **0** | 0 | none listed | **2026-03-31** | **Dead / unvetted** — skip. |
| [RuoqiJin/mac-auto-bridge](https://github.com/RuoqiJin/mac-auto-bridge) | **1** | 0 | MIT | **2026-10-07** | Fresh but tiny; reference only. |
| [johnexzy/macos-computer-use](https://github.com/johnexzy/macos-computer-use) | **1** | 0 | MIT | **2026-09-19** | Tiny; reference only. |
| [d-wwei/macos-desktop-control](https://github.com/d-wwei/macos-desktop-control) | **3** | 0 | MIT | **2026-04-01** | Uses **cliclick** + AppleScript foreground / CGEvent background — useful *pattern*, weak maintenance signal. |
| [klemensms/mcp-computer-use](https://github.com/klemensms/mcp-computer-use) | **0** | 0 | MIT | **2026-06-30** | Beta; skip as dependency. |

---

## 5. Option D — GhostDesk and “full desktop product” MCPs

[GhostDesk](https://github.com/YV17labs/GhostDesk) (★★153, push 2026-09-23) is the most productized open MCP desktop stack reviewed: Docker Linux virtual desktop **or** native macOS/Windows binary; MCP over Streamable HTTP; recent releases (e.g. v7 line in 2026).

**Why MOB should not depend on it as core actuation:**

1. **FSL-1.1-ALv2** Permitted Purpose excludes **Competing Use** — making available a commercial product/service that substitutes for GhostDesk or offers substantially similar functionality. MOB’s office agent + desktop control is squarely in that risk zone if we redistribute or wrap GhostDesk as the product’s hands.
2. Default narrative is still **Linux container**; macOS is “the desk in front of you” **without sandbox** — same TCC burden as any local MCP, plus their stack.
3. Transport is primarily **HTTP MCP**, while Codex Phase 1 prefers simple **stdio** spawn (HTTP is possible later, not the thinnest path).

**Verdict:** Optional **internal experiment only** after legal review — **not** Phase 1 default, **not** vendored into MOB.

---

## 6. Option E — OS tools behind our MCP facade

Mature building blocks (not MCP servers themselves):

| Tool / API | Role | Notes |
| --- | --- | --- |
| `screencapture` | Screenshots | Built into macOS; Retina scale must be handled in coordinates. |
| ScreenCaptureKit | Modern capture | Best long-term; usually Swift. |
| Accessibility (`AXUIElement`) | Semantic UI | Required for robust automation; TCC Accessibility. |
| CGEvent / Quartz | Mouse/keyboard | Core for clicks/keys; coordinate spaces matter. |
| [BlueM/cliclick](https://github.com/BlueM/cliclick) | CLI mouse/keyboard | **2023★**, last push **2025-08-23**; GitHub license field `NOASSERTION` — verify license file before bundling. Fine as **optional** PATH dependency for MVP. |
| `osascript` / AppleScript | App/window helpers | Good for `open`, menus; brittle for arbitrary UI. |
| `open -a` | Launch apps | Simple and reliable for MVP. |

**Pattern:** MCP server = policy + schema + logging; OS tools = actuators. This is how several small Node/Python MCPs work (e.g. cliclick-based servers) and is acceptable for Phase 1 if wrapped **in-repo**.

---

## 7. Comparison matrix (Phase 1)

| Option | Maintenance | License fit | macOS real-desk | Codex stdio | Security control | Phase 1 effort | Score for MOB |
| --- | --- | --- | --- | --- | --- | --- | --- |
| **In-repo `mob-mcp`** | Us | MIT (ours) | Yes | Yes | Full | Medium | **Primary** |
| CursorTouch/MacOS-MCP | Good (2026-10) | MIT | Yes | Yes | Upstream | Low if adopted as-is | Strong **reference** / emergency spike only |
| minghinmatthewlam/computer-use-mcp | Moderate | MIT | Yes (excellent) | Yes (docs) | Upstream | Low–med | Reference / optional eval |
| AdelElo13/mac-control-mcp | Moderate | MIT | Yes | Yes | Upstream | Low–med | Reference |
| Anthropic CU API / Claude Code CU | Vendor | N/A / product lock-in | Yes (Claude path) | Poor fit for Codex | Vendor + us | Wrong runner | Patterns only |
| GhostDesk | Good | **FSL Competing Use** | Yes (native) | HTTP-first | Shared | Med | **Avoid as dependency** |
| ControlMCP | **Poor** | **GPL-3.0** | Yes | Yes | Upstream | Low but toxic | **Reject** |
| Remote-macos-use | Stale | MIT | Remote VNC shape | Docker-heavy | Shared | Wrong arch | **Reject for local actuation** |

---

## 8. Recommendation (one primary path)

### Primary: **Build `services/mob-mcp/` in-repo (MIT), Python MVP**

**Rationale:**

1. **ControlMCP is disqualified** on maintenance *and* **GPL-3.0** — keeping ADR 0006 would recreate the exact failure the project already hit.
2. **GhostDesk is disqualified as a shipped dependency** under FSL Competing Use for a productized MOB.
3. Best MIT third parties (**MacOS-MCP**, **computer-use-mcp**) are useful **references** but are still young / single-vendor risks; MOB needs long-term ownership for office private deployment, audit hooks, and runner integration.
4. Anthropic computer use is the wrong **runtime** for a **Codex-first** office runner (keep as design literature).
5. A **thin** MCP with a stable tool schema matches ADR 0003 (local actuation) and lets `mobd` inject `command`/`args` without hard-coding a third-party binary forever.

### Phase 1 MVP tool list

Minimum (Codex must complete “open app + visible UI change”):

| Tool | Purpose |
| --- | --- |
| `screenshot` | Full display (or primary) PNG/JPEG; return size + scale factor |
| `click` | `x`, `y`, button (`left`/`right`), optional click count |
| `type_text` | Unicode string to focused element / via CGEvent or paste fallback |
| `key` | Key + modifiers (e.g. `cmd+c`, `return`, `tab`) |
| `list_windows` | App name, title, bounds, PID (for targeting / debugging) |
| `open_app` | Launch by name or bundle id |

Defer to Phase 1.5+: scroll/drag, AX tree dump, OCR, background-only input ladder, confirm policies for destructive actions (steal ideas from computer-use-mcp / Anthropic guides).

### Proposed repo layout

```text
mob/
  cmd/mobd/                    # Go control plane (existing plan)
  internal/...
  services/
    mob-mcp/                   # NEW — office actuation MCP (MIT)
      README.md
      pyproject.toml
      src/mob_mcp/
        __init__.py
        server.py              # stdio MCP
        tools/
          screenshot.py
          input.py             # click, type_text, key
          windows.py           # list_windows, open_app
      tests/
  docs/
    research-computer-use.md   # this file
    adr/0007-actuation-mob-mcp.md
```

`mobd` / Codex config example (illustrative):

```toml
[mcp_servers.mob-mcp]
command = "uv"
args = ["run", "--directory", "/path/to/mob/services/mob-mcp", "mob-mcp"]
```

### What NOT to do

- Do **not** add `nix18/ControlMCP` as a submodule, vendored tree, or documented default install.
- Do **not** ship or rebrand **GhostDesk** as MOB’s hands without a commercial license review (FSL Competing Use).
- Do **not** make Anthropic / Claude Code computer-use the Phase 1 office runtime while the runner is Codex.
- Do **not** drive the human **viewOnly** VNC session for actuation (keep ADR 0001 / 0003 split).
- Do **not** expand MVP to 50+ AX tools before the dispatch → Codex → visible UI → svnc loop works.
- Do **not** treat star count alone as safety — still read license and last-push; prefer in-repo ownership for the critical path.

### Optional parallel (non-blocking)

Office engineers may **temporarily** spike Codex against [CursorTouch/MacOS-MCP](https://github.com/CursorTouch/MacOS-MCP) (MIT, active as of 2026-10-07) to validate permissions and prompts **while** `mob-mcp` is built — document it as a **throwaway** smoke aid, not a product dependency.

---

### Related later: per-agent desktop isolation

Actuation assumes **one** GUI session per driver. Parallel agents on one WindowServer will fight the cursor; future isolation (multi-user or Tart/UTM) is planned in [research-per-agent-desktop-vm.md](research-per-agent-desktop-vm.md), not in the MCP tool surface.

---

## 9. ADR impact

- [ADR 0006](adr/0006-actuation-controlmcp-first.md) (**ControlMCP first**) → **Superseded** by [ADR 0007](adr/0007-actuation-mob-mcp.md).
- [ADR 0003](adr/0003-actuation-local-mcp.md) remains: local MCP actuation, separate from human viewOnly VNC.
- Update [implementation-phase1.md](implementation-phase1.md) and [office-smoke-checklist.md](office-smoke-checklist.md) to target **`mob-mcp`**.

---

## 10. Source checklist (retrieved 2026-10-12)

- GitHub API `GET /repos/{owner}/{repo}` for fields cited in tables (stars, forks, `pushed_at`, `license.spdx_id`).
- Raw LICENSE: ControlMCP GPL-3.0 header; GhostDesk FSL-1.1-ALv2 text (Competing Use); MIT headers for MacOS-MCP, mac-control-mcp, computer-use-mcp.
- Project READMEs: MacOS-MCP tools; computer-use-mcp Codex config; GhostDesk platform table; Anthropic / Claude Code computer-use docs as linked above.
