//go:build !cgo

package audio

import "fmt"

// Open without CGO cannot reach a real device, so it degrades to silence
// rather than failing the whole client (NFR-1).
func Open(cfg Config) (Device, error) { return NullDevice{}, nil }

// List reports that enumeration needs CGO.
func List() ([]DeviceInfo, error) {
	return nil, fmt.Errorf("audio device enumeration requires a CGO build (CGO_ENABLED=1)")
}
