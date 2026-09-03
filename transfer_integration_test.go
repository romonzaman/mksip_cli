package main

import (
	"strings"
	"testing"
	"time"

	"sipclient/internal/config"
	"sipclient/internal/testpbx"
)

// TestRegistration covers acceptance A1: register, and report it.
func TestRegistration(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	out, code := runClient(t, cfgPath, "status\nquit\n", 30*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "registration: registered")

	rec := pbx.Records()
	if rec.AuthedRegister < 1 {
		t.Errorf("PBX saw %d authenticated REGISTERs, want >= 1", rec.AuthedRegister)
	}
	if rec.Unregisters < 1 {
		t.Errorf("client did not de-register on exit (FR-2.5)")
	}
}

// TestWrongPassword covers acceptance A2: a bad password fails clearly with
// exit code 2 and does not retry forever.
func TestWrongPassword(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.SIP.Password = "wrong-password"
	})

	out, code := runClient(t, cfgPath, "status\nquit\n", 40*time.Second)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (registration failure)\n%s", code, out)
	}
	requireContains(t, out, "registration failed")

	// It must give up rather than hammer the registrar (FR-2.4).
	if rec := pbx.Records(); rec.Registers > 10 {
		t.Errorf("client sent %d REGISTERs for a terminal rejection, want few", rec.Registers)
	}
}

// TestOutboundCallAndMedia covers A4 signalling plus real RTP flow.
func TestOutboundCallAndMedia(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"sleep 600",
		"stats 1",
		"hangup 1",
		"wait 1 IDLE 5000",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 40*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "connected, codec PCMU", "codec=PCMU")

	rec := pbx.Records()
	if len(rec.Invites) < 1 {
		t.Fatalf("PBX saw no INVITE")
	}
	if got := rec.Invites[0].RequestTo; !strings.Contains(got, "2001") {
		t.Errorf("INVITE Request-URI = %q, want it to name 2001", got)
	}
	if rec.Byes < 1 {
		t.Errorf("PBX saw no BYE after hangup")
	}

	// RTP must actually have flowed both ways (the PBX echoes what it gets).
	if n := pbx.RTPPacketsReceived(); n < 10 {
		t.Errorf("PBX received %d RTP packets in ~600ms, want >= 10", n)
	}
	if !strings.Contains(out, "recv=0") {
		// good: we received the echo
	} else {
		t.Errorf("client received no RTP; stats show recv=0\n%s", out)
	}
}

// TestHoldSendsCorrectDirection covers A7: hold must re-INVITE with sendonly.
func TestHoldSendsCorrectDirection(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"hold 1",
		"wait 1 HELD 5000",
		"unhold 1",
		"wait 1 CONNECTED 5000",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 40*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "hold done", "unhold done")

	// Expect: initial sendrecv offer, a sendonly re-INVITE, then sendrecv again.
	var dirs []string
	for _, inv := range pbx.Records().Invites {
		dirs = append(dirs, inv.SDPDir)
	}
	want := []string{"sendrecv", "sendonly", "sendrecv"}
	if len(dirs) < 3 {
		t.Fatalf("INVITE directions = %v, want at least %v", dirs, want)
	}
	for i, w := range want {
		if dirs[i] != w {
			t.Errorf("INVITE %d direction = %q, want %q (all: %v)", i, dirs[i], w, dirs)
		}
	}
	if !pbx.Records().Invites[1].InDialog {
		t.Error("hold was not sent as an in-dialog re-INVITE")
	}
}

// TestWarmTransfer is acceptance A9, the reason this client exists.
//
// It asserts the whole §7.1 flow, and critically that the Replaces header
// identifies the consultation dialog with the correct tag orientation.
func TestWarmTransfer(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.SendInterimNotify = true // prove interim NOTIFYs are not mistaken for the result
	})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"# leg A: the party to be transferred",
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"hold 1",
		"wait 1 HELD 5000",
		"# leg B: consult the transfer target",
		"dial 2002 2",
		"wait 2 CONNECTED 8000",
		"sleep 300",
		"# complete the warm transfer",
		"xfer 1 2",
		"sleep 800",
		"status",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "transfer succeeded", "talking directly")
	requireNotContains(t, out, "outcome unknown", "transfer failed")

	rec := pbx.Records()
	if len(rec.Refers) != 1 {
		t.Fatalf("PBX saw %d REFERs, want exactly 1: %+v", len(rec.Refers), rec.Refers)
	}
	ref := rec.Refers[0]

	// Identify the two call legs the PBX answered.
	var legs []string
	for _, inv := range rec.Invites {
		if !inv.InDialog {
			legs = append(legs, inv.CallID)
		}
	}
	if len(legs) != 2 {
		t.Fatalf("expected 2 call legs, got %d", len(legs))
	}
	legA, legB := legs[0], legs[1]

	// The REFER must travel on leg A's dialog (the transferee).
	if ref.CallID != legA {
		t.Errorf("REFER sent on Call-ID %q, want leg A %q", ref.CallID, legA)
	}
	// ...and its Replaces must point at leg B (the consultation).
	if ref.ReplacesID != legB {
		t.Errorf("Replaces Call-ID = %q, want leg B %q", ref.ReplacesID, legB)
	}
	if !strings.Contains(ref.TargetURI, "2002") {
		t.Errorf("Refer-To target = %q, want it to name the consult party 2002", ref.TargetURI)
	}
	if ref.ReferredBy == "" {
		t.Error("REFER carried no Referred-By header")
	}

	// Tag orientation: to-tag is the replaced UA's own tag, from-tag is ours.
	// Reversing these is the classic cause of a 481 from the PBX (FR-5.3).
	pbxTag, clientTag, ok := pbx.DialogTags(legB)
	if !ok {
		t.Fatalf("PBX has no tags recorded for leg B %q", legB)
	}
	if ref.ToTag != pbxTag {
		t.Errorf("Replaces to-tag = %q, want the replaced party's own tag %q",
			ref.ToTag, pbxTag)
	}
	if ref.FromTag != clientTag {
		t.Errorf("Replaces from-tag = %q, want our tag %q", ref.FromTag, clientTag)
	}

	// After a successful transfer both channels are released (FR-5.2 step 7).
	if !strings.Contains(out, "[1]  IDLE") && !strings.Contains(out, "[1]* IDLE") {
		t.Errorf("channel 1 not returned to IDLE after transfer\n%s", out)
	}
}

