# sipclient

A terminal SIP client with **two independent call channels**, built for exercising PBX call
control — in particular **warm (attended) transfer**.

Registration details come from `config.json`. Two channels are the minimum needed to hold a
caller, consult the transfer target privately, and then join the two together and drop out.

See [requirements.md](requirements.md) for the full specification.

## Build

```sh
make build
```

CGO is required for microphone and speaker access, and the Makefile sets it for you. The
equivalent by hand:

```sh
CGO_ENABLED=1 go build -o sipclient .
```

Run `make help` to list every target. No system audio library is needed — the miniaudio backend is bundled and talks to CoreAudio
(macOS), ALSA/PulseAudio (Linux), or WASAPI (Windows) directly.

## Configure

Create `config.json` from the template, then edit four values:

```sh
make config      # copies config.json.example, mode 600; never overwrites
```

| Field | Set to |
|---|---|
| `sip.username` / `sip.auth_username` | your extension |
| `sip.domain` | your SIP domain / digest realm |
| `sip.server.host` | registrar IP or hostname |
| `sip.password` | `export SIP_PASSWORD=...` (the default reads `env:SIP_PASSWORD`), or a literal string |

Every field is documented in requirements.md §3.3 with its default and valid range. The file
is decoded strictly, so a misspelled key fails at startup rather than being silently ignored
— which also means you cannot add comments to it.

**The SIP port needs no configuration either.** `network.local_sip_port` defaults to `0`,
letting the OS pick a free port, so the client never collides with a PBX or another SIP
process on the same machine. The registrar learns the port from each `REGISTER`, so inbound
calls work regardless of it changing between runs. Pin it only if a firewall rule needs a
predictable port.

**RTP ports need no configuration.** By default the OS assigns a free port per call, so the
client cannot collide with anything else on the host. If a firewall needs a predictable
range, set both `media.rtp_port_start` and `media.rtp_port_end`; ports inside that range
that are held by another process are skipped automatically.

## Run

```sh
export SIP_PASSWORD=...
make run                         # build, then start with ./config.json
make run CONFIG=other.json       # a different config
make run-no-audio                # signalling only, no device
```

Or run the binary directly:

```sh
./sipclient                      # uses ./config.json
./sipclient -config other.json
./sipclient -no-audio
```

The prompt shows registration and both channels, refreshed on every state change:

```
REG ok (refresh 2m14s)  │  [1]  HELD ← +15551234567  02:31  │  [2]* CONNECTED → 2002  00:18
sip>
```

`*` marks the channel connected to your microphone and speakers.

## Warm transfer

The flow this client exists for. Alice calls you on channel 1; you transfer her to 2002:

```
sip> hold 1              # Alice hears music on hold
sip> dial 2002 2         # consult the target on channel 2
                         # ...talk to them...
sip> xfer                # connect Alice to 2002 and drop out
transfer succeeded (200 OK); the parties are talking directly
```

`xfer` with no arguments transfers the *other* channel's party to whoever you are consulting
on the active channel. `xfer 1 2` states it explicitly.

If the transfer fails, **both calls stay up** and the client says so — `swap` takes you back
to the held caller. To abandon a consultation before transferring, `cancelxfer` drops the
consult leg and retrieves the original caller.

Some PBXs accept the `REFER` with `202`, carry out the transfer, and never send the `NOTIFY`
that reports the result. When that happens the client probes the dialog instead of giving up:

```
sip> xfer
transfer completed, but the PBX sent no NOTIFY to confirm it
       channel 2's dialog no longer exists on the PBX (481), so the transfer took effect
       verify with the parties before hanging up the remaining channel
```

It never hangs up a live call on an inference — the remaining channel is left for you to
confirm and clear. If the probe shows the call is still established, it says the transfer did
not take effect; if the probe gets no answer at all, it reports the outcome as unknown and
leaves everything alone.

## Commands

| Command | Description |
|---|---|
| `dial <target> [ch]` | Place a call (bare extension or full SIP URI) |
| `answer [ch]` / `reject [ch] [code]` | Answer or decline a ringing call |
| `hangup [ch\|all]` | End a call |
| `hold [ch]` / `unhold [ch]` | Hold / retrieve |
| `swap` | Hold the active channel, retrieve the other |
| `xfer [transferee] [consult]` | Attended (warm) transfer |
| `bxfer <target> [ch]` | Blind transfer |
| `cancelxfer [ch]` | Abandon a consultation, return to the held party |
| `dtmf <digits> [ch]` | Send DTMF |
| `status` / `stats [ch]` | Call state / RTP statistics |
| `register` / `unregister` | Force re-registration / de-register |
| `devices` / `mute` / `unmute` | Audio device list and microphone control |
| `history [count]` | Show recent calls |
| `redial [entry] [ch]` | Call the last number dialled, or history entry N |
| `debug [on\|off]` | Show SIP packets in the terminal (no argument toggles) |
| — | The same actions are available in the browser with `-web` |
| `config` | Effective configuration, password redacted |
| `sleep <ms>` / `wait <ch> <state> [ms]` | For scripted runs |
| `help [cmd]` / `quit` | Usage / exit |

