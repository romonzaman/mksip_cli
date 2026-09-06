# SIP CLI Client — Requirements

**Project:** `sipclient_cli`
**Language:** Go 1.25+
**Status:** Draft v1 — 2026-09-03

---

## 1. Purpose

A single-binary, terminal-based SIP user agent that registers to a PBX using credentials
from a `config.json` file and maintains **two independent call channels**. Two channels are
the minimum needed to perform a **warm (attended) transfer**: hold the first party, consult
privately with the transfer target on the second channel, then join the two together and drop out.

The primary use case is manual and scripted verification of PBX call-control behaviour
(registration, hold, transfer, DTMF) without needing a softphone GUI.

### 1.1 Goals

- G1 — Register/re-register to a SIP PBX and stay registered unattended.
- G2 — Place and receive calls on two independent channels simultaneously.
- G3 — Perform attended (warm), blind, and consultative-cancel transfers from the CLI.
- G4 — Real two-way audio through the operator's microphone and speakers, so the consult
  leg of a warm transfer is an actual conversation.
- G5 — Full SIP signalling trace on disk for debugging PBX interop.

### 1.2 Non-goals (out of scope for v1)

- Video, screen sharing, or presence/BLF.
- TLS/SIPS signalling and SRTP media (see §12, deferred).
- Opus or any wideband codec (deferred).
- Call recording to file, IVR scripting, or conference bridging beyond the 3-way case in FR-7.4.
- More than two channels. The channel model must not *prevent* N channels, but only 2 are required.
- A GUI, web UI, or daemon/IPC mode.

---

## 2. Definitions

| Term | Meaning |
|---|---|
| **Channel** | One of two independently addressable call slots, `1` and `2`. Each owns at most one SIP dialog and one RTP session at a time. |
| **Active channel** | The channel currently connected to the microphone and speakers. At most one at a time (except in 3-way mode, FR-7.4). |
| **Transferee** | The party being transferred (the original caller, typically on channel 1). |
| **Transfer target** | The party the transferee is being sent to (consulted on channel 2). |
| **Transferor** | This CLI client — the one performing the transfer. |
| **Warm / attended transfer** | Transferor speaks to the target before completing the transfer. |
| **Blind / unattended transfer** | Transferor sends the transferee to the target without consulting. |

---

## 3. Configuration

### 3.1 Requirements

- **FR-1.1** — Config path defaults to `./config.json`, overridable with `-config <path>`.
- **FR-1.2** — The file MUST be validated on startup. Any missing required field or invalid
  value MUST abort startup with a message naming the offending JSON path
  (e.g. `sip.server.port: must be 1-65535`). The client MUST NOT start half-configured.
- **FR-1.3** — Unknown JSON fields MUST be rejected (strict decoding), so typos in field
  names surface immediately instead of silently taking a default.
- **FR-1.4** — Every field with a sensible default MAY be omitted; defaults are listed below.
- **FR-1.5** — `sip.password` MUST NOT be written to any log file or printed by any command.
  Where a config dump is shown, it MUST be rendered as `***`.
- **FR-1.6** — The config file's permissions SHOULD be checked; warn (do not fail) if it is
  group- or world-readable, since it holds a plaintext password.
- **FR-1.7** — `sip.password` MAY use the form `env:VAR_NAME` to read the secret from an
  environment variable instead of storing it in the file.

### 3.2 Schema

```json
{
  "sip": {
    "username": "1001",
    "auth_username": "1001",
    "password": "env:SIP_PASSWORD",
    "display_name": "CLI Client",
    "domain": "pbx.example.com",
    "server": {
      "host": "10.0.0.10",
      "port": 5060,
      "transport": "udp"
    },
    "outbound_proxy": "",
    "register_expiry_seconds": 300,
    "session_expires_seconds": 1800,
    "min_se_seconds": 90,
    "user_agent": "sipclient-cli/1.0"
  },
  "network": {
    "local_address": "",
    "local_sip_port": 0,
    "public_address": "",
    "stun_server": "",
    "rport": true
  },
  "media": {
    "codecs": ["PCMU", "PCMA"],
    "ptime_ms": 20,
    "dtmf_mode": "rfc4733",
    "jitter_buffer_ms": 60,
    "rtcp_enabled": true
  },
  "audio": {
    "input_device": "",
    "output_device": "",
    "input_gain": 1.0,
    "output_gain": 1.0,
    "ringback_enabled": true
  },
  "transfer": {
    "mode": "refer",
    "notify_timeout_seconds": 30,
    "hangup_after_success": true
  },
  "logging": {
    "level": "info",
    "file": "sipclient.log",
    "sip_trace": true,
    "sip_trace_file": "sip-trace.log",
    "console_level": "warn"
  }
}
```

### 3.3 Field reference

**`sip`**

| Field | Req. | Default | Notes |
|---|---|---|---|
| `username` | yes | — | AOR user part; the extension to register. |
| `auth_username` | no | `username` | Digest auth username when it differs from the AOR. |
| `password` | yes | — | Plaintext, or `env:VAR` per FR-1.7. |
| `display_name` | no | `username` | Used in the `From` display name. |
| `domain` | yes | — | SIP domain / digest realm host; the AOR is `sip:username@domain`. |
| `server.host` | yes | — | Registrar/proxy IP or hostname. |
| `server.port` | no | `5060` | 1–65535. |
| `server.transport` | no | `"udp"` | `"udp"` or `"tcp"` only in v1. |
| `outbound_proxy` | no | `""` | If set, all requests route here regardless of Request-URI. |
| `register_expiry_seconds` | no | `300` | Requested `Expires`; 60–3600. |
| `session_expires_seconds` | no | `1800` | RFC 4028 session interval to propose. `0` disables session timers. Otherwise 90–86400. |
| `min_se_seconds` | no | `90` | Shortest session interval accepted from a peer; a shorter request is answered `422`. 90–86400, and never above `session_expires_seconds`. |
| `user_agent` | no | `sipclient-cli/<version>` | `User-Agent` header value. |

**`network`**

