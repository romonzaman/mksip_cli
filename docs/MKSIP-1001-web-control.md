# MKSIP-1001 — Browser control for the SIP client

**Issue:** [romonzaman/mksip_cli#1](https://github.com/romonzaman/mksip_cli/issues/1)
**Branch:** `MKSIP-1001-web-control`
**Status:** Plan — not yet implemented

---

## 1. Goal

The client is driven from a terminal REPL today. This adds a small embedded web server so the
same client can be driven from a browser, covering the five capabilities named in the issue:

| # | Issue item | Meaning here |
|---|---|---|
| 1 | Monitor Channel Status | Live view of registration and both channels: state, remote party, direction, duration, hold, mute, codec |
| 2 | Dialpad | Place a call, and send DTMF digits during an established call |
| 3 | Option to transfer call | Attended (`xfer`) and blind (`bxfer`) transfer, plus abandoning a consultation |
| 4 | Hangup Call | End a call on either channel, or all |
| 5 | Answer/Reject Incoming call | Inbound call surfaced immediately, with answer and reject actions |

Plus the operations these depend on to be usable: hold/retrieve, swap, and mute.

---

## 2. Decisions

These were settled before planning because each one changes the shape of the work.

| Decision | Choice | Why |
|---|---|---|
| **Audio** | Stays on the host | The browser is a control surface. Mic and speakers remain on the machine running the client, exactly as today. This matches the issue's wording and keeps the feature self-contained. |
| **Live updates** | WebSocket (`github.com/coder/websocket` v1.8.15) | One bidirectional connection carries both state pushes and commands. The library has **zero dependencies** and a context-aware API, so it adds nothing to the dependency tree. |
| **Access** | `127.0.0.1` only, no auth | Anyone who can reach this server can place calls on your extension. Binding loopback means only someone already on the machine can. No token to manage, no accidental exposure. |

### 2.1 Consequence of the audio decision

The browser cannot hear or speak. A warm transfer driven from the browser still requires the
operator to be at the machine to talk to the consulted party. This is worth stating plainly
in the UI itself, so the page is not mistaken for a softphone.

If browser audio is wanted later it is a separate epic: RTP↔WebRTC bridging means DTLS-SRTP,
ICE, and a jitter/transcode path. Nothing in this plan blocks it, but nothing here anticipates
it either.

---

## 3. Non-goals

- Talking or listening through the browser (§2.1).
- Authentication, TLS, or remote access. Loopback only.
- Replacing the REPL. Both run at once and stay in sync.
- Multi-account, call history, or a phonebook.
- Any change to SIP or media behaviour. This issue adds a control surface over the existing
  `channel.Manager`; if it changes call handling, something has gone wrong.

---

## 4. Architecture

```
                      ┌───────────────────────────────┐
   browser ──ws/http──▶│  internal/web (new)           │
                      │  handlers, ws hub, go:embed UI │
                      └───────────────┬───────────────┘
                                      │
   terminal ──────────▶┌──────────────▼───────────────┐
                      │  internal/control (new)        │
                      │  the operations, no I/O        │
                      └───────────────┬───────────────┘
                                      │
                      ┌───────────────▼───────────────┐
                      │  channel.Manager · transfer   │
                      │  sipua · media · audio        │   unchanged
                      └───────────────────────────────┘
```

Two new packages. Everything below them is untouched.

### 4.1 Why `internal/control` exists

Command logic lives in `internal/cli/commands.go` today, mixed with argument parsing and
terminal printing. For example `dial` resolves the target URI, picks a channel, makes it
active (auto-holding the other), and then dials.

If the web server called `channel.Manager` directly it would have to re-implement that
selection and auto-hold logic, and the two copies would drift — the browser and the terminal
would then behave differently for the same action, which is the worst outcome for a tool whose
job is testing call control.

So the operations move down into `internal/control`, returning values and errors and printing
nothing:

```go
type Controller struct { /* cfg, ua, manager, transferor */ }

func (c *Controller) Dial(ctx context.Context, target string, channelID int) (int, error)
func (c *Controller) Answer(ctx context.Context, channelID int) error
func (c *Controller) Reject(ctx context.Context, channelID, code int) error
func (c *Controller) Hangup(ctx context.Context, channelID int, all bool) error
func (c *Controller) Hold(ctx context.Context, channelID int, hold bool) error
func (c *Controller) Swap(ctx context.Context) error
func (c *Controller) AttendedTransfer(ctx context.Context, transferee, consult int) (transfer.Result, error)
func (c *Controller) BlindTransfer(ctx context.Context, channelID int, target string) (transfer.Result, error)
func (c *Controller) CancelConsult(ctx context.Context, consultID int) error
func (c *Controller) SendDTMF(channelID int, digits string) error
func (c *Controller) SetMuted(muted bool)
func (c *Controller) Snapshot() State
```

`internal/cli` becomes a parser and printer over this; `internal/web` becomes a JSON layer over
the same. Both surfaces then cannot disagree.

This is a refactor of existing, tested code — the scenario suite covers every one of these
operations, so it is a safe change with immediate proof it worked.

### 4.2 Event fan-out

`channel.Manager` publishes asynchronous events on a single channel:

```go
func (m *Manager) Events() <-chan Event   // one consumer
func (m *Manager) SetOnChange(f func())   // one callback
```

The REPL consumes both today. With a second consumer the browser and the terminal would
**steal events from each other** — an incoming call would appear in one and not the other.

So `Manager` gains a subscriber registry:

```go
func (m *Manager) Subscribe() (<-chan Event, func())  // returns an unsubscribe
func (m *Manager) AddChangeListener(f func()) func()
```

with the existing single-consumer methods removed. Each subscriber gets its own buffered
channel; a subscriber that falls behind drops events rather than blocking the manager, and the
drop is logged. State changes are idempotent, so a dropped change event costs nothing — the
next state push carries the truth.

---

## 5. Protocol

### 5.1 HTTP

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | The UI (single embedded HTML page) |
| GET | `/static/*` | Embedded CSS/JS via `go:embed` |
| GET | `/ws` | WebSocket upgrade |
| GET | `/api/state` | One-shot state, for a page load before the socket is up |
| GET | `/healthz` | Liveness, for scripts |

No REST command endpoints: commands go over the socket so every action's result and the
resulting state arrive on the same ordered stream.

### 5.2 WebSocket messages

Every frame is a JSON object with a `type`. Server→client:

```jsonc
// Full state. Sent on connect and after every change.
{ "type": "state",
  "registration": { "state": "registered", "aor": "sip:1004@mkzaman",
                    "server": "172.28.0.10:5060", "transport": "tcp",
                    "refreshInSeconds": 134 },
  "muted": false,
  "activeChannel": 1,
  "channels": [
    { "id": 1, "state": "CONNECTED", "remote": "1001", "inbound": true,
      "active": true, "durationSeconds": 18, "localHold": false,
      "remoteHold": false, "codec": "PCMU", "rtpPort": 50274 },
    { "id": 2, "state": "IDLE" }
  ] }

// A notification, mirroring what the REPL prints above its prompt.
{ "type": "event", "kind": "incoming", "channel": 1, "text": "from 1001" }

// The outcome of a command the client sent.
{ "type": "result", "id": "c7", "ok": false,
  "error": "channel 2 is IDLE, cannot hold" }
```

Client→server:

```jsonc
{ "type": "command", "id": "c7", "action": "hold", "channel": 2 }
{ "type": "command", "id": "c8", "action": "dial", "target": "1001", "channel": 1 }
{ "type": "command", "id": "c9", "action": "dtmf", "channel": 1, "digits": "1234#" }
{ "type": "command", "id": "c10", "action": "xfer", "transferee": 1, "consult": 2 }
```

`action` values mirror the REPL verbs exactly: `dial`, `answer`, `reject`, `hangup`, `hold`,
`unhold`, `swap`, `xfer`, `bxfer`, `cancelxfer`, `dtmf`, `mute`, `unmute`.

Rules:
- Every command carries a client-generated `id`; every command gets exactly one `result`.
- A command that fails returns the **same error text the REPL prints**, because those messages
  already name the channel and state (FR-9.6) and are the product of real debugging.
- Long-running actions (`dial`, `xfer`) return a `result` when the operation is *accepted* or
  fails to start; the outcome arrives as `event` and `state` frames, exactly as in the REPL.
- The server pushes `state` on every change; the client never polls.

---

## 6. UI

One page, no build step. Vanilla JS and CSS embedded with `go:embed` — the deliverable stays a
single binary, which is the whole appeal of the tool.

```
┌──────────────────────────────────────────────────────────┐
│  REG ok · sip:1004@mkzaman · 172.28.0.10:5060 tcp        │
│  ⚠ audio is on the host machine, not in this browser      │
├───────────────────────────┬──────────────────────────────┤
│ Channel 1        ● ACTIVE │ Channel 2                    │
│ CONNECTED ← 1001    00:18 │ IDLE                         │
│ PCMU                      │                              │
│ [Hold] [Hangup]           │ [Dial…]                      │
├───────────────────────────┴──────────────────────────────┤
│  Dialpad   [1][2][3]   target: [______]  [Dial]          │
│            [4][5][6]   during a call the keys send DTMF   │
│            [7][8][9]                                     │
│            [*][0][#]                                     │
├──────────────────────────────────────────────────────────┤
│  [Swap]  [Transfer 1→2]  [Blind transfer…]  [Mute]       │
└──────────────────────────────────────────────────────────┘
```

Behaviour worth specifying:
- **Incoming call** takes over visually — the channel card turns into Answer / Reject buttons
  and the tab title changes, since an inbound call is the one thing the operator must not miss.
- **Buttons reflect state**: hold is disabled on an idle channel, transfer is disabled unless
  two channels are established. The server still validates everything; the UI disabling is a
  convenience, not the check.
- **Connection loss** is visible. If the socket drops, the page greys out and says so rather
  than showing stale state, and reconnects with backoff.

---

## 7. Configuration

A new optional section. Absent means the server is off, so existing configs behave exactly as
before:

```jsonc
"web": {
  "enabled": true,
  "listen_address": "127.0.0.1",
  "port": 8080
}
```

| Field | Default | Notes |
|---|---|---|
| `enabled` | `false` | Off unless asked for. |
| `listen_address` | `127.0.0.1` | Loopback. Validation MUST reject a non-loopback address for now, rather than silently exposing call control to the network. |
| `port` | `8080` | `0` picks a free port, as the SIP and RTP ports already do. |

Flags: `-web` enables it and `-web-port` overrides the port, for a quick run without editing
config.

The chosen URL is printed at startup and shown by the `status` command, since with port `0`
the operator cannot guess it.

---

## 8. Security

The threat is straightforward: this endpoint can place and transfer calls on your extension.

- Bind loopback, and refuse to bind anything else (§7). A future LAN mode needs a token, and
  that is a separate issue.
- Reject cross-origin WebSocket upgrades (`OriginPatterns` restricted to the listen host), so a
  page you visit in the same browser cannot drive your phone.
- The config's SIP password must never reach the browser. `/api/state` returns no credentials;
  the existing `Redacted()` path is the model.
- No shell, file, or config mutation over the API. It exposes call control and nothing else.

---

## 9. Testing

The existing `internal/testpbx` makes this properly testable end to end — the same fake
registrar and UAS the scenario suite already drives.

| Layer | Approach |
|---|---|
| `internal/control` | Inherited: the existing scenario tests are rewritten to drive the controller, so the refactor is proven by tests that already pass. |
| Event fan-out | Unit test: two subscribers both receive every event; a slow subscriber drops without blocking the manager or the other subscriber. |
| HTTP/WS handlers | `httptest.NewServer` + a real WebSocket client, against a live client wired to `testpbx`: connect, assert the first `state` frame, send `dial`, assert `result` then the `state` transitions to CONNECTED. |
| Full path | A scenario test that performs an **entire warm transfer over the WebSocket** — dial, hold, dial, xfer — and asserts the PBX saw the correct `REFER` with `Replaces`, exactly as `TestWarmTransfer` does today. |
| UI | Not automated. The page is small and its logic is state rendering; the protocol underneath is what carries the risk, and that is covered. |

`make check` must stay green throughout.

---

## 10. Work breakdown

Ordered so that each step is independently verifiable, and the risky refactor comes first while
the test suite is the only consumer.

| # | Task | Deliverable | Covers |
|---|---|---|---|
| 1 | Event fan-out in `channel.Manager` | `Subscribe`/`AddChangeListener`, REPL migrated, unit tests for two subscribers and a slow one | prerequisite |
| 2 | Extract `internal/control` | Controller with the operations; `internal/cli` reduced to parsing and printing; whole suite still green | prerequisite |
| 3 | Web server skeleton | `internal/web`, config section, `-web` flag, `/healthz`, `/api/state`, startup URL log | — |
| 4 | WebSocket hub | `/ws`, state pushes, event relay, command dispatch with `result` frames; handler tests | 1 |
| 5 | UI: channel status | Embedded page rendering both channels live | **issue #1** |
| 6 | UI: dialpad | Dial, and DTMF during a call | **issue #2** |
| 7 | UI: hangup, answer, reject | Inbound call takeover in the UI | **issue #4, #5** |
| 8 | UI: transfer | Attended, blind, cancel consult, hold/swap/mute | **issue #3** |
| 9 | End-to-end test | Full warm transfer driven over the WebSocket against `testpbx` | all |
| 10 | Docs | README section, `requirements.md` §9 companion for the web surface, `config.json.example` | — |

Tasks 1 and 2 touch existing code; 3–10 are additive.

---

## 11. Acceptance criteria

| # | Scenario | Expected |
|---|---|---|
| W1 | Start with `web.enabled` false | No listener, no behaviour change. Existing configs unaffected. |
| W2 | Start with `-web` | URL logged and shown by `status`; page loads; first `state` frame matches `status`. |
| W3 | Terminal and browser open together | An action in one is reflected in the other within one state push. Neither steals the other's events. |
| W4 | Dial from the dialpad | Channel goes CALLING → CONNECTED in the browser; the REPL agrees. |
| W5 | Inbound call | Appears in the browser without interaction; Answer connects; Reject declines with 603. |
| W6 | DTMF from the dialpad during a call | Digits arrive at the PBX exactly once each (the `TestDTMFDigitsArriveOnce` bar). |
| W7 | Warm transfer from the browser | Hold, consult, transfer; PBX sees the correct `REFER` with `Replaces`; both channels clear. |
| W8 | Invalid action | `result` carries the same message the REPL gives, e.g. `channel 2 is IDLE, cannot hold`. |
| W9 | Server killed / socket dropped | Page greys out, says disconnected, reconnects and recovers full state. |
| W10 | Non-loopback `listen_address` | Startup fails with a message naming the field. |
| W11 | Cross-origin WS upgrade | Rejected. |
| W12 | `make check` | Clean, including `-race` with the client instrumented. |

---

## 12. Risks

| Risk | Mitigation |
|---|---|
| The `control` extraction changes CLI behaviour subtly | Do it first, with the existing scenario suite as the oracle; no behaviour change is allowed to be invisible. |
| Event fan-out introduces a deadlock or leak | Non-blocking sends with drop-and-log; `-race` and a leak check on subscribe/unsubscribe. |
| Two surfaces racing on the same channel | None needed at the state layer: `channel.Manager` is already mutex-guarded and the scenario suite runs race-instrumented. Concurrent conflicting commands resolve to whichever wins the lock, and both surfaces then see the same pushed state. |
| Scope creep into a softphone | §2.1 and §3 are explicit; the UI says so on the page. |

---

## 13. Open questions

- **Q1** — Should the browser be able to toggle SIP `debug` and view the trace live? It is a
  natural fit for a debugging tool and cheap over the same socket, but it is not in the issue.
  Deferring unless asked.
- **Q2** — Should the dialpad offer a recent-calls list? Needs call history, which does not
  exist today. Out of scope unless asked.
- **Q3** — Port `8080` is a common conflict. Is a less popular default (e.g. `8722`) preferred,
  given `0` is available for "pick one"?
