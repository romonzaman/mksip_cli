package main

import (
	"strings"
	"testing"
	"time"
)

// TestRealAudioDevice exercises the actual CGO audio path in the built binary:
// it opens the microphone and speakers, streams a call through them, and lists
// the devices. Skipped when no audio hardware is available (FR-8.6, NFR-1).
func TestRealAudioDevice(t *testing.T) {
	pbx := startPBX(t, nil)
	cfgPath := writeConfig(t, testDir(t), pbx, nil)

	script := strings.Join([]string{
		"devices",
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"sleep 700",
		"stats 1",
		"mute",
		"sleep 200",
		"unmute",
		"hangup all",
		"quit",
	}, "\n") + "\n"

	// No -no-audio: this opens the real device.
	out, code := runClientWithFlags(t, cfgPath, script, 40*time.Second)

	if strings.Contains(out, "audio unavailable") || strings.Contains(out, "no audio device") {
		t.Skipf("no usable audio device in this environment:\n%s", out)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	requireContains(t, out, "input ", "output", "in use:")
	requireContains(t, out, "connected, codec PCMU",
		"microphone muted", "microphone live")

	// Media must flow through the device path just as it does headless.
	if n := pbx.RTPPacketsReceived(); n < 10 {
		t.Errorf("PBX received %d RTP packets with a real device, want >= 10\n%s", n, out)
	}
}
