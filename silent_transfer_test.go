package main

import (
	"strings"
	"testing"
	"time"

	"sipclient/internal/config"
	"sipclient/internal/testpbx"
)

// TestSilentTransferIsInferred reproduces a PBX that accepts the REFER,
// performs the transfer, and reports nothing: no NOTIFY, and the replaced
// dialog silently discarded.
//
// Observed against Kamailio 6.0.7 proxying FreeSWITCH 1.10.12: the REFER was
// answered 202, no NOTIFY ever arrived, and a later BYE on the consultation
// leg returned 481 -- proof the transfer had taken effect. The client must
// reach that conclusion itself rather than reporting "outcome unknown".
func TestSilentTransferIsInferred(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.SuppressNotify = true
		p.DropDialogAfterRefer = true
	})
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Transfer.NotifyTimeoutSeconds = 2 // keep the test quick
	})

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"hold 1", "wait 1 HELD 5000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"xfer 1 2",
		"sleep 500",
		"status",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (the outcome was determined)\n%s", code, out)
	}

	requireContains(t, out,
		"transfer completed, but the PBX sent no NOTIFY",
		"no longer exists on the PBX",
		"verify with the parties",
	)
	requireNotContains(t, out, "outcome unknown")

	// The consult channel must be released, since its dialog is dead: leaving
	// it CONNECTED is what makes a later `hangup` fail with 481.
	if !strings.Contains(out, "[2]* IDLE") && !strings.Contains(out, "[2]  IDLE") {
		t.Errorf("channel 2 not released after its dialog was discarded\n%s", out)
	}
	// The transferee must be left alone: we inferred, we did not confirm.
	if !strings.Contains(out, "[1]  HELD") {
		t.Errorf("channel 1 should remain up and held after an inferred transfer\n%s", out)
	}
}

// TestSilentTransferThatDidNotHappenStaysUnknown is the other half: no NOTIFY,
// but the dialog is still there, so nothing was transferred and both calls
// must be left alone.
func TestSilentTransferThatDidNotHappenStaysUnknown(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.SuppressNotify = true // dialog deliberately left intact
	})
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Transfer.NotifyTimeoutSeconds = 2
	})

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"hold 1", "wait 1 HELD 5000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"xfer 1 2",
		"sleep 300",
		"status",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3\n%s", code, out)
	}
	requireContains(t, out,
		"no final NOTIFY",
		"still established",
		"did not take effect",
		"both calls left up",
	)
	// Both legs must survive so the operator can recover.
	requireContains(t, out, "[1]  HELD", "[2]* CONNECTED")
}
