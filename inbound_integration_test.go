package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"sipclient/internal/testpbx"
)

// TestInboundCall covers acceptance A5: an inbound call is announced with the
// caller's identity, lands on an idle channel, and answers with two-way audio.
func TestInboundCall(t *testing.T) {
	// Call the client back as soon as it registers, the way a PBX routes a call
	// to a registered extension. The hook must be installed before the PBX
	// starts serving, or its handler goroutine races the assignment.
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.OnRegistered = func(contact sip.Uri, source string) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := p.Invite(ctx, contact, source); err != nil {
				t.Logf("pbx invite: %v", err)
			}
		}
	})

	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"wait 1 RINGING 10000",
		"answer 1",
		"wait 1 CONNECTED 8000",
		"sleep 500",
		"stats 1",
		"hangup 1",
		"wait 1 IDLE 5000",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	// The caller must be identified in the announcement (FR-9.5).
	requireContains(t, out,
		"incoming call",
		"Test Caller",
		"15551234567",
		"answered, codec PCMU",
	)

	if n := pbx.RTPPacketsReceived(); n < 10 {
		t.Errorf("PBX received %d RTP packets on the inbound call, want >= 10", n)
	}
}

// TestInboundCallRejected covers FR-4.5's decline path.
func TestInboundCallRejected(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.OnRegistered = func(contact sip.Uri, source string) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = p.Invite(ctx, contact, source)
		}
	})

	cfgPath := writeConfig(t, testDir(t), pbx, nil)
	script := strings.Join([]string{
		"wait 1 RINGING 10000",
		"reject 1",
		"wait 1 IDLE 5000",
		"status",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "rejected with 603", "[1]* IDLE")
}

// TestFarEndHangup covers acceptance A14: the channel returns to IDLE and
// releases its resources when the PBX ends the call.
func TestFarEndHangup(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	// Hang up from the PBX once the call is established.
	go func() {
		time.Sleep(3 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pbx.HangupAll(ctx)
	}()

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"# the PBX will BYE us; the channel must clear itself",
		"wait 1 IDLE 12000",
		"status",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "call ended by remote", "[1]* IDLE")

	// A second call must succeed, proving the RTP port was released and the
	// channel is genuinely reusable (FR-3.1, FR-3.6).
	out2, code2 := runClient(t, cfgPath, "dial 2001 1\nwait 1 CONNECTED 8000\nhangup all\nquit\n",
		40*time.Second)
	if code2 != 0 {
		t.Errorf("second run exit code = %d, want 0\n%s", code2, out2)
	}
}

// TestSwap covers acceptance A8: audio moves to the other party and the
// previous one goes on hold.
func TestSwap(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"status",
		"swap",
		"wait 1 CONNECTED 8000",
		"status",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "audio now on channel 1")

	// Dialling channel 2 must have auto-held channel 1 (FR-3.4), and the swap
	// must then reverse the roles.
	if !strings.Contains(out, "[1]  HELD") {
		t.Errorf("channel 1 was not auto-held when channel 2 became active\n%s", out)
	}
	if !strings.Contains(out, "[1]* CONNECTED") || !strings.Contains(out, "[2]  HELD") {
		t.Errorf("after swap, expected channel 1 active and channel 2 held\n%s", out)
	}
}

// TestDTMFDigitsArriveOnce covers acceptance A13 at the RTP level: each digit
// must be registered exactly once, with no duplicates or drops.
func TestDTMFDigitsArriveOnce(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"dtmf 1234#",
		"sleep 2500",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	got := strings.Join(pbx.DTMFDigits(), "")
	if got != "1234#" {
		t.Errorf("PBX received DTMF %q, want %q (no duplicates, no drops)\n%s",
			got, "1234#", out)
	}
}
