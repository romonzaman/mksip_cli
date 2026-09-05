package main

import (
	"strings"
	"testing"
	"time"

	"sipclient/internal/config"
)

// TestEchoCancellationOnRealDevice opens the real audio hardware in duplex and
// runs a call through it, which is the only way to know the device side works.
// Skipped where there is no usable audio device.
func TestEchoCancellationOnRealDevice(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Audio.EchoCancel = true
		c.Audio.EchoTailMS = 128
	})

	script := strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"sleep 1200",
		"stats 1",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	// No -no-audio: this must touch the real device.
	out, code := runClientWithFlags(t, cfgPath, script, 40*time.Second)

	if strings.Contains(out, "audio unavailable") || strings.Contains(out, "no audio device") {
		t.Skipf("no usable audio device here:\n%s", out)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	// Either duplex opened and echo cancellation is on, or it fell back and
	// said so plainly. Both are acceptable; silence about it is not.
	switch {
	case strings.Contains(out, "echo cancellation: "):
		// Reported by `stats`, so the canceller is running.
	case strings.Contains(out, "echo cancellation unavailable"):
		t.Logf("duplex unavailable on this host; fell back as designed")
	default:
		t.Errorf("no word either way about echo cancellation\n%s", out)
	}
}

// TestEchoCancellationDisabledUsesSeparateDevices: with it off the client must
// behave exactly as before.
func TestEchoCancellationDisabled(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, func(c *config.Config) {
		c.Audio.EchoCancel = false
	})

	out, code := runClientWithFlags(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 8000\nstats 1\nhangup all\nquit\n",
		40*time.Second)

	if strings.Contains(out, "audio unavailable") || strings.Contains(out, "no audio device") {
		t.Skip("no usable audio device here")
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "echo cancellation:") {
		t.Error("echo cancellation reported though it is disabled")
	}
}
