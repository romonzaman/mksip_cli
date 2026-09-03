package main

import (
	"testing"
	"time"

	"sipclient/internal/config"
)

// TestMediaAdvertiseAddress covers media.advertise_address: SDP must carry it
// while SIP keeps using the signalling address. Needed when RTP and SIP reach
// this host by different paths -- the split FreeSWITCH calls ext-sip-ip versus
// ext-rtp-ip.
func TestMediaAdvertiseAddress(t *testing.T) {
	const mediaAddr = "192.0.2.10" // RFC 5737 documentation range

	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Media.AdvertiseAddress = mediaAddr
	})

	// Signalling still works; only the SDP address changes, so media will not
	// flow to a documentation address -- which this test does not need.
	out, code := runClient(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 8000\nhangup all\nquit\n", 40*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	rec := pbx.Records()
	if len(rec.Invites) == 0 {
		t.Fatal("PBX saw no INVITE")
	}
	if got := rec.Invites[0].SDPAddress; got != mediaAddr {
		t.Errorf("SDP connection address = %q, want %q", got, mediaAddr)
	}
}

// TestMediaAddressDefaultsToSignalling: with nothing set, SDP uses the same
// address as SIP.
func TestMediaAddressDefaultsToSignalling(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil) // local_address 127.0.0.1

	out, code := runClient(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 8000\nhangup all\nquit\n", 40*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	rec := pbx.Records()
	if len(rec.Invites) == 0 {
		t.Fatal("PBX saw no INVITE")
	}
	if got := rec.Invites[0].SDPAddress; got != "127.0.0.1" {
		t.Errorf("SDP connection address = %q, want the signalling address 127.0.0.1", got)
	}
}
