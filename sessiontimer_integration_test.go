package main

import (
	"strings"
	"testing"
	"time"

	"sipclient/internal/config"
	"sipclient/internal/testpbx"
)

// TestSessionTimerKeepsLongCallAlive is the failure this feature exists to
// prevent: the PBX names the client as refresher, and a client that never
// refreshes has its call torn down mid-conversation with nothing to show the
// user why.
//
// The interval is squeezed to 90s (RFC 4028's floor) and the client refreshes
// at half of it, so the test observes real refreshes in ~45s without waiting
// for a production 30-minute timer.
func TestSessionTimerKeepsLongCallAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~60s: exercises a real refresh cycle")
	}

	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.SessionExpires = 90 * time.Second
		p.SessionRefresher = "uac" // the client must refresh
	})
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.SIP.SessionExpiresSeconds = 90
		c.SIP.MinSESeconds = 90
	})

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"# outlive one full session interval: the refresh must arrive at ~45s",
		"sleep 55000",
		"status",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 120*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	// Judge only what happened before we deliberately hung up.
	duringCall := out
	if i := strings.Index(out, "> hangup all"); i > 0 {
		duringCall = out[:i]
	}

	// The call must still be up after outliving the refresh point.
	if !strings.Contains(duringCall, "[1]* CONNECTED") {
		t.Errorf("call did not survive the session interval\n%s", out)
	}
	for _, teardown := range []string{"stopped refreshing", "call ended by remote"} {
		if strings.Contains(duringCall, teardown) {
			t.Errorf("call was torn down mid-interval (%q)\n%s", teardown, out)
		}
	}

	if n := pbx.SessionRefreshes(); n < 1 {
		t.Errorf("PBX saw %d session refreshes, want at least 1 -- "+
			"without them it would drop the call", n)
	}
	if n := pbx.SessionExpiredCalls(); n != 0 {
		t.Errorf("PBX expired %d calls; the client failed to refresh in time", n)
	}
}

// TestSessionTimerAnswersPeerRefresh covers the other direction: the PBX
// refreshes and the client must answer, echoing the agreed timer.
func TestSessionTimerAnswersPeerRefresh(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.SessionExpires = 90 * time.Second
		p.SessionRefresher = "uas" // the PBX refreshes; we just answer
	})
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.SIP.SessionExpiresSeconds = 90
		c.SIP.MinSESeconds = 90
	})

	out, code := runClient(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 8000\nsleep 1500\nstatus\nhangup all\nquit\n",
		60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "[1]* CONNECTED")

	// We must not refresh when the peer said it would.
	if n := pbx.SessionRefreshes(); n != 0 {
		t.Errorf("client sent %d refreshes though the PBX named itself refresher", n)
	}
}

// TestSessionIntervalTooSmallRetries covers the 422 path: a PBX whose minimum
// is longer than ours must not fail the call, it must be retried.
func TestSessionIntervalTooSmallRetries(t *testing.T) {
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.SessionExpires = 1800 * time.Second
		p.SessionRefresher = "uas"
		p.MinSE = 1200 * time.Second // longer than the client's offer below
	})
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.SIP.SessionExpiresSeconds = 120 // below the PBX's minimum
		c.SIP.MinSESeconds = 90
	})

	out, code := runClient(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 12000\nstatus\nhangup all\nquit\n",
		60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 -- a 422 must be retried, not fatal\n%s", code, out)
	}
	requireContains(t, out, "connected, codec PCMU")

	// Two INVITEs: the rejected one and the retry.
	var initial int
	for _, inv := range pbx.Records().Invites {
		if !inv.InDialog {
			initial++
		}
	}
	if initial < 2 {
		t.Errorf("saw %d initial INVITEs, want 2 (the 422 and the retry)", initial)
	}
}

// TestSessionTimerDisabled: with the feature off the client must behave exactly
// as before, offering no interval and never refreshing.
func TestSessionTimerDisabled(t *testing.T) {
	pbx := startPBX(t, nil) // PBX negotiates no session timer
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.SIP.SessionExpiresSeconds = 0
	})

	out, code := runClient(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 8000\nsleep 800\nhangup all\nquit\n",
		40*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "connected, codec PCMU")
	if n := pbx.SessionRefreshes(); n != 0 {
		t.Errorf("client refreshed %d times with session timers disabled", n)
	}
}
