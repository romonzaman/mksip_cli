package main

import (
	"strings"
	"testing"
	"time"

	"sipclient/internal/config"
)

// TestDebugCommandShowsSIPPackets covers the `debug` command: SIP packets must
// appear on the terminal while it is on, and stop when it is off.
func TestDebugCommandShowsSIPPackets(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"status",
		"debug on",
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"hangup all",
		"wait 1 IDLE 5000",
		"debug off",
		"# nothing below should produce packet output",
		"status",
		"quit",
	}, "\n") + "\n"

	out, code := runClient(t, cfgPath, script, 60*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	requireContains(t, out,
		"debug on: SIP packets will be shown here",
		"debug off: SIP packets no longer shown here",
		// The INVITE and its answer must be visible verbatim.
		"INVITE sip:2001@testpbx SIP/2.0",
		"SIP/2.0 200 OK",
		"=== ", // the trace header line
		"SEND UDP",
		"RECV UDP",
	)

	// Credentials must stay redacted even on the terminal (NFR-7).
	requireNotContains(t, out, `response="`)

	// Packets must appear only between `debug on` and `debug off`.
	afterOff := out[strings.LastIndex(out, "debug off:"):]
	if strings.Contains(afterOff, "SEND UDP") || strings.Contains(afterOff, "RECV UDP") {
		t.Errorf("SIP packets still shown after `debug off`:\n%s", afterOff)
	}
}

// TestDebugDefaultsOffAndReportsState checks `status` describes where packets
// are going, and that bare `debug` toggles.
func TestDebugDefaultsOffAndReportsState(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	out, code := runClient(t, cfgPath,
		"status\ndebug\nstatus\ndebug\nstatus\nquit\n", 40*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	// Off by default, naming the trace file; on after a bare toggle; off again.
	requireContains(t, out,
		"debug: off (SIP trace in",
		"debug: on (terminal +",
	)
	if n := strings.Count(out, "debug: off (SIP trace in"); n != 2 {
		t.Errorf("expected debug reported off twice (before and after toggling back), got %d\n%s",
			n, out)
	}
}

// TestDebugWithFileTracingDisabled: the command must still work when
// logging.sip_trace is off, and say that packets are terminal-only.
func TestDebugWithFileTracingDisabled(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Logging.SIPTrace = false
	})

	out, code := runClient(t, cfgPath,
		"debug on\ndial 2001 1\nwait 1 CONNECTED 8000\nhangup all\nquit\n",
		40*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out,
		"packets are shown here only",
		"INVITE sip:2001@testpbx SIP/2.0",
	)
}
