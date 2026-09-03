package main

import (
	"strings"
	"testing"
	"time"

	"sipclient/internal/config"
	"sipclient/internal/testpbx"
)

// TestTCPTransport covers the TCP half of FR §3.3's transport setting: the
// client must register and place a call over SIP/TCP.
//
// This is the configuration that exposed a real bug: binding outgoing requests
// to the listener's local port works for UDP, where one socket both listens
// and sends, but on TCP it means dialling out from a port the listener already
// holds, which fails with "bind: address already in use".
func TestTCPTransport(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.Network = "tcp"
	})
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Network.LocalSIPPort = 0 // ephemeral, as in the reported failure
	})

	script := strings.Join([]string{
		"status",
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"hold 1",
		"wait 1 HELD 5000",
		"unhold 1",
		"wait 1 CONNECTED 5000",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	requireNotContains(t, out, "address already in use")
	requireContains(t, out,
		"registration: registered",
		"connected, codec PCMU",
		"hold done",
		"unhold done",
	)

	if rec := pbx.Records(); rec.AuthedRegister < 1 {
		t.Errorf("PBX saw %d authenticated REGISTERs over TCP, want >= 1",
			rec.AuthedRegister)
	}
}

// TestTCPWarmTransfer proves the transfer path works over TCP too, since that
// is the transport this deployment uses.
func TestTCPWarmTransfer(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.Network = "tcp"
	})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"hold 1", "wait 1 HELD 5000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"xfer 1 2",
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
		t.Fatalf("PBX saw %d REFERs over TCP, want 1", len(rec.Refers))
	}
	if rec.Refers[0].ReplacesID == "" {
		t.Error("REFER over TCP carried no Replaces")
	}
}
