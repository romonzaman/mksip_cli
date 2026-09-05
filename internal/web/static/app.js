// Browser control surface for mksip (MKSIP-1001).
//
// State is pushed by the server on every change; this file never polls and
// never merges deltas -- each `state` frame is the whole truth and replaces
// what is rendered.
"use strict";

const $ = (id) => document.getElementById(id);

let socket = null;
let state = null;
let backoff = 500;
let commandSeq = 0;
const pending = new Map();

// ---------------------------------------------------------------- connection

function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  socket = new WebSocket(`${proto}//${location.host}/ws`);

  socket.onopen = () => {
    backoff = 500;
    document.body.classList.remove("disconnected");
    $("offline").hidden = true;
  };

  socket.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch { return; }

    if (msg.type === "state") {
      state = msg.state;
      render();
    } else if (msg.type === "event") {
      log(msg.text, msg.kind, msg.channel);
    } else if (msg.type === "result") {
      const label = pending.get(msg.id) || "";
      pending.delete(msg.id);
      if (msg.ok) {
        if (msg.text) log(msg.text, "info");
      } else {
        log(`${label ? label + ": " : ""}${msg.error}`, "error");
      }
    }
  };

  socket.onclose = () => {
    document.body.classList.add("disconnected");
    $("offline").hidden = false;
    // Reconnect with backoff; the first `state` frame restores everything, so
    // there is nothing to resynchronise by hand.
    setTimeout(connect, backoff);
    backoff = Math.min(backoff * 2, 8000);
  };

  socket.onerror = () => socket.close();
}

function send(action, extra = {}, label = action) {
  if (!socket || socket.readyState !== WebSocket.OPEN) {
    log("not connected", "error");
    return;
  }
  const id = `c${++commandSeq}`;
  pending.set(id, label);
  socket.send(JSON.stringify({ type: "command", id, action, ...extra }));
}

// -------------------------------------------------------------------- render