| Field | Req. | Default | Notes |
|---|---|---|---|
| `local_address` | no | auto | The address advertised in `Via`, `Contact` and SDP. Auto-detected via a route lookup toward `server.host`. This is what peers are told to reach us on, **not** what we bind. |
| `listen_address` | no | all interfaces | The address the signalling sockets bind to. Defaults to `0.0.0.0` so a call routed to this host by any path is accepted; pin it only to restrict which interface accepts SIP. |
| `local_sip_port` | no | `0` (OS-assigned) | The port the signalling sockets bind to, and the port advertised in `Contact`. Zero (the default) lets the OS pick a free one, so the client never collides with a PBX or another SIP process on the same host. Pin it when a firewall rule needs a predictable port. |
| `public_address` | no | `""` | Static NAT override placed in `Contact`, `Via`, and SDP `c=`. |
| `stun_server` | no | `""` | e.g. `stun.l.google.com:19302`. Used to discover the public address when `public_address` is empty. Optional; skip if the PBX is on the LAN. |
| `rport` | no | `true` | Add `;rport` to `Via` and honour the reflected source for responses. |

Transport note (learned the hard way): on **UDP** the client MUST send from the same socket
it listens on, so `Via`, `Contact` and the packet source all agree — otherwise a NAT-aware
PBX replies to a source port that holds no dialog state. On **TCP** it MUST NOT do that: the
listener and outbound connections are separate sockets, and forcing the local address makes
the dialer bind a port the listener already holds, failing with `bind: address already in
use`. TCP dials from an ephemeral port and reuses that connection for the dialog.

**`media`**

| Field | Req. | Default | Notes |
|---|---|---|---|
| `advertise_address` | no | same as signalling | Address for the SDP connection line, when RTP reaches this host by a different path than SIP. The split FreeSWITCH calls `ext-sip-ip` versus `ext-rtp-ip`. |
| `rtp_port_start` / `rtp_port_end` | no | `0` / `0` (OS-assigned) | Omit both and the operating system assigns a free RTP port per call, which needs no configuration and cannot collide. Set both to pin a range for a firewall: ports are then allocated even-numbered with RTCP at `port+1`, and the range MUST hold at least 4 ports for 2 concurrent channels. Setting only one end is an error. |
| `codecs` | no | `["PCMU","PCMA"]` | Offer order = preference order. v1 accepts only `PCMU` and `PCMA`. |
| `ptime_ms` | no | `20` | 20 or 30. Drives packet size (20 ms G.711 = 160 bytes). |
| `dtmf_mode` | no | `"rfc4733"` | `"rfc4733"` (telephone-event), `"info"` (SIP INFO), or `"both"`. |
| `jitter_buffer_ms` | no | `60` | Fixed-delay playout buffer; 0 disables. |
| `rtcp_enabled` | no | `true` | Send/receive RTCP SR/RR. |

**`audio`**

| Field | Req. | Default | Notes |
|---|---|---|---|
| `input_device` | no | system default | Substring match against the device name; ambiguous matches MUST error and list candidates. |
| `output_device` | no | system default | As above. |
| `input_gain` / `output_gain` | no | `1.0` | Linear multiplier, 0.0–4.0, applied with clipping protection. |
| `ringback_enabled` | no | `true` | Generate local ringback tone when the PBX sends `180` without early media. |
| `echo_cancel` | no | `false` | Cancel acoustic echo, so the speakerphone is usable. Off by default, since a headset gives it nothing to cancel. Requires a duplex device; falls back with a warning when one cannot be opened. `-echo-cancel[=false]` overrides it for one run. |
| `echo_tail_ms` | no | `128` | How long an echo path to model, 16–500. Longer covers a more reverberant room but converges more slowly. |

**`transfer`**

| Field | Req. | Default | Notes |
|---|---|---|---|
| `mode` | no | `"refer"` | v1 supports `"refer"` only. Reserved for a future local-bridge mode. |
| `notify_timeout_seconds` | no | `30` | How long to await the final `NOTIFY` after a `REFER` before declaring the transfer's outcome unknown. |
| `hangup_after_success` | no | `true` | Send `BYE` on any leg the PBX has not already torn down once the transfer succeeds. |

**`logging`**

| Field | Req. | Default | Notes |
|---|---|---|---|
| `level` | no | `"info"` | `debug`, `info`, `warn`, `error`. Applies to the log file. |
| `file` | no | `"sipclient.log"` | Appended to; rotation is out of scope. |
| `sip_trace` | no | `true` | Log every SIP message sent/received verbatim. |
| `sip_trace_file` | no | `"sip-trace.log"` | Separate file so the trace does not drown the app log. |
| `console_level` | no | `"warn"` | Kept above `info` by default so log lines do not interleave with the REPL prompt. |

---

## 4. Registration

- **FR-2.1** — On startup, send `REGISTER` to the registrar and handle digest authentication
  (`401`/`407`) per RFC 3261 §22, supporting MD5 and `qop=auth`.
- **FR-2.2** — Re-register at 50% of the *server-granted* `Expires` (from the `200 OK`
  `Contact` or `Expires` header), not the requested value.
- **FR-2.3** — On registration failure, retry with exponential backoff (2s, 4s, 8s … capped
  at 60s) and jitter. Report each failure with the SIP response code and reason.
- **FR-2.4** — Terminal failures (`403 Forbidden`, `404 Not Found`) MUST be reported prominently
  and MUST NOT be retried indefinitely — stop after 3 attempts and require an explicit
  `register` command to retry.
- **FR-2.5** — On clean shutdown, send `REGISTER` with `Expires: 0` to de-register.
- **FR-2.6** — Registration state (`registered` / `registering` / `failed`, with time until
  next refresh) MUST be visible in the status line and via a `status` command.
- **FR-2.7** — Incoming requests MUST be accepted only while registered; when not registered,
  answer `INVITE` with `480 Temporarily Unavailable`.
- **FR-2.8** — The client MUST listen on **both** UDP and TCP regardless of
  `sip.server.transport`. That setting chooses how requests are *sent*; it must not decide
  what can be *received*, because a proxy may route an inbound call over either transport no
  matter how the client registered. Failing to bind the configured transport is fatal;
  failing on the other one is a warning.
