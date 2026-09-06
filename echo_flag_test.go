package main

import (
	"flag"
	"strings"
	"testing"

	"sipclient/internal/config"
)

// TestEchoCancelFlagOverride pins the three states -echo-cancel must express:
// absent leaves the config alone, and either explicit value wins over it.
func TestEchoCancelFlagOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		fromFile bool
		want     bool
	}{
		{"absent keeps config off", nil, false, false},
		{"absent keeps config on", nil, true, true},
		{"-echo-cancel turns it on", []string{"-echo-cancel"}, false, true},
		{"-echo-cancel=true turns it on", []string{"-echo-cancel=true"}, false, true},
		{"-echo-cancel=false turns it off", []string{"-echo-cancel=false"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			echoCancel := fs.Bool("echo-cancel", false, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse: %v", err)
			}

			cfg := config.Default()
			cfg.Audio.EchoCancel = tc.fromFile
			if flagWasSet(fs, "echo-cancel") {
				cfg.Audio.EchoCancel = *echoCancel
			}

			if cfg.Audio.EchoCancel != tc.want {
				t.Errorf("EchoCancel = %v, want %v (config had %v, args %v)",
					cfg.Audio.EchoCancel, tc.want, tc.fromFile, tc.args)
			}
		})
	}
}

// TestEchoCancelDefaultOff pins the documented default.
func TestEchoCancelDefaultOff(t *testing.T) {
	d := config.Default()
	if d.Audio.EchoCancel {
		t.Error("audio.echo_cancel defaults to on; it must default to off")
	}
	if d.Audio.EchoTailMS != 128 {
		t.Errorf("audio.echo_tail_ms = %d, want 128", d.Audio.EchoTailMS)
	}
	// The tail must stay valid even though cancellation is off, so turning it
	// on with -echo-cancel alone cannot fail validation on the tail. (Validate
	// still reports the SIP fields every config must supply; those are not
	// this test's business, so only the tail rule is asserted.)
	d.Audio.EchoCancel = true
	if err := d.Validate(); err != nil && strings.Contains(err.Error(), "echo_tail_ms") {
		t.Errorf("defaults + -echo-cancel must not fail on the tail, got: %v", err)
	}
}