function fmtDuration(seconds) {
  const s = Math.max(0, seconds | 0);
  return `${String((s / 60) | 0).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
}

function render() {
  if (!state) return;
  renderRegistration();
  renderChannels();
  renderControls();
  renderRecents();
  renderTitle();
}

function renderRegistration() {
  const r = state.registration;
  const registered = r.state === "registered";

  $("reg-dot").className = "dot " + (registered ? "ok" : r.state === "failed" ? "bad" : "");
  $("reg-state").textContent = registered ? "Registered" : r.state;
  $("reg-aor").textContent = r.aor || "";

  const bits = [];
  if (r.server) bits.push(`${r.server} ${r.transport || ""}`.trim());
  if (registered && r.refreshInSeconds) bits.push(`refresh in ${fmtDuration(r.refreshInSeconds)}`);
  if (r.lastError) bits.push(r.lastError);
  if (state.audioDevice) bits.push(`audio: ${state.audioDevice}`);
  $("reg-detail").textContent = bits.join(" · ");
}

function renderChannels() {
  const host = $("channels");
  host.innerHTML = "";

  for (const ch of state.channels) {
    const card = document.createElement("div");
    card.className = "chan"
      + (ch.active ? " active" : "")
      + (ch.ringing ? " ringing" : "");

    const head = document.createElement("div");
    head.className = "chan-head";
    head.innerHTML =
      `<span class="chan-id">Channel ${ch.id}${ch.active ? " ●" : ""}</span>` +
      `<span class="chan-state">${ch.state}</span>`;
    card.appendChild(head);

    const remote = document.createElement("div");
    remote.className = "chan-remote";
    remote.textContent = ch.remote
      ? `${ch.inbound ? "←" : "→"} ${ch.remote}`
      : "—";
    card.appendChild(remote);

    const meta = document.createElement("div");
    meta.className = "chan-meta";
    const m = [];
    if (ch.inCall) m.push(fmtDuration(ch.durationSeconds));
    if (ch.codec) m.push(ch.codec);
    if (ch.localHold) m.push("on hold");
    if (ch.remoteHold) m.push("remote hold");
    meta.textContent = m.join(" · ");
    card.appendChild(meta);

    card.appendChild(channelActions(ch));
    host.appendChild(card);
  }
}

// channelActions builds the buttons for one channel. An inbound call takes the
// card over with Answer and Reject, because that is the one thing the operator
// must not miss.
function channelActions(ch) {
  const row = document.createElement("div");
  row.className = "chan-actions";

  const add = (text, cls, fn, disabled = false) => {
    const b = document.createElement("button");
    b.textContent = text;
    if (cls) b.className = cls;
    b.disabled = disabled;
    b.onclick = fn;
    row.appendChild(b);
  };

  if (ch.ringing) {
    add("Answer", "ok", () => send("answer", { channel: ch.id }, `answer ${ch.id}`));
    add("Reject", "danger", () => send("reject", { channel: ch.id }, `reject ${ch.id}`));
    return row;
  }

  if (ch.state === "IDLE") {
    add("Dial here", "", () => {
      const t = $("target").value.trim();
      if (!t) { $("target").focus(); return; }
      send("dial", { target: t, channel: ch.id }, `dial ${t}`);
    });
    return row;
  }

  if (ch.localHold) {
    add("Retrieve", "primary", () => send("unhold", { channel: ch.id }, `unhold ${ch.id}`));
  } else {
    add("Hold", "", () => send("hold", { channel: ch.id }, `hold ${ch.id}`), !ch.inCall);
  }
  add("Hang up", "danger", () => send("hangup", { channel: ch.id }, `hangup ${ch.id}`));
  return row;
}

// renderControls disables what cannot apply. The server validates everything
// regardless; this only saves the operator a pointless click.
function renderControls() {
  const inCall = state.channels.filter((c) => c.inCall);
  const bothUp = inCall.length === 2;

  $("btn-xfer").disabled = !bothUp;
  $("btn-xfer").title = bothUp
    ? "Connect the two parties and drop out"
    : "Needs an established call on both channels";
  $("btn-swap").disabled = !bothUp;
  $("btn-cancelxfer").disabled = !bothUp;
  $("btn-hangup-all").disabled = inCall.length === 0
    && !state.channels.some((c) => c.state !== "IDLE");

  const mute = $("btn-mute");
  mute.textContent = state.muted ? "Unmute" : "Mute";
  mute.classList.toggle("danger", state.muted);

  $("dtmf-hint").textContent = activeInCall()
    ? "Keys send DTMF on the active call."
    : "Keys type into the box. During a call they send DTMF.";
}

function renderTitle() {
  const ringing = state.channels.find((c) => c.ringing);
  if (ringing) {
    document.title = `☎ ${ringing.remote || "incoming"} — mksip`;
    return;
  }
  const up = state.channels.filter((c) => c.inCall).length;
  document.title = up ? `(${up}) mksip control` : "mksip control";
}

function activeInCall() {
  return state && state.channels.find((c) => c.active && c.inCall);
}

// Recent calls. A missed call is styled apart because it is the one people
// open this list to find.
function renderRecents() {
  const host = $("recents");
  const records = state.recent || [];
  host.innerHTML = "";

  if (records.length === 0) {
    const li = document.createElement("li");
    li.className = "empty";
    li.textContent = "No calls yet.";
    host.appendChild(li);
    return;
  }

  records.forEach((r, i) => {
    const li = document.createElement("li");
    if (r.disposition === "missed") li.classList.add("missed");

    const dir = document.createElement("span");
    dir.className = "dir";
    dir.textContent = r.direction === "in"
      ? (r.disposition === "missed" ? "\u2717" : "\u2190")
      : "\u2192";
    dir.title = `${r.direction === "in" ? "inbound" : "outbound"}, ${r.disposition}`;
    li.appendChild(dir);

    const who = document.createElement("span");
    who.className = "who";
    who.textContent = r.remote || r.remoteUri || "unknown";
    li.appendChild(who);

    const when = document.createElement("span");
    when.className = "when";
    when.textContent = shortWhen(r.startedAt)
      + (r.talkSeconds ? ` · ${fmtDuration(r.talkSeconds)}` : "");
    li.appendChild(when);

    const call = document.createElement("button");
    call.textContent = "Call";
    // Redial by entry number: the server owns the history, so the browser
    // never has to send back a number it might have stale.
    call.onclick = () => send("redial", { entry: i + 1 },
      `redial ${r.remote || r.remoteUri}`);
    li.appendChild(call);

    host.appendChild(li);
  });
}

// shortWhen renders a timestamp as a time today, or a date before that.
function shortWhen(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  return sameDay
    ? d.toTimeString().slice(0, 5)
    : d.toLocaleDateString(undefined, { day: "numeric", month: "short" });
}

// ----------------------------------------------------------------------- log

function log(text, kind = "info", channel = 0) {
  const li = document.createElement("li");
  const time = new Date().toTimeString().slice(0, 8);
  li.innerHTML = `<span class="t">${time}</span>`;

  const body = document.createElement("span");
  body.className = kind;
  body.textContent = (channel ? `[${channel}] ` : "") + text;
  li.appendChild(body);

  const list = $("log");
  list.prepend(li);
  while (list.children.length > 200) list.lastChild.remove();
}

// -------------------------------------------------------------------- wiring

function dialpadKey(key) {
  // During a call the pad is a DTMF keypad; otherwise it composes a number.
  const ch = activeInCall();
  if (ch) {
    send("dtmf", { channel: ch.id, digits: key }, `dtmf ${key}`);
  } else {
    $("target").value += key;
    $("target").focus();
  }
}

function wire() {
  const keys = $("keys");
  for (const k of ["1", "2", "3", "4", "5", "6", "7", "8", "9", "*", "0", "#"]) {
    const b = document.createElement("button");
    b.textContent = k;
    b.onclick = () => dialpadKey(k);
    keys.appendChild(b);
  }

  $("dial").onclick = () => {
    const t = $("target").value.trim();
    if (!t) return;
    send("dial", { target: t }, `dial ${t}`);
  };
  $("target").onkeydown = (e) => { if (e.key === "Enter") $("dial").click(); };

  $("bxfer").onclick = () => {
    const t = $("bxfer-target").value.trim();
    if (!t) return;
    send("bxfer", { target: t }, `blind transfer to ${t}`);
  };
  $("bxfer-target").onkeydown = (e) => { if (e.key === "Enter") $("bxfer").click(); };

  $("btn-swap").onclick = () => send("swap");
  $("btn-xfer").onclick = () => send("xfer", {}, "transfer");
  $("btn-cancelxfer").onclick = () => send("cancelxfer", {}, "cancel consult");
  $("btn-mute").onclick = () => send(state && state.muted ? "unmute" : "mute");
  $("btn-hangup-all").onclick = () => send("hangup", { all: true }, "hangup all");
}

wire();
connect();