- **FR-2.9** — Sockets MUST bind all interfaces by default (see `network.listen_address`). A
  PBX may reach this host by a different route than the one used to reach the PBX, and a
  socket bound to a single address silently refuses everything else.
- **FR-2.11** — A dynamic (`0`) SIP port MUST be resolved to a concrete port *before* the
  Contact is built, and inbound calls MUST work with it: the registrar learns the port from
  each `REGISTER`, so it changes freely between runs. The only residue is that an unclean
  exit (`SIGKILL`, no de-registration) leaves a binding pointing at the dead port until it
  expires; a clean exit removes it (FR-2.5). The client MUST NOT clear *all* bindings for the
  AOR to tidy this up, since another device may legitimately share the extension.
- **FR-2.10** — At startup the client MUST check that the address it advertises can accept
  connections, and warn when it cannot, naming the fix. An address discovered from the route
  to the SIP server is valid as a packet *source* but not necessarily as a *destination* —
  the network address of a subnet (a `.0` host in a `/24`) is the common case and the kernel
  refuses connections to it. Outbound calls and RTP still work, because peers latch onto our
  source address, so nothing looks wrong; but no inbound `INVITE` and no refer `NOTIFY` can
  arrive, since both are sent to `Contact`.

  The structural part of the check (network or broadcast address) MUST always run. The
  dial-based part MUST be skipped when `network.public_address` is set explicitly, because a
  configured NAT or container alias is often reachable from the PBX but not from this host,
  and reporting that as a failure would be wrong.

---

## 5. Channel model

- **FR-3.1** — Exactly two channels exist for the process lifetime, addressed as `1` and `2`.
  A channel is a stable slot: it does not disappear when its call ends.
- **FR-3.2** — Each channel independently holds: one SIP dialog, one RTP/RTCP port pair, one
  jitter buffer, one call state, and its own call metadata (remote party, direction, start time,
  duration, codec in use).
- **FR-3.3** — Channel state machine, per channel:

```
                  ┌──────┐
                  │ IDLE │◄──────────────────────────────┐
                  └──┬───┘                              │
       dial          │            incoming INVITE       │
   ┌─────────────────┴─────────────────┐                 │
   ▼                                   ▼                 │
┌─────────┐                      ┌──────────┐            │
│ CALLING │                      │ RINGING  │            │
└────┬────┘                      └────┬─────┘            │
     │ 180/183 → RINGBACK             │ answer           │
     │ 2xx                            │ 2xx sent         │
     ▼                                ▼                  │
   ┌──────────────────────────────────────┐              │
   │             CONNECTED                │              │
   └───┬──────────────────────────────┬───┘              │
       │ hold (re-INVITE sendonly)    │ BYE / 4xx-6xx    │
       ▼                              │                  │
   ┌────────┐  unhold                 │                  │
   │  HELD  │────────────────────────►│                  │
   └───┬────┘                         │                  │
       │                              ▼                  │
       │                        ┌────────────┐            │
       └───────────────────────►│ TERMINATED ├────────────┘
                                └────────────┘
```

  Additional transient states: `TRANSFERRING` (a `REFER` is outstanding on this channel) and
  `TERMINATED` (a settling state that returns to `IDLE` once media and ports are released).

- **FR-3.4** — Exactly one channel is *active* (mic/speaker attached). Placing a channel on
  hold detaches it; answering or dialling on a channel makes it active and MUST automatically
  place the previously active channel on hold (configurable auto-hold, on by default).
- **FR-3.5** — Every command that acts on a call MUST accept an explicit channel number, and
  MUST default to the active channel when omitted.
- **FR-3.6** — By default RTP ports are assigned by the operating system per call, so no
  range needs configuring and two channels can never collide.

  When a range **is** pinned (`media.rtp_port_start`/`end`), ports MUST be released back to
  the pool on call teardown and MUST NOT be reused until any in-flight RTP for the old call
  has drained (short quarantine, ~1 s), to avoid stray packets landing in a new session.
  A pinned range routinely overlaps something else on the host — a container runtime
  forwarding a PBX's own RTP range is the common case — so a port that fails to bind MUST be
  skipped and excluded from further use rather than failing the call, and exhausting the
  range MUST report that ports are held elsewhere and name the setting to change.

---

## 6. Call control

- **FR-4.1 Outbound call** — `dial <target> [channel]` sends `INVITE` with an SDP offer.
  Target may be a bare extension (`1002`, expanded to `sip:1002@<domain>`) or a full SIP URI.
- **FR-4.2** — Handle `100`, `180` (ring / local ringback), `183` with early media (play it),
  `3xx` (report; do not auto-follow in v1), `401/407` (re-send with credentials),
  `486 Busy`, `487`, and other `4xx`–`6xx` with a clear reason shown to the operator.
- **FR-4.3 Cancel** — `hangup` on a channel in `CALLING` MUST send `CANCEL`, and MUST handle
  the `CANCEL`/`200 OK` race by sending `BYE` if a `2xx` arrives first.
- **FR-4.4 Inbound call** — An `INVITE` for the registered AOR MUST be routed to the first
  `IDLE` channel, `180 Ringing` sent, and the caller's identity displayed. If both channels
  are busy, reply `486 Busy Here`.
- **FR-4.5** — `answer [channel]` sends `200 OK` with the answer SDP; `reject [channel] [code]`
  replies `603 Decline` by default.
- **FR-4.6 Hangup** — `hangup [channel]` sends `BYE` (or `CANCEL`/decline as appropriate for
  the state) and releases media. `hangup all` clears both channels.
- **FR-4.7 Hold / unhold** — `hold [channel]` sends a re-INVITE with the local stream marked
  `a=sendonly` (falling back to `a=inactive` if the peer previously offered `sendonly`), and
  stops sending mic audio. `unhold [channel]` restores `a=sendrecv`. Both MUST handle a
  re-INVITE `491 Request Pending` collision by retrying after a short randomised delay.
- **FR-4.8** — Remote hold (peer re-INVITE with `sendonly`/`inactive`) MUST be detected and
  displayed, and inbound audio treated as absent.
- **FR-4.9 Swap** — `swap` MUST hold the active channel and unhold the other in one command,
  which is the core operator gesture during a warm transfer.
