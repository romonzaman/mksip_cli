package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `{
  "sip": {"username":"1001","password":"pw","domain":"pbx.example","server":{"host":"10.0.0.1"}}
}`

// TestOmittedFieldsKeepDefaults is the property the loader depends on:
// decoding into a pre-filled struct must leave absent keys alone, so booleans
// that default true stay true (FR-1.4).
func TestOmittedFieldsKeepDefaults(t *testing.T) {
	cfg, _, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.Network.Rport {
		t.Error("network.rport lost its true default")
	}
	if !cfg.Media.RTCPEnabled {
		t.Error("media.rtcp_enabled lost its true default")
	}
	if !cfg.Logging.SIPTrace {
		t.Error("logging.sip_trace lost its true default")
	}
	if !cfg.Audio.RingbackEnabled {
		t.Error("audio.ringback_enabled lost its true default")
	}
	if !cfg.Transfer.HangupAfterSuccess {
		t.Error("transfer.hangup_after_success lost its true default")
	}
	if cfg.Audio.InputGain != 1.0 || cfg.Audio.OutputGain != 1.0 {
		t.Errorf("gains = %v/%v, want 1.0/1.0", cfg.Audio.InputGain, cfg.Audio.OutputGain)
	}
	if cfg.Media.PtimeMS != 20 || cfg.SIP.Server.Port != 5060 {
		t.Errorf("numeric defaults lost: ptime=%d port=%d",
			cfg.Media.PtimeMS, cfg.SIP.Server.Port)
	}
	// Derived values.
	if cfg.SIP.AuthUsername != "1001" || cfg.SIP.DisplayName != "1001" {
		t.Errorf("derived auth/display = %q/%q", cfg.SIP.AuthUsername, cfg.SIP.DisplayName)
	}
	if cfg.AOR() != "sip:1001@pbx.example" {
		t.Errorf("AOR() = %q", cfg.AOR())
	}
	if cfg.ServerAddr() != "10.0.0.1:5060" {
		t.Errorf("ServerAddr() = %q", cfg.ServerAddr())
	}
}

// TestExplicitFalseIsHonoured guards against a defaults-merge that ignores an
// explicit false.
func TestExplicitFalseIsHonoured(t *testing.T) {
	cfg, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"pw","domain":"d","server":{"host":"h"}},
	  "network":{"rport":false},
	  "logging":{"sip_trace":false}
	}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Network.Rport {
		t.Error("explicit rport=false was ignored")
	}
	if cfg.Logging.SIPTrace {
		t.Error("explicit sip_trace=false was ignored")
	}
}

// TestUnknownFieldRejected covers FR-1.3.
func TestUnknownFieldRejected(t *testing.T) {
	_, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"pw","domain":"d","server":{"host":"h"}},
	  "medai":{"ptime_ms":20}
	}`))
	if err == nil {
		t.Fatal("expected an error for a misspelled section")
	}
	if !strings.Contains(err.Error(), "medai") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// TestValidationNamesJSONPaths covers FR-1.2.
func TestValidationNamesJSONPaths(t *testing.T) {
	_, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"pw","domain":"d",
	         "server":{"host":"h","port":0,"transport":"tls"}},
	  "media":{"ptime_ms":17,"codecs":["G729"]},
	  "audio":{"input_gain":9.0}
	}`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		"sip.server.port", "sip.server.transport",
		"media.ptime_ms", "media.codecs", "audio.input_gain",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing path %q\ngot: %v", want, err)
		}
	}
}

// TestPasswordFromEnv covers FR-1.7.
func TestPasswordFromEnv(t *testing.T) {
	t.Setenv("SIPCLIENT_TEST_PW", "from-env")
	cfg, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"env:SIPCLIENT_TEST_PW","domain":"d",
	         "server":{"host":"h"}}
	}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SIP.Password != "from-env" {
		t.Errorf("password = %q, want the env value", cfg.SIP.Password)
	}

	// A missing variable must fail loudly rather than register with "env:...".
	if _, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"env:SIPCLIENT_TEST_ABSENT","domain":"d",
	         "server":{"host":"h"}}
	}`)); err == nil {
		t.Error("expected an error for an unset env password")
	}
}

// TestRedactedHidesPassword covers FR-1.5.
func TestRedactedHidesPassword(t *testing.T) {
	cfg, _, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Redacted()
	if r.SIP.Password != "***" {
		t.Errorf("Redacted password = %q", r.SIP.Password)
	}
	if cfg.SIP.Password != "pw" {
		t.Error("Redacted must not mutate the original config")
	}
}

// TestInsecurePermsDetected covers FR-1.6.
func TestInsecurePermsDetected(t *testing.T) {
	path := write(t, minimal)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, insecure, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !insecure {
		t.Error("world-readable config not flagged")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, insecure, _ := Load(path); insecure {
		t.Error("0600 config wrongly flagged as insecure")
	}
}

// TestRTPPortsDefaultToEphemeral: with nothing configured the OS assigns a
// port per call, so the client needs no port setup at all.
func TestRTPPortsDefaultToEphemeral(t *testing.T) {
	cfg, _, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EphemeralRTPPorts() {
		t.Errorf("expected OS-assigned ports by default, got %d-%d",
			cfg.Media.RTPPortStart, cfg.Media.RTPPortEnd)
	}
}

// TestRTPRangeExplicitIsHonoured: a range can still be pinned for firewalls.
func TestRTPRangeExplicitIsHonoured(t *testing.T) {
	cfg, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"pw","domain":"d","server":{"host":"h"}},
	  "media":{"rtp_port_start":30000,"rtp_port_end":30100}
	}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EphemeralRTPPorts() {
		t.Error("an explicit range should not be treated as ephemeral")
	}
	if cfg.Media.RTPPortStart != 30000 || cfg.Media.RTPPortEnd != 30100 {
		t.Errorf("range = %d-%d", cfg.Media.RTPPortStart, cfg.Media.RTPPortEnd)
	}
}

// TestRTPRangeHalfConfiguredRejected: setting one end only is a mistake worth
// naming, rather than silently meaning something.
func TestRTPRangeHalfConfiguredRejected(t *testing.T) {
	_, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"pw","domain":"d","server":{"host":"h"}},
	  "media":{"rtp_port_end":30100}
	}`))
	if err == nil || !strings.Contains(err.Error(), "media.rtp_port_start") {
		t.Errorf("expected an error naming media.rtp_port_start, got: %v", err)
	}
}

// TestRTPRangeNeedsRoomForTwoChannels guards the two-channel invariant when a
// range is pinned explicitly.
func TestRTPRangeNeedsRoomForTwoChannels(t *testing.T) {
	_, _, err := Load(write(t, `{
	  "sip":{"username":"1001","password":"pw","domain":"d","server":{"host":"h"}},
	  "media":{"rtp_port_start":16000,"rtp_port_end":16002}
	}`))
	if err == nil || !strings.Contains(err.Error(), "at least 4 ports") {
		t.Errorf("expected a port-range error, got: %v", err)
	}
}
