package main

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"sipclient/internal/config"
)

// holdPorts binds a block of UDP ports for the duration of the test, standing
// in for another process on the host (a container runtime forwarding a PBX's
// own RTP range is the usual culprit).
func holdPorts(t *testing.T, start, count int) int {
	t.Helper()

	held := 0
	for p := start; p < start+count; p++ {
		c, err := net.ListenUDP("udp", &net.UDPAddr{Port: p})
		if err != nil {
			continue // already taken by something else; that serves the test too
		}
		t.Cleanup(func() { _ = c.Close() })
		held++
	}
	if held == 0 {
		t.Skip("could not hold any ports to simulate the collision")
	}
	return held
}

// TestRTPPortCollisionIsSurvivable covers the reported failure: the first
// ports of the configured RTP range are held by another process, so a call
// must use later ports in the range instead of failing.
func TestRTPPortCollisionIsSurvivable(t *testing.T) {
	const rtpStart = 24800
	holdPorts(t, rtpStart, 12) // covers pairs 24800, 24802, ... 24810

	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Media.RTPPortStart = rtpStart
		c.Media.RTPPortEnd = rtpStart + 40
	})

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"sleep 400",
		"stats 1",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	requireNotContains(t, out, "bind RTP port", "address already in use")
	requireContains(t, out, "connected, codec PCMU")

	// The call must have landed above the held block, and media must flow.
	if n := pbx.RTPPacketsReceived(); n < 5 {
		t.Errorf("PBX received %d RTP packets, want >= 5\n%s", n, out)
	}
	for p := rtpStart; p < rtpStart+12; p++ {
		if strings.Contains(out, fmt.Sprintf("local_port=%d", p)) {
			t.Errorf("call used held port %d\n%s", p, out)
		}
	}
}

// TestRTPRangeFullyHeldFailsClearly: when the whole range is unusable the
// error must name the cause and the setting to change, not just say "bind".
func TestRTPRangeFullyHeldFailsClearly(t *testing.T) {
	const rtpStart = 24900
	holdPorts(t, rtpStart, 8)

	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Media.RTPPortStart = rtpStart
		c.Media.RTPPortEnd = rtpStart + 7 // every pair is held
	})

	out, code := runClient(t, cfgPath,
		"dial 2001 1\nsleep 500\nstatus\nquit\n", 60*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3\n%s", code, out)
	}
	requireContains(t, out, "held by another process", "rtp_port_start")
}