- **FR-4.10 DTMF** — `dtmf <digits> [channel]` sends digits `0-9*#A-D` on the given channel
  using `media.dtmf_mode`. RFC 4733 events MUST use the negotiated `telephone-event` payload
  type, 3 packets per digit with the end bit set on the last, and honour inter-digit gaps.
- **FR-4.11** — Session timers (RFC 4028) MUST be supported, in both directions.

  The failure this prevents is specific: when the PBX names the client as refresher and the
  client never refreshes, the PBX tears the call down at its interval — typically 30 minutes —
  with nothing the user can see. A long call simply ends, and the phone looks blameless.

  | Situation | Required behaviour |
  |---|---|
  | Outbound `INVITE` | Advertise `Supported: timer` and offer `Session-Expires` plus `Min-SE`. `timer` MUST NOT go in `Require`: demanding it fails the call against a PBX that lacks it, and a call without a session timer is still a working call. |
  | Answer names no refresher | The UAC is responsible (RFC 4028 §7.1) — that is us. Getting this backwards is precisely how the call dies. |
  | Answer omits `Session-Expires` | No timer. The client MUST NOT refresh unbidden. |
  | We are refresher | Send a re-INVITE at **half** the interval, so one lost refresh still leaves time for another. A rejected refresh MUST be retried on the next tick, not treated as fatal. |
  | Peer is refresher | Watch for its re-INVITE. If none arrives within the interval plus a grace period, the session is dead at the far end and the client MUST hang up rather than show a call that no longer exists. |
  | Peer's refresh arrives | Answer it, echo the agreed `Session-Expires`, and restart the watchdog. |
  | Inbound `INVITE` below our `Min-SE` | Reject with `422 Session Interval Too Small` carrying our `Min-SE`, rather than silently accepting an interval we will not honour. |
  | Our `INVITE` rejected `422` | Retry **once** with the interval the peer demands. Failing the call instead would make the client unusable against any PBX with a longer minimum. |

  Setting `sip.session_expires_seconds` to `0` disables the feature entirely, restoring the
  earlier behaviour of neither offering timers nor refreshing.
- **FR-4.12** — `INVITE` transactions MUST honour Timer B / Timer F expiry and report a
  timeout rather than hanging in `CALLING` forever.

---

## 7. Transfer

Transfer uses `REFER` (RFC 3515) with the `Replaces` header (RFC 3891) as profiled for
call control by RFC 5589. The PBX performs the actual bridging; this client leaves the call.

### 7.1 Warm / attended transfer

- **FR-5.1** — Command: `xfer [from_channel] [to_channel]`, defaulting to
  `xfer 1 2` semantics — transfer the *other* channel's party to the party on the
  *consult* channel. It MUST be rejected unless both channels are in `CONNECTED` or `HELD`.
- **FR-5.2** — Required flow:

```
  Transferee (A) ──── channel 1 ──── [ CLI client ] ──── channel 2 ──── Target (C)

  1. Channel 1 is CONNECTED with A.
  2. `hold 1`        → re-INVITE A, sendonly. A hears PBX music-on-hold.
  3. `dial C 2`      → INVITE C on channel 2; channel 2 becomes active.
  4. Operator consults with C over mic/speaker (this is the "warm" part).
  5. `xfer`          → REFER sent *on channel 1's dialog* (to A), carrying:
                          Refer-To: <sip:C@domain?Replaces=<call-id-of-ch2>
                                     %3Bto-tag%3D<ch2-to-tag>
                                     %3Bfrom-tag%3D<ch2-from-tag>>
                          Referred-By: <sip:username@domain>
  6. Expect 202 Accepted, then NOTIFY (Event: refer) with a message/sipfrag body.
  7. On sipfrag `200 OK` → transfer succeeded: A and C are now talking directly.
     Tear down both channels (BYE any leg the PBX has not already BYE'd), return to IDLE.
  8. On sipfrag 4xx/5xx/6xx → transfer failed: report the code, leave both calls up,
     and leave channel 1 on hold so the operator can `swap` back and recover.
```

- **FR-5.3** — The `Replaces` parameters MUST be taken from channel 2's dialog identifiers
  and MUST be percent-escaped inside `Refer-To`, since the `;` separators would otherwise
  terminate the header.

  Tag orientation is written from the perspective of the UA whose dialog is being replaced:
  its own tag becomes `to-tag` and ours becomes `from-tag`. Reversing them is the classic
  cause of a `481 Call/Transaction Does Not Exist` from the PBX, so it is covered by a
  dedicated test (§13 A9).

  The URI part of `Refer-To` MUST be the consultation dialog's **remote target** (the peer's
  `Contact`), not the target's AOR. The remote target is guaranteed to reach the entity that
  actually holds the dialog named in `Replaces`; an AOR routed through the proxy may fork or
  reach a different device, which answers `481`.
- **FR-5.4** — Interim `NOTIFY`s (sipfrag `100 Trying`, `180 Ringing`) MUST be surfaced as
  progress, not treated as the final result. Only a final response ends the transfer.
- **FR-5.5** — If no final `NOTIFY` arrives within `transfer.notify_timeout_seconds`, the
  client MUST NOT simply report "unknown": some PBXs and proxies accept a `REFER` with `202`,
  perform the transfer, and never send the refer `NOTIFY` at all.

  It MUST instead probe the dialog that a successful transfer replaces — the consultation
  dialog for an attended transfer, the transferee's own dialog for a blind one — with an
  in-dialog `OPTIONS`, and resolve one of three outcomes:

  | Probe result | Meaning | Action |
  |---|---|---|
  | `481 Call/Transaction Does Not Exist` | The `Replaces` INVITE reached the peer; the transfer took effect. | Release that channel (no `BYE`: it would only earn another `481`). Report success as **inferred**, and leave the remaining leg up so the operator can confirm with the parties. MUST NOT auto-hang up on an inference. |
  | Any other final response | The peer still holds the dialog; nothing was transferred. | Report that the transfer did not take effect. Leave both legs up. |
  | No response, or a transport error | Genuinely undeterminable. | Report **unknown**. Leave both legs up and the operator in control. |

  Observed need: Kamailio 6.0.7 proxying FreeSWITCH 1.10.12 answered `202 Accepted`, sent no
  `NOTIFY`, and silently discarded the consultation dialog — a later `BYE` on it returned
  `481`. Without the probe the client reports "unknown" while showing a dead channel as
  `CONNECTED`, and the operator's next `hangup` fails.