Any command taking `[ch]` defaults to the active channel. Tab completion and history work at
the prompt; Ctrl-C shuts down gracefully (twice forces exit).

## Scripting

With stdin not a terminal, commands are read in order and the client exits at EOF. This is
how the test suite drives the transfer scenarios:

```sh
./sipclient <<'EOF'
dial 2001 1
wait 1 CONNECTED 8000
hold 1
dial 2002 2
wait 2 CONNECTED 8000
xfer 1 2
quit
EOF
```

Exit codes: `0` clean, `1` config/startup error, `2` registration failure, `3` a scripted
command failed.

## Browser control

The client can also be driven from a browser ([MKSIP-1001](docs/MKSIP-1001-web-control.md)):

```sh
make run-web            # or: ./sipclient -web
web control UI: http://127.0.0.1:8080
```

Channel status, a dialpad, transfer, hangup and answer/reject, all live — state is pushed over
a WebSocket, so the page never polls and an incoming call appears without interaction.

**The browser controls the client; it does not carry the audio.** Microphone and speakers stay
on the machine running the client, so a warm transfer driven from the browser still needs you
at that machine to talk to the party you are consulting. The page says so.

It binds **loopback only and has no authentication**, because anyone who can reach it can place
and transfer calls on your extension. A non-loopback `web.listen_address` is refused at startup.

Enable it in config instead of by flag with:

```jsonc
"web": { "enabled": true, "listen_address": "127.0.0.1", "port": 8080 }
```

`port` may be `0` to let the OS choose; the chosen URL is printed at startup and shown by
`status`.

## Speakerphone

Acoustic echo cancellation stops the far end hearing themselves when you use the laptop
speakers. It is **off by default**, since a headset gives it nothing to cancel. Turn it on
for a run:

```
sipclient -echo-cancel
```

or leave it on in the config:

```jsonc
"audio": { "echo_cancel": true, "echo_tail_ms": 128 }
```

`-echo-cancel=false` forces it off again for one run without editing the file.

It needs a **duplex** audio device, so the microphone frame and the speaker frame arrive
together — that alignment is what makes cancellation tractable. If the chosen microphone and
speaker cannot pair, the client says so and carries on without cancellation:

```
audio: echo cancellation unavailable (...); using separate devices, so avoid the speakerphone
```

`stats` shows how it is doing on a live call:

```
echo cancellation: converged, 31 dB reduction
```

Above about 20 dB the echo is inaudible. "adapting" means the filter is still learning the
room; it converges within a few seconds of the far end speaking. When it is switched off, or
when no duplex device could be opened, the same line reads `echo cancellation: off`.

Whether it is on is settled when the audio device opens, because cancellation needs a duplex
device — so it is a startup choice, not something to toggle mid-call.

`echo_tail_ms` is how long an echo path to model. The default suits a laptop; a large,
reverberant room may want more, at the cost of slower convergence.

## Call history

Finished calls are logged, so you can see who rang and call back without retyping a URI:

```
sip> history
   1  05 Sep 16:23  -> 2001                     answered  01:14
   2  05 Sep 16:19  <- Test Caller <15551234>   missed
   3  05 Sep 16:02  -> 2002                     failed              (486 Busy Here)
  redial <n> calls one of these back

sip> redial          # the last number dialled
sip> redial 2        # return the missed call
```

The browser shows the same list, with missed calls picked out and a Call button on each.

A missed call is distinguished from one you declined and from one that failed to connect —
they look identical in the state machine but mean opposite things when you are scanning the
log. Failures keep the SIP code that explains them.

History is kept in `call-history.jsonl` (mode 600 — it records who you called), capped at
`max_entries`, and survives restarts:

```jsonc
"history": { "enabled": true, "file": "call-history.jsonl", "max_entries": 200 }
```

Set `enabled` to `false` to keep no record at all.

## Long calls

The client negotiates RFC 4028 session timers, so a PBX that expects periodic refreshes keeps
the call up. Without them a long call is silently torn down at the PBX's interval — commonly
30 minutes — with nothing on screen to explain it.

It works in both directions: it refreshes when the PBX names it refresher, answers the PBX's
refreshes when the PBX is, and hangs up rather than showing a call the far end has already
abandoned. A `422 Session Interval Too Small` is retried once with the interval the PBX
demands.

