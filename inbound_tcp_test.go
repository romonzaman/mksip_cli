package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"sipclient/internal/testpbx"
)

// TestInboundCallOverTCPConnectionReuse: the PBX sends the INVITE back down
// the TCP connection the client registered on. This is what a proxy does when
// it can reuse the connection.
func TestInboundCallOverTCPConnectionReuse(t *testing.T) {
	assertInboundTCP(t, true)
}

// TestInboundCallOverTCPViaContact: the PBX routes the INVITE by the Contact
// the client registered, opening a fresh TCP connection to the port we
// advertise. This is the path taken when the proxy cannot reuse the existing
// connection -- and if our advertised port is unreachable, an incoming call
// simply never appears.
func TestInboundCallOverTCPViaContact(t *testing.T) {
	assertInboundTCP(t, false)
}

func assertInboundTCP(t *testing.T, reuseConnection bool) {
	t.Helper()

	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.Network = "tcp"
		p.OnRegistered = func(contact sip.Uri, source string) {
			if !reuseConnection {
				source = "" // force routing by the Contact URI
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := p.Invite(ctx, contact, source); err != nil {
				t.Logf("pbx invite (reuse=%v): %v", reuseConnection, err)
			}
		}
	})
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"wait 1 RINGING 12000",
		"answer 1",
		"wait 1 CONNECTED 8000",
		"sleep 400",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("reuse=%v: exit code = %d, want 0\n%s", reuseConnection, code, out)
	}
	requireContains(t, out, "incoming call", "answered, codec PCMU")
}