- **FR-5.6** — A `REFER` rejected with `405`, `501`, or `403` MUST produce an explicit
  "PBX does not permit REFER-based transfer" message naming the response code, since this
  is the most likely interop failure.
- **FR-5.7** — While a `REFER` is outstanding, the channel is `TRANSFERRING` and MUST reject
  further transfer or hold commands on that channel.

### 7.2 Blind / unattended transfer

- **FR-6.1** — `bxfer <target> [channel]` sends `REFER` on the given channel's dialog with a
  plain `Refer-To: <sip:target@domain>` and no `Replaces`, then follows FR-5.4 through FR-5.6.
- **FR-6.2** — The local leg MUST be torn down only after a successful final `NOTIFY`, or
  immediately if `transfer.hangup_after_success` is false and the operator asks.

### 7.3 Abandoning a consult

- **FR-6.3** — `cancelxfer` (or simply `hangup 2` then `unhold 1`) MUST cleanly abandon a
  consultation: drop the consult leg and return the transferee to a live, unheld conversation.
  This path MUST be reliable — a failed transfer must never leave the transferee stranded on hold.

### 7.4 Optional: 3-way consult

- **FR-7.4** *(nice to have, not required for v1)* — `conf` MAY unhold both channels
  simultaneously, mixing both inbound streams to the speaker and sending the mic to both,
  so the operator can introduce the two parties before completing the transfer.

---

## 8. Media

- **FR-8.1** — Codecs: G.711 µ-law (`PCMU`, PT 0) and A-law (`PCMA`, PT 8), 8 kHz mono,
  plus `telephone-event` (dynamic PT, offered as 101) for DTMF.
- **FR-8.2** — SDP offer/answer MUST follow RFC 3264: offer the configured codec list; in the
  answer, select the first offered codec that is also configured, and fail the call with
  `488 Not Acceptable Here` if there is no overlap.
- **FR-8.3** — RTP MUST use a random initial sequence number and SSRC, monotonically
  increasing timestamps at 8000 Hz (160 samples per 20 ms frame), and correct marker-bit
  handling on the first packet of a talk spurt.
- **FR-8.4** — A fixed-delay jitter buffer of `media.jitter_buffer_ms` MUST reorder packets by
  sequence number, discard late arrivals, and conceal a missing frame by repeating the previous
  frame at reduced gain (simple PLC).
- **FR-8.5** — Symmetric RTP: after the first inbound packet, send to the observed source
  address/port rather than only the SDP-advertised one, to survive NAT.
- **FR-8.6** — Audio device capture and playback run at 8 kHz mono; the audio backend's
  internal resampler handles conversion from the hardware rate. Device buffer size SHOULD
  match `ptime_ms`.
- **FR-8.11** — Acoustic echo MUST be cancelled, or a speakerphone is unusable: the far end
  hears itself, and at enough gain the loop howls.

  The client MUST open a **duplex** device when echo cancellation is enabled, so each callback
  carries the microphone frame and the speaker frame that accompanies it. That alignment is
  what removes the need to estimate delay or compensate clock drift between two independent
  devices, and it is the reason a straightforward time-domain filter suffices here.

  Cancellation MUST comprise three stages: an adaptive filter that learns and subtracts the
  echo path; a double-talk detector that freezes adaptation while the near-end talker speaks,
  since adapting to the user's own voice makes the filter diverge; and residual suppression
  for what the linear filter cannot remove.

  **The near-end talker MUST never be suppressed.** An echo canceller that quietens the user's
  own voice sounds half-duplex, which is worse to talk through than the echo it removed.

  Duplex may be unavailable — the chosen microphone and speaker may not pair on the platform.
  The client MUST then say so plainly and continue with separate devices and no cancellation,
  rather than failing to start: a phone that works without a usable speakerphone beats a phone
  that does not run.

  Two notes for implementers. The double-talk detector cannot rely on the echo being quieter
  than the reference: a speakerphone at volume produces echo only a decibel or two down, and a
  detector assuming otherwise fires constantly and blocks the very adaptation needed to
  converge. Once converged, compare the residual against its own established floor instead.
  And the filter MUST tolerate a loud reference without diverging, which is what normalising
  the adaptation step by reference power achieves.
- **FR-8.7** — Mic audio is routed only to the active channel. Held channels transmit nothing
  (or comfort noise) and their inbound audio is dropped rather than mixed.
- **FR-8.8** — Media loss MUST be detected: if no inbound RTP arrives for 10 seconds on a
  connected channel, warn the operator (do not drop the call automatically).
- **FR-8.9** — Per-call media statistics (packets sent/received, lost, jitter estimate) MUST
  be available via a `stats [channel]` command and logged at call teardown.
- **FR-8.10** — Local ringback tone (440/480 Hz, 2s on / 4s off) MUST be generated on `180`
  when `audio.ringback_enabled` is true and no early media is present.

---

## 9. CLI / UX

- **FR-9.1** — The client is a long-running interactive REPL. It MUST support line editing,
  history, and tab completion of command names and channel numbers.
- **FR-9.2** — A status line MUST show both channels at a glance, refreshed on every state
  change:

```
  REG ok (refresh 2m14s)  │  [1] HELD  1001 ← +15551234567  02:31  │  [2]* CONNECTED → 2002  00:18
```

  `*` marks the active channel. `←`/`→` mark inbound/outbound direction.

- **FR-9.3** — Command set (minimum):

