package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"sipclient/internal/config"
	"sipclient/internal/control"
	"sipclient/internal/testpbx"
	"sipclient/internal/web"

	"github.com/emiago/sipgo/sip"
)

// sipURI is the URI type the PBX hands back on registration.
type sipURI = sip.Uri

// wsConn is a test browser.
type wsConn struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
	seq  int
}

func dialWS(t *testing.T, base string) *wsConn {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	url := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	return &wsConn{t: t, conn: conn, ctx: ctx}
}

// next reads one frame, failing the test on timeout.
func (w *wsConn) next(timeout time.Duration) map[string]any {
	w.t.Helper()

	ctx, cancel := context.WithTimeout(w.ctx, timeout)
	defer cancel()

	_, data, err := w.conn.Read(ctx)
	if err != nil {
		w.t.Fatalf("read frame: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		w.t.Fatalf("decode frame %q: %v", data, err)
	}
	return msg
}

// send issues a command and returns its id.
func (w *wsConn) send(action string, extra map[string]any) string {
	w.t.Helper()

	w.seq++
	id := fmt.Sprintf("c%d", w.seq)
	cmd := map[string]any{"type": "command", "id": id, "action": action}
	for k, v := range extra {
		cmd[k] = v
	}
	data, _ := json.Marshal(cmd)
	if err := w.conn.Write(w.ctx, websocket.MessageText, data); err != nil {
		w.t.Fatalf("write command: %v", err)
	}
	return id
}

// result waits for the result of one command.
func (w *wsConn) result(id string, timeout time.Duration) map[string]any {
	w.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg := w.next(time.Until(deadline))
		if msg["type"] == "result" && msg["id"] == id {
			return msg
		}
	}
	w.t.Fatalf("no result for command %s within %s", id, timeout)
	return nil
}

// waitChannelState blocks until a state frame shows the wanted channel state.
func (w *wsConn) waitChannelState(id int, want string, timeout time.Duration) map[string]any {
	w.t.Helper()

	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		msg := w.next(time.Until(deadline))
		if msg["type"] != "state" {
			continue
		}
		for _, raw := range channelsOf(msg) {
			ch, _ := raw.(map[string]any)
			if int(ch["id"].(float64)) != id {
				continue
			}
			last, _ = ch["state"].(string)
			if last == want {
				return ch
			}
		}
	}
	w.t.Fatalf("channel %d never reached %s within %s (last saw %q)", id, want, timeout, last)
	return nil
}

func channelsOf(msg map[string]any) []any {
	state, ok := msg["state"].(map[string]any)
	if !ok {
		return nil
	}
	list, _ := state["channels"].([]any)
	return list
}

// TestWebUIServesAndReportsState covers W2: the page loads and the first
// WebSocket frame is the full state.
func TestWebUIServesAndReportsState(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)
	base, _ := startClientWithWeb(t, cfgPath)

	// The page itself.
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	for _, want := range []string{"mksip control", "Dialpad", "/static/app.js"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The page must say the audio is not in the browser (plan §2.1).
	if !strings.Contains(string(body), "Audio stays on the machine") {
		t.Error("page does not say audio stays on the host")
	}

	// One-shot state, for a page load before the socket is up.
	var snap control.State
	resp, err = http.Get(base + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	resp.Body.Close()

	if snap.Registration.State != "registered" {
		t.Errorf("registration = %q, want registered", snap.Registration.State)
	}
	if len(snap.Channels) != 2 {
		t.Errorf("got %d channels, want 2", len(snap.Channels))
	}
	if !snap.AudioOnHost {
		t.Error("audioOnHost should be true: the browser does not carry audio")
	}

	// The socket's first frame is the whole state.
	ws := dialWS(t, base)
	first := ws.next(5 * time.Second)
	if first["type"] != "state" {
		t.Fatalf("first frame is %v, want state", first["type"])
	}
	if len(channelsOf(first)) != 2 {
		t.Errorf("first state frame has %d channels, want 2", len(channelsOf(first)))
	}
}

// TestWebUICallLifecycle covers W4 and W8: dial from the browser, watch the
// state change, hang up, and get the REPL's own error text for a bad command.
func TestWebUICallLifecycle(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)
	base, _ := startClientWithWeb(t, cfgPath)

	ws := dialWS(t, base)
	ws.next(5 * time.Second) // initial state

	id := ws.send("dial", map[string]any{"target": "2001", "channel": 1})
	if res := ws.result(id, 10*time.Second); res["ok"] != true {
		t.Fatalf("dial failed: %v", res["error"])
	}
	ch := ws.waitChannelState(1, "CONNECTED", 15*time.Second)
	if ch["codec"] != "PCMU" {
		t.Errorf("codec = %v, want PCMU", ch["codec"])
	}

	// An invalid action must come back with the message the REPL would print.
	id = ws.send("hold", map[string]any{"channel": 2})
	res := ws.result(id, 10*time.Second)
	if res["ok"] != false {
		t.Fatal("holding an idle channel should fail")
	}
	if got, _ := res["error"].(string); !strings.Contains(got, "channel 2 is IDLE, cannot hold") {
		t.Errorf("error = %q, want the REPL's wording", got)
	}

	id = ws.send("hangup", map[string]any{"channel": 1})
	if res := ws.result(id, 10*time.Second); res["ok"] != true {
		t.Fatalf("hangup failed: %v", res["error"])
	}
	ws.waitChannelState(1, "IDLE", 10*time.Second)

	if pbx.Records().Byes < 1 {
		t.Error("PBX saw no BYE")
	}
}