// TestWarmTransferRejectedByPBX covers FR-5.6: a PBX that refuses REFER must
// produce an unmistakable message rather than a timeout.
func TestWarmTransferRejectedByPBX(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.ReferStatus = 405 // Method Not Allowed
	})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000", "hold 1", "wait 1 HELD 5000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"xfer 1 2",
		"sleep 300",
		"status",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (scripted command failed)\n%s", code, out)
	}
	requireContains(t, out,
		"PBX does not permit REFER-based transfer",
		"405")
	// Both calls must survive so the operator can recover.
	requireContains(t, out, "CONNECTED", "swap")
}

// TestWarmTransferTargetRejects covers acceptance A10: the target declines, so
// the transfer fails but both calls stay up and recovery works.
func TestWarmTransferTargetRejects(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.ReferOutcome = 486 // Busy Here in the final sipfrag
	})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000", "hold 1", "wait 1 HELD 5000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"xfer 1 2",
		"sleep 800",
		"# recover: drop the consult and take the transferee back",
		"cancelxfer 2",
		"wait 1 CONNECTED 8000",
		"status",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	// The transfer failing is reported as a script failure, which is correct.
	if code != 3 {
		t.Errorf("exit code = %d, want 3\n%s", code, out)
	}
	requireContains(t, out,
		"transfer failed: 486",
		"both calls are still up",
		"consultation abandoned")

	// Recovery must actually restore two-way audio on the transferee.
	if !strings.Contains(out, "[1]* CONNECTED") {
		t.Errorf("transferee not retrieved and active after recovery\n%s", out)
	}
}

// TestWarmTransferNotifyTimeout covers FR-5.5's last resort: no NOTIFY, and
// the liveness probe gets no answer either, so the outcome is genuinely
// unknown and nothing may be torn down.
//
// The two determinate cases are covered by TestSilentTransferIsInferred and
// TestSilentTransferThatDidNotHappenStaysUnknown.
func TestWarmTransferNotifyTimeout(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.SuppressNotify = true
		p.IgnoreOptions = true // the probe cannot get an answer either
	})
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Transfer.NotifyTimeoutSeconds = 2 // keep the test quick
	})

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"hold 1", "wait 1 HELD 5000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"xfer 1 2",
		"status",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 90*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3\n%s", code, out)
	}
	requireContains(t, out,
		"no final NOTIFY",
		"could not be probed",
		"outcome unknown",
		"both calls left up",
	)
	// Nothing may be torn down on a genuinely unknown outcome.
	requireContains(t, out, "[1]  HELD", "[2]* CONNECTED")
}

// TestBlindTransfer covers acceptance A12.
func TestBlindTransfer(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"bxfer 2002 1",
		"sleep 800",
		"status",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "transfer succeeded")

	rec := pbx.Records()
	if len(rec.Refers) != 1 {
		t.Fatalf("PBX saw %d REFERs, want 1", len(rec.Refers))
	}
	// A blind transfer must carry no Replaces (FR-6.1).
	if rec.Refers[0].Replaces != "" {
		t.Errorf("blind transfer carried Replaces=%q, want none", rec.Refers[0].Replaces)
	}
	if !strings.Contains(rec.Refers[0].TargetURI, "2002") {
		t.Errorf("Refer-To = %q, want it to name 2002", rec.Refers[0].TargetURI)
	}
}

// TestBothChannelsBusy covers FR-4.4: a third call is refused with 486.
func TestBothChannelsBusy(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"dial 2003",
		"status",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (the third dial must fail)\n%s", code, out)
	}
	requireContains(t, out, "both channels are busy")
}

// TestDTMF covers acceptance A13 at the signalling level.
func TestDTMF(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"dtmf 1234#",
		"sleep 1500",
		"stats 1",
		"dtmf 9X",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (the invalid digit must be rejected)\n%s", code, out)
	}
	requireContains(t, out, "sent DTMF 1234#", "invalid DTMF digit")
}

// TestInvalidCommands covers FR-9.6: refusals must name the reason.
func TestInvalidCommands(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"hold 2",
		"answer",
		"xfer 1 1",
		"nonsense",
		"hangup 9",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 30*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3\n%s", code, out)
	}
	requireContains(t, out,
		"channel 2 is IDLE, cannot hold",
		"no ringing inbound call",
		"cannot transfer channel 1 to itself",
		`unknown command "nonsense"`,
		"no channel 9",
	)
}