| Command | Description |
|---|---|
| `dial <target> [ch]` | Place a call. |
| `answer [ch]` | Answer a ringing call. |
| `reject [ch] [code]` | Decline a ringing call. |
| `hangup [ch\|all]` | End a call. |
| `hold [ch]` / `unhold [ch]` | Hold / retrieve. |
| `swap` | Hold the active channel, retrieve the other. |
| `xfer [from] [to]` | Attended transfer (§7.1). |
| `bxfer <target> [ch]` | Blind transfer (§7.2). |
| `cancelxfer` | Abandon a consultation and return to the transferee. |
| `dtmf <digits> [ch]` | Send DTMF. |
| `status` | Registration + both channels in detail. |
| `stats [ch]` | RTP statistics. |
| `register` / `unregister` | Force re-registration / de-register. |
| `devices` | List audio input/output devices. |
| `mute` / `unmute` | Mute the microphone on the active channel. |
| `history [count]` | Show recent calls. |
| `redial [entry] [ch]` | Call the last number dialled, or history entry N. |
| `debug [on\|off]` | Echo SIP packets to the terminal; no argument toggles. |
| `config` | Show effective config, password redacted. |
| `help [cmd]` | Usage. |
| `quit` | De-register, hang up all calls, exit. |

- **FR-9.4** — Asynchronous events (incoming call, remote hangup, transfer `NOTIFY`) MUST be
  printed above the prompt without corrupting the line the operator is typing.
- **FR-9.5** — An incoming call MUST be announced with a visible, distinguishable event line
  including the caller's URI and display name and the channel it landed on.
- **FR-9.6** — Every command MUST validate its arguments and its channel's state, and refuse
  with a specific reason (`channel 2 is IDLE, nothing to hold`) rather than failing silently.
- **FR-9.11** — Finished calls MUST be recorded, so the operator can see who rang and redial
  without retyping a URI.

  The disposition MUST distinguish **answered**, **missed** (rang here, never answered),
  **rejected** (declined here), **cancelled** (we gave up before the far end answered) and
  **failed** (could not be set up, keeping the SIP code). Missed and rejected look identical
  in the state machine and mean opposite things to the person reading the log.

  Records MUST survive a restart, MUST be capped so the file cannot grow without bound, and
  the file MUST be owner-only since it records who was called. A corrupt record MUST be
  skipped rather than stopping startup. `history.enabled: false` disables the feature.

  Note for implementers: a call can be torn down by three paths — `reset`, the dialog watcher,
  and the failed-dial path — and all three must record, or entries silently go missing
  depending on which won the race.
- **FR-9.10** — `debug [on|off]` MUST echo SIP messages to the terminal, printed above the
  prompt like any other output and with credentials redacted as in the trace file. It MUST be
  independent of `logging.sip_trace`: turning terminal echo off never stops the file record,
  and the command MUST work even when file tracing is disabled. It MUST NOT be implemented by
  toggling the SIP stack's global debug flag at runtime, which the transport goroutines read
  without synchronisation.
- **FR-9.7** — Ctrl-C MUST perform the same graceful shutdown as `quit`; a second Ctrl-C
  forces immediate exit.
- **FR-9.8** — The REPL MUST also accept commands piped on stdin, executing them in order and
  exiting at EOF, so transfer scenarios can be scripted for regression testing. A `sleep <ms>`
  and a `wait <channel> <state> [timeout]` command MUST exist for scripted flows.
- **FR-9.9** — Exit codes: `0` clean, `1` config/startup error, `2` registration failure,
  `3` scripted-command failure.

---

## 10. Architecture

### 10.1 Package layout

```
sipclient_cli/
  main.go                 // flag parsing, wiring, signal handling
  requirements.md
  config.json.example
  internal/
    config/               // schema, strict load, validation, defaults
    sip/                  // UA: registration, transactions, dialogs, REFER/NOTIFY
    sdp/                  // offer/answer construction and negotiation
    media/                // RTP session, jitter buffer, G.711 codec, DTMF, tones
    audio/                // device enumeration, capture/playback, routing, mixing
    channel/              // channel state machine, the 2-channel manager
    transfer/             // attended/blind transfer orchestration
    cli/                  // REPL, commands, status line, event printing
    applog/               // structured app log + verbatim SIP trace
    testpbx/              // fake registrar/UAS used by the tests (§13)
```

Flags: `-config <path>`, `-no-audio` (signalling only, for headless runs), `-wait-register
<duration>`, `-version`.

### 10.2 Dependencies

| Purpose | Module | Version | Notes |
|---|---|---|---|
| SIP stack | `github.com/emiago/sipgo` | v1.6.0 | Transactions, dialogs, digest auth, UDP/TCP. |
| Digest auth | `github.com/icholy/digest` | v1.1.0 | Transitive via sipgo; also used by `testpbx` to validate credentials server-side. |
| SDP | `github.com/pion/sdp/v3` | v3.0.19 | Parse/build SDP. |
| RTP | `github.com/pion/rtp` | v1.10.5 | Packet marshalling, `telephone-event` payloader. |
| Audio device | `github.com/gen2brain/malgo` | v0.11.26 | miniaudio binding; CoreAudio on macOS, no external system library. CGO required. |
| G.711 | `github.com/zaf/g711` | v1.4.0 | µ-law/A-law encode/decode. |
| Line editing | `github.com/chzyer/readline` | v1.5.1 | History, completion. |

- **NFR-1** — CGO is required (audio device access). The build MUST be documented as
  `CGO_ENABLED=1`, and the audio layer MUST sit behind an interface so a null-audio
  implementation can be substituted for headless testing.
- **NFR-2** — No dependency on an external softphone, PJSIP, or a system-wide SIP library.

### 10.3 Concurrency

- **NFR-3** — Each channel's state MUST be race-free under `-race`, with the CLI, the SIP
  stack, and the media threads unable to interleave on it.

  *As built:* a per-channel mutex, taken by every exported method on the channel, rather
  than the single-owner goroutine with a command queue that earlier drafts of this document
  specified. The reason is deadlock risk: several channel operations block on SIP
  transactions (a re-INVITE waits for its response), and routing those through a serialising
  goroutine that also serves SIP callbacks invites a cycle. Long-running SIP work therefore
  runs outside the lock and re-acquires it to publish the result. The `-race` guarantee is
  unchanged and is verified with the client binary itself instrumented — see §13 A18.
- **NFR-4** — Media send/receive loops MUST NOT block on CLI or SIP work. A stalled console
  MUST NOT cause audio gaps.
- **NFR-5** — All goroutines MUST be tied to a `context.Context` and exit on shutdown; the
  process MUST terminate within 3 seconds of `quit`, and leak no goroutines between calls.

---

