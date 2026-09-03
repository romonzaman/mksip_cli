package audio

import (
	"fmt"
	"strings"
)

// Device is the capture/playback backend. The malgo implementation drives real
// hardware; NullDevice stands in for headless runs and tests (NFR-1).
type Device interface {
	Start() error
	Stop() error
	Close() error
	// Description names the devices actually in use.
	Description() string
}

// DeviceInfo describes one enumerated endpoint.
type DeviceInfo struct {
	Name      string
	IsDefault bool
	IsCapture bool
}

// Config configures a device.
type Config struct {
	SampleRate   int
	Channels     int
	FrameSamples int

	InputDevice  string // substring match; empty means system default
	OutputDevice string

	Router *Router
}

// matchDevice resolves a substring against enumerated names. An ambiguous or
// missing match is an error naming the candidates (FR §3.3 audio devices).
func matchDevice[T any](want string, items []T, name func(T) string, kind string) (idx int, err error) {
	if want == "" {
		return -1, nil // system default
	}
	var hits []int
	lower := strings.ToLower(want)
	for i, it := range items {
		if strings.Contains(strings.ToLower(name(it)), lower) {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		var names []string
		for _, it := range items {
			names = append(names, name(it))
		}
		return -1, fmt.Errorf("no %s device matches %q; available: %s",
			kind, want, strings.Join(names, ", "))
	default:
		var names []string
		for _, i := range hits {
			names = append(names, name(items[i]))
		}
		return -1, fmt.Errorf("%q matches %d %s devices (%s); use a more specific name",
			want, len(hits), kind, strings.Join(names, ", "))
	}
}

// NullDevice satisfies Device without touching hardware. It never produces or
// consumes audio, which lets signalling be exercised headlessly.
type NullDevice struct{}

func (NullDevice) Start() error        { return nil }
func (NullDevice) Stop() error         { return nil }
func (NullDevice) Close() error        { return nil }
func (NullDevice) Description() string { return "null (no audio device)" }
