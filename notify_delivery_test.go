package main

import (
	"strings"
	"testing"
	"time"

	"sipclient/internal/testpbx"
)

// TestNotifyDeliveredViaContactUDP checks we receive an in-dialog NOTIFY that
// the peer routes by our advertised Contact rather than back down the
// connection the REFER arrived on.
func TestNotifyDeliveredViaContactUDP(t *testing.T) {
	assertNotifyViaContact(t, "udp")
}

// TestNotifyDeliveredViaContactTCP is the same over TCP, where the listener and
// our outbound connection are different sockets. This is the path a proxy takes
// when it cannot reuse the existing connection, and if our advertised Contact
// port were unreachable the refer NOTIFY would never arrive -- which is exactly
// how a transfer ends up reported as "outcome unknown".
func TestNotifyDeliveredViaContactTCP(t *testing.T) {
	assertNotifyViaContact(t, "tcp")
}

func assertNotifyViaContact(t *testing.T, network string) {
	t.Helper()

	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.Network = network
		p.NotifyViaContact = true
	})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"hold 1", "wait 1 HELD 5000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"xfer 1 2",
		"sleep 500",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("[%s] exit code = %d, want 0\n%s", network, code, out)
	}

	// The whole point: the NOTIFY must arrive, so the outcome is known.
	requireContains(t, out, "transfer succeeded")
	requireNotContains(t, out, "no final NOTIFY", "outcome unknown")

	if n := pbx.Records().Notifies; n < 1 {
		t.Errorf("[%s] PBX could not deliver any NOTIFY via our Contact", network)
	}
}