## 11. Observability and error handling

- **NFR-6** — Structured application log (level, timestamp, channel, event) to `logging.file`.
- **NFR-7** — Verbatim SIP trace to `logging.sip_trace_file`, each message stamped with
  direction, transport, peer address, and time — enough to diagnose PBX interop without a
  packet capture. Passwords and `Authorization` response digests MUST be redacted.
- **NFR-8** — Every SIP failure response shown to the operator MUST include the numeric code
  and reason phrase.
- **NFR-9** — Malformed inbound SIP or RTP MUST be logged and dropped, never panic. A recovered
  panic in any goroutine MUST be logged with a stack trace and MUST NOT kill in-progress calls
  on the other channel.

---

## 12. Deferred / future

- D1 — TLS signalling and SRTP (SDES) media.
- D2 — Opus and G.722 wideband codecs.
- ~~AEC~~ — done, see FR-8.11.
- D3 — `transfer.mode = "bridge"`: local B2BUA media bridging for PBXs that reject `REFER`.
- D4 — More than two channels (`channels.count`).
- D5 — Call recording to WAV per leg.
- D6 — Presence/BLF subscriptions and MWI.
- D8 — RTCP. `media.rtcp_enabled` currently only reserves the `port+1` socket to keep the
  convention; no SR/RR is sent or parsed (FR-8.1's RTCP mention is not yet met).
- D7 — Multiple registered accounts in one process.

---

## 13. Acceptance criteria

Most scenarios are verified automatically against `internal/testpbx`, a fake registrar and
UAS that validates digest credentials, answers calls, honours re-INVITEs, and reports REFER
outcomes by NOTIFY. Scenarios are driven through the client's scripted stdin mode (FR-9.8),
so the automated runs exercise the real binary end to end rather than the packages in
isolation. Run them with `go test ./...`; set `SIPCLIENT_TEST_DIR=<dir>` to keep each
scenario's app log and SIP trace for inspection.

Anything marked **manual** needs a real PBX or a human ear and is not automated.

| # | Scenario | Expected | Verified by |
|---|---|---|---|
| A1 | Start with a valid `config.json` | Registers; status shows `REG ok`; re-registers at half the granted expiry. | `TestRegistration` |
| A2 | Start with a bad password | Clear `403`/`401` failure message; exit code `2`; no crash, no retry storm. | `TestWrongPassword` |
| A3 | Start with a malformed config | Names the offending JSON path; exit code `1`. | `internal/config` tests |
| A4 | `dial <ext>` and answer at the far end | Channel 1 `CONNECTED`; two-way audio audible both directions. | `TestOutboundCallAndMedia` (RTP), `TestRealAudioDevice` (device path); audible check **manual** |
| A5 | Inbound call | Announced with caller ID, lands on an idle channel, `answer` gives two-way audio. | `TestInboundCall`, `TestInboundCallRejected` |
| A6 | Two simultaneous calls | Both channels independent; separate RTP ports; audio follows the active channel only. | `TestSwap`, `TestBothChannelsBusy` |
| A7 | `hold` / `unhold` | Correct SDP direction; audio resumes on retrieve. | `TestHoldSendsCorrectDirection`; music-on-hold **manual** |
| A8 | `swap` | Audio moves to the other party, previous party goes on hold. | `TestSwap` |
| A9 | **Warm transfer** (§7.1 flow) | `202 Accepted`, final `NOTIFY` sipfrag `200 OK`, parties talking directly, both channels `IDLE`. Replaces must name the consult dialog with the correct tag orientation. | `TestWarmTransfer`, `TestReferToWithReplaces` |
| A10 | Warm transfer where the target rejects | Failure reported with the code; both legs still up; recovery works. | `TestWarmTransferTargetRejects` |
| A11 | `cancelxfer` mid-consult | Consult leg dropped, transferee retrieved and audible; no stranded hold. | `TestWarmTransferTargetRejects` |
| A12 | Blind transfer | Transferee reaches the target; no `Replaces`; local leg torn down after success. | `TestBlindTransfer` |
| A13 | `dtmf 1234#` into an IVR | All digits registered exactly once, no duplicates or drops. | `TestDTMFDigitsArriveOnce` (counted from RTP telephone-event packets) |
| A14 | Far-end hangup | Local channel returns to `IDLE`, RTP ports released, channel reusable. | `TestFarEndHangup` |
| A15 | 30-minute idle registration | Still registered, no leaks, log free of errors. | **manual** |
| A16 | 10-minute active call | No audio drift or growing latency; plausible loss/jitter. | **manual** |
| A17 | Piped script (FR-9.8) driving A9 | Completes unattended and exits `0`. | every scenario test above |
| A18 | `go build` + `go vet` + `go test -race ./...` | Clean, with the client binary itself race-instrumented. | verified; set `SIPCLIENT_TEST_RACE=1` to instrument the client |

Additional failure paths covered beyond the original list:

| Requirement | Scenario | Verified by |
|---|---|---|
| FR-5.6 | PBX refuses `REFER` (`405`) | `TestWarmTransferRejectedByPBX` |
| FR-5.5 | No `NOTIFY`, dialog gone: success inferred by probe, consult channel released, transferee left up | `TestSilentTransferIsInferred` |
| FR-5.5 | No `NOTIFY`, dialog alive: reported as not taken effect, both legs kept | `TestSilentTransferThatDidNotHappenStaysUnknown` |
| FR-5.5 | No `NOTIFY` and the probe gets no answer: reported unknown, nothing torn down | `TestWarmTransferNotifyTimeout` |
| FR-9.10 / NFR-7 | In-dialog `NOTIFY` routed by our advertised Contact reaches us on UDP and TCP | `TestNotifyDeliveredViaContactUDP`, `TestNotifyDeliveredViaContactTCP` |
| FR-4.4 | Third call while both channels busy | `TestBothChannelsBusy` |
| FR-9.6 | Commands refused with a specific reason | `TestInvalidCommands` |
| FR-8.4 | Jitter buffer reordering, concealment, late drops | `internal/media` tests |
| FR-8.7 | Held/inactive channels detached from audio | `internal/audio` tests |
| FR-9.2 | Status-line format | `TestSnapshotLabel` |
| §3.3 `transport` | Registration, call, hold and warm transfer over SIP/TCP | `TestTCPTransport`, `TestTCPWarmTransfer` |
| FR-2.8 | An inbound call arriving over the transport we did **not** register on is accepted | `TestInboundCallOverOtherTransport` |
| FR-4.11 | A call outlives its session interval because the client refreshes | `TestSessionTimerKeepsLongCallAlive` |
| FR-4.11 | The peer's refresh is answered, and we do not refresh when it said it would | `TestSessionTimerAnswersPeerRefresh` |
| FR-4.11 | `422 Session Interval Too Small` is retried, not fatal | `TestSessionIntervalTooSmallRetries` |
| FR-4.11 | Disabled by config, the client offers and sends nothing | `TestSessionTimerDisabled` |
| FR-4.11 | Header parsing, refresher defaulting and 422 handling | `internal/sipua` session timer tests |
| FR-9.11 | An answered call is logged with talk time, codec and a dialable target | `TestHistoryRecordsAnsweredCall` |
| FR-9.11 | Rejected is distinguished from missed | `TestHistoryDistinguishesMissedFromRejected` |
| FR-9.11 | `redial` calls the last number with nothing typed | `TestRedialCallsTheLastNumber` |
| FR-9.11 | History outlives the process | `TestHistorySurvivesRestart` |
| FR-9.11 | Disabled writes nothing and says so | `TestHistoryDisabled` |
| FR-9.11 | Cap, ordering, atomic write, 0600 mode, corrupt-line tolerance | `internal/history` tests |
| FR-8.11 | Echo is cancelled: ≥20 dB reduction against a synthetic echo path | `TestCancelsEcho` (measures 50+ dB) |
| FR-8.11 | The near-end talker is never suppressed, alone or during double-talk | `TestNearEndPassesThrough`, `TestDoubleTalkPreservesNearEnd` |
| FR-8.11 | The filter survives silence and a clipping-loud reference | `TestSilenceIsStable`, `TestLoudReferenceDoesNotDiverge` |
| FR-8.11 | A duplex device really opens, or the fallback is announced | `TestEchoCancellationOnRealDevice` |
| FR-8.11 | Disabled changes nothing | `TestEchoCancellationDisabled` |
| FR-2.8 | Inbound call over TCP, by connection reuse and by a fresh connection to our Contact | `TestInboundCallOverTCPConnectionReuse`, `TestInboundCallOverTCPViaContact` |
| §3.3 `advertise_address` | SDP carries the media address while SIP keeps the signalling one | `TestMediaAdvertiseAddress`, `TestMediaAddressDefaultsToSignalling` |
| FR-2.11 | Inbound calls, transfers and media all work with an OS-assigned SIP port | every scenario test (the harness leaves `local_sip_port` at `0`) |
| FR-3.6 | Ports held by another process inside a pinned range are skipped | `TestRTPPortCollisionIsSurvivable` |
| FR-3.6 | A fully held range fails naming the cause and the setting | `TestRTPRangeFullyHeldFailsClearly` |
| §3.3 `rtp_port_*` | Ports are OS-assigned with nothing configured | `TestRTPPortsDefaultToEphemeral`, `TestShippedConfigUsesEphemeralPorts` |
| FR-9.10 | `debug on/off` shows and stops showing SIP packets, credentials redacted | `TestDebugCommandShowsSIPPackets` |
| FR-9.10 | State reported by `status`; bare `debug` toggles | `TestDebugDefaultsOffAndReportsState` |
| FR-9.10 | Works with `logging.sip_trace` disabled | `TestDebugWithFileTracingDisabled` |

## 14. Milestones

| M | Deliverable |
|---|---|
| M1 | Config load/validate, logging, SIP trace, `REGISTER` with digest auth + refresh. Acceptance: A1–A3, A15. |
| M2 | Single-channel outbound + inbound call with SDP negotiation and G.711 RTP, no audio device (tone/null sink). Acceptance: signalling for A4–A5. |
| M3 | Audio device capture/playback, jitter buffer, PLC, ringback, DTMF. Acceptance: A4, A5, A13, A16. |
| M4 | Two-channel manager, hold/unhold/swap, audio routing, REPL and status line. Acceptance: A6–A8, A14. |
| M5 | `REFER`/`NOTIFY`: warm transfer, blind transfer, consult cancel, failure recovery. Acceptance: A9–A12. |
| M6 | Scripted stdin mode, `stats`, hardening, race/leak cleanup, docs and `config.json.example`. Acceptance: A17, A18. |

---

## 15. Open questions

- ~~Q1 — Which PBX is the primary interop target?~~ **Answered:** Kamailio 6.0.7 over
  SIP/TCP, observed from its `Server` header. Kamailio is a SIP **proxy**, not a B2BUA, which
  matters for §7: it relays `REFER` to the transferee's endpoint rather than executing the
  transfer itself. So an attended transfer depends on the *transferee's device* supporting
  `REFER` and the *target's device* honouring `Replaces` — the proxy only routes. This makes
  FR-5.3's choice of the dialog's remote target for the `Refer-To` URI more important, not
  less: it addresses the endpoint that actually holds the dialog being replaced.

  Two follow-ups for the live environment: Kamailio must be configured to loose-route
  in-dialog `REFER`, and it currently accepts our `REGISTER` **without a digest challenge**
  (a `200 OK` with no `401`), so the digest path is exercised only by the test suite.
- ~~Q2 — Is NAT traversal needed?~~ **Answered, and it is not STUN that is needed.** The
  PBX runs in containers on a host bridge. The host's own address on that bridge is
  `172.28.0.0/24` — the *network* address — which is a valid packet source but refuses
  incoming connections, and which the containers cannot reach either. Advertising it made
  outbound calls and RTP work (peers latch onto the source) while inbound `INVITE`s and refer
  `NOTIFY`s silently vanished. The fix is `network.public_address` set to an address the PBX
  can reach; STUN is irrelevant here. This is what FR-2.10's startup check now catches.
- Q3 — Should the transferee hear PBX music-on-hold, or is silence acceptable? MoH depends on
  the PBX being configured for it and affects how hold is verified in A7.
