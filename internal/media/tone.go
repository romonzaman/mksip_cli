package media

import (
	"encoding/binary"
	"math"
)

// ToneGen synthesises a two-frequency cadenced tone, used for local ringback
// (FR-8.10: 440/480 Hz, 2 s on / 4 s off).
type ToneGen struct {
	f1, f2   float64
	onTicks  int
	offTicks int
	amp      float64

	phase int // sample counter, drives both oscillators
	tick  int // position in the on/off cadence
}

// NewRingback builds the standard North American ringback cadence.
func NewRingback(ptimeMS int) *ToneGen {
	ticksPerSec := 1000 / ptimeMS
	return &ToneGen{
		f1: 440, f2: 480,
		onTicks:  2 * ticksPerSec,
		offTicks: 4 * ticksPerSec,
		amp:      0.18 * 32767,
	}
}

// Frame returns the next frame of the cadence as 16-bit little-endian PCM.
func (t *ToneGen) Frame(ptimeMS int) []byte {
	n := FrameSamples(ptimeMS)
	out := make([]byte, n*BytesPerSample)

	silent := t.tick >= t.onTicks
	t.tick++
	if t.tick >= t.onTicks+t.offTicks {
		t.tick = 0
	}
	if silent {
		t.phase += n
		return out
	}

	for i := 0; i < n; i++ {
		s := float64(t.phase+i) / float64(SampleRate)
		v := t.amp * (math.Sin(2*math.Pi*t.f1*s) + math.Sin(2*math.Pi*t.f2*s)) / 2
		binary.LittleEndian.PutUint16(out[i*BytesPerSample:], uint16(clip16(v)))
	}
	t.phase += n
	return out
}

// Reset restarts the cadence.
func (t *ToneGen) Reset() { t.phase, t.tick = 0, 0 }