```jsonc
"sip": { "session_expires_seconds": 1800, "min_se_seconds": 90 }
```

Set `session_expires_seconds` to `0` to disable it entirely.

## Seeing SIP packets

`debug on` echoes every SIP message to the terminal as it happens, so you do not have to
tail the trace file:

```
sip> debug on
debug on: SIP packets will be shown here
sip> register
=== 22:04:13.996 SEND TCP 172.28.0.0:51186 -> 172.28.0.10:5060 ===
REGISTER sip:mkzaman SIP/2.0
Via: SIP/2.0/TCP 172.28.0.0:51185;branch=z9hG4bK.MXROPM6ewth0Zfbl;rport
...
=== 22:04:13.997 RECV TCP 172.28.0.0:51186 <- 172.28.0.10:5060 ===
SIP/2.0 200 OK
Server: kamailio (6.0.7 (x86_64/linux))
...
sip> debug off
```

Bare `debug` toggles. It is independent of the trace file: `debug off` stops the terminal
echo but the file keeps recording, and `debug on` works even with `logging.sip_trace` set to
false. Digest credentials are redacted in both. `status` shows where packets are going.

## Logs

- `sipclient.log` — structured application log
- `sip-trace.log` — every SIP message verbatim, with direction, transport, peer and
  timestamp, credentials redacted

The trace is usually enough to diagnose PBX interop without a packet capture. Console output
stays at `warn` by default so log lines do not interleave with the prompt.

## Tests

```sh
make test          # unit + scenario tests
make test-unit     # only the fast in-process tests
make test-keep     # keep each scenario's app log and SIP trace under ./testruns/
make race          # every test, with the client binary itself race-instrumented
make check         # fmt-check + vet + build + race, i.e. acceptance criterion A18
```

The underlying commands, if you prefer them directly:

```sh
go test ./...
SIPCLIENT_TEST_DIR=/tmp/runs go test ./...     # keep logs and SIP traces
SIPCLIENT_TEST_RACE=1 go test -race ./...      # race-instrument the client
```

Scenario tests run the real binary against `internal/testpbx`, a fake registrar and UAS that
validates digest credentials, answers calls, honours re-INVITEs, echoes RTP, and reports
REFER outcomes by NOTIFY. requirements.md §13 maps each acceptance criterion to its test.

## Troubleshooting

**`warn: TCP ref went negative` on exit** — a reference-counting warning from inside the
sipgo transport layer during shutdown. It appears after de-registration has already
succeeded and is harmless.

**Registration fails with a socket error rather than a SIP response** — check
`network.local_sip_port` is free, and that `sip.server.transport` matches what the PBX
accepts. `debug on` then `register` shows exactly what goes on the wire.

**Inbound calls never appear, but outbound works** — almost always the address the client
advertises is not one the PBX can open a connection to. Outbound calls and RTP keep working
because the client initiates them and peers latch onto its source address, so nothing looks
broken; but an inbound `INVITE` is sent to `Contact`, and if that address is unreachable the
call silently never arrives. The same cause silently loses the refer `NOTIFY` after a
transfer.

`config` shows both addresses:

```
network:  listening 0.0.0.0:5080 (udp+tcp)  advertised 172.28.0.0:5080  rport=true
```

The client checks this at startup and warns, for example:

```
warn: the address we advertise cannot accept connections, so inbound calls and transfer
      NOTIFYs will not arrive  advertised=172.28.0.0
      reason=it is the network address of 172.28.0.0/24 on interface bridge101
      fix=set network.public_address to an address your PBX can reach
```

Set `network.public_address` to an address your PBX can reach. With a containerised PBX,
ask the container what it can reach rather than guessing:

```sh
docker exec <pbx-container> getent hosts host.docker.internal
docker exec <pbx-container> bash -c 'exec 3<>/dev/tcp/<candidate>/5080' && echo reachable
```

A dynamic SIP port is fine here — the registrar picks up the new port from each `REGISTER`.
The one residue is that killing the client outright (rather than `quit`) leaves the registrar
holding a binding for the dead port until it expires; a clean exit de-registers it.

**One-way or no audio** — `stats` shows packets sent and received per channel. Before
chasing it, check the far end is actually generating audio: a parked or held call often sends
nothing. `sent=200 recv=0` against FreeSWITCH's `&park` is normal; `&echo` proves both
directions. If `recv` really is 0 with audio expected, set `media.advertise_address` to an
address the PBX can send RTP to (it may differ from the signalling address).

## Scope

v1 is UDP/TCP signalling with G.711 (PCMU/PCMA) and RFC 4733 DTMF. TLS/SRTP, Opus, call
recording, and more than two channels are out of scope — see requirements.md §12.
