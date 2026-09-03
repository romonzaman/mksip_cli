package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"sipclient/internal/config"
	"sipclient/internal/testpbx"
)

// TestInboundCallOverOtherTransport: the client is configured to register over
// one transport, but the inbound call arrives over the other.
//
// A proxy may route an incoming call over either transport regardless of how
// we registered, so sip.server.transport must not decide what we can receive.
// Before this was fixed the client listened on the configured transport only,
// and an incoming call over the other one produced no output at all.
func TestInboundCallOverOtherTransport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure string // transport the client registers with
		inbound   string // transport the PBX uses for the INVITE
	}{
		{"register_tcp_call_arrives_udp", "tcp", "udp"},
		{"register_udp_call_arrives_tcp", "udp", "tcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The PBX registers on one transport and calls on the other, so it
			// needs both served. Two instances share nothing but the port.
			pbx := startPBX(t, func(p *testpbx.PBX) {
				p.Network = tc.configure
				p.CallNetwork = tc.inbound
				p.OnRegistered = func(contact sip.Uri, source string) {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					if _, err := p.Invite(ctx, contact, ""); err != nil {
						t.Logf("pbx invite over %s: %v", tc.inbound, err)
					}
				}
			})
			cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
				c.SIP.Server.Transport = tc.configure
			})

			script := strings.Join([]string{
				"wait 1 RINGING 12000",
				"answer 1",
				"wait 1 CONNECTED 8000",
				"hangup all",
				"quit",
			}, "\n") + "\n"

			out, code := runClient(t, cfgPath, script, 60*time.Second)
			if code != 0 {
				t.Errorf("exit code = %d, want 0\n%s", code, out)
			}
			requireContains(t, out, "incoming call", "answered, codec PCMU")
		})
	}
}
