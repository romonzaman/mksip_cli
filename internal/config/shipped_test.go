package config

import "testing"

// TestShippedConfigUsesEphemeralPorts guards the promise that a fresh
// config.json needs no RTP port setup at all.
func TestShippedConfigUsesEphemeralPorts(t *testing.T) {
	// The template reads the password from the environment (FR-1.7).
	t.Setenv("SIP_PASSWORD", "placeholder")

	cfg, _, err := Load("../../config.json.example")
	if err != nil {
		t.Fatalf("shipped config.json.example does not load: %v", err)
	}
	if !cfg.EphemeralRTPPorts() {
		t.Errorf("shipped config should use OS-assigned RTP ports, got %d-%d",
			cfg.Media.RTPPortStart, cfg.Media.RTPPortEnd)
	}
}