// TestWebUIWarmTransfer is the acceptance bar from the plan (W7): an entire
// attended transfer driven from the browser, verified on the PBX exactly as
// TestWarmTransfer verifies the terminal one.
func TestWebUIWarmTransfer(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) { p.SendInterimNotify = true })
	cfgPath := writeConfig(t, testDir(t), pbx, nil)
	base, _ := startClientWithWeb(t, cfgPath)

	ws := dialWS(t, base)
	ws.next(5 * time.Second)

	// Leg A: the party to be transferred.
	ws.result(ws.send("dial", map[string]any{"target": "2001", "channel": 1}), 10*time.Second)
	ws.waitChannelState(1, "CONNECTED", 15*time.Second)

	ws.result(ws.send("hold", map[string]any{"channel": 1}), 10*time.Second)
	ws.waitChannelState(1, "HELD", 10*time.Second)

	// Leg B: consult the target.
	ws.result(ws.send("dial", map[string]any{"target": "2002", "channel": 2}), 10*time.Second)
	ws.waitChannelState(2, "CONNECTED", 15*time.Second)

	// Complete the warm transfer.
	res := ws.result(ws.send("xfer", map[string]any{"transferee": 1, "consult": 2}), 30*time.Second)
	if res["ok"] != true {
		t.Fatalf("transfer failed: %v", res["error"])
	}
	if got, _ := res["text"].(string); !strings.Contains(got, "transfer succeeded") {
		t.Errorf("transfer text = %q", got)
	}

	// The PBX must have seen a correct attended transfer.
	rec := pbx.Records()
	if len(rec.Refers) != 1 {
		t.Fatalf("PBX saw %d REFERs, want 1", len(rec.Refers))
	}
	ref := rec.Refers[0]

	var legs []string
	for _, inv := range rec.Invites {
		if !inv.InDialog {
			legs = append(legs, inv.CallID)
		}
	}
	if len(legs) != 2 {
		t.Fatalf("expected 2 call legs, got %d", len(legs))
	}
	if ref.CallID != legs[0] {
		t.Errorf("REFER sent on %q, want leg A %q", ref.CallID, legs[0])
	}
	if ref.ReplacesID != legs[1] {
		t.Errorf("Replaces names %q, want leg B %q", ref.ReplacesID, legs[1])
	}

	pbxTag, clientTag, ok := pbx.DialogTags(legs[1])
	if !ok {
		t.Fatal("no tags recorded for leg B")
	}
	if ref.ToTag != pbxTag || ref.FromTag != clientTag {
		t.Errorf("Replaces tags = to:%q from:%q, want to:%q from:%q",
			ref.ToTag, ref.FromTag, pbxTag, clientTag)
	}
}

// TestWebUIInboundCall covers W5: an inbound call reaches the browser without
// interaction, and can be answered from it.
func TestWebUIInboundCall(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.OnRegistered = func(contact sipURI, source string) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := p.Invite(ctx, contact, source); err != nil {
				t.Logf("pbx invite: %v", err)
			}
		}
	})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)
	base, _ := startClientWithWeb(t, cfgPath)

	ws := dialWS(t, base)

	// Do not discard the first frame here. The call may already be ringing by
	// the time the browser connects, in which case the initial snapshot is the
	// only frame carrying RINGING -- which is exactly the behaviour that makes
	// a browser opened mid-call correct.
	ch := ws.waitChannelState(1, "RINGING", 25*time.Second)
	if ch["ringing"] != true {
		t.Error("inbound call should be flagged ringing so the UI cannot miss it")
	}
	if ch["inbound"] != true {
		t.Error("call should be marked inbound")
	}

	if res := ws.result(ws.send("answer", map[string]any{"channel": 1}), 10*time.Second); res["ok"] != true {
		t.Fatalf("answer failed: %v", res["error"])
	}
	ws.waitChannelState(1, "CONNECTED", 10*time.Second)
}

// TestWebUIRejectsCrossOrigin covers W11.
func TestWebUIRejectsCrossOrigin(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)
	base, _ := startClientWithWeb(t, cfgPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(base, "http") + "/ws"
	_, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		t.Error("a cross-origin upgrade was accepted; a page elsewhere could drive the phone")
	}
}

// TestWebUIDisabledByDefault covers W1: an existing config is unaffected.
func TestWebUIDisabledByDefault(t *testing.T) {
	if config.Default().Web.Enabled {
		t.Error("the web UI must be off unless asked for")
	}

	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	out, code := runClient(t, cfgPath, "status\nquit\n", 30*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "web control UI") {
		t.Error("web UI started without being enabled")
	}
}

// TestWebConfigRejectsNonLoopback covers W10: this endpoint can place calls and
// has no authentication, so it must refuse to bind a routable address.
func TestWebConfigRejectsNonLoopback(t *testing.T) {
	cfg := config.Default()
	cfg.SIP.Username = "1001"
	cfg.SIP.Password = "pw"
	cfg.SIP.Domain = "d"
	cfg.SIP.Server.Host = "h"
	cfg.Web.Enabled = true

	for _, addr := range []string{"0.0.0.0", "192.168.1.10"} {
		cfg.Web.ListenAddress = addr
		err := cfg.Validate()
		if err == nil {
			t.Errorf("listen_address %q was accepted", addr)
			continue
		}
		if !strings.Contains(err.Error(), "web.listen_address") {
			t.Errorf("error for %q does not name the field: %v", addr, err)
		}
	}

	for _, addr := range []string{"127.0.0.1", "::1", "localhost"} {
		cfg.Web.ListenAddress = addr
		if err := cfg.Validate(); err != nil {
			t.Errorf("loopback %q rejected: %v", addr, err)
		}
	}
}

// compile-time guard that the server type is what main wires up.
var _ = web.New
