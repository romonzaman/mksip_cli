// Package media implements RTP transport, G.711 coding, a jitter buffer with
// packet-loss concealment, DTMF generation and local tones (requirements §8).
package media

import (
	"encoding/binary"
	"fmt"

	"github.com/zaf/g711"
)

// Codec is a negotiated audio codec.
type Codec struct {
	Name        string
	PayloadType uint8
	ClockRate   uint32
}

// Supported codecs (FR-8.1). Only static-payload G.711 in v1.
var (
	CodecPCMU = Codec{Name: "PCMU", PayloadType: 0, ClockRate: 8000}
	CodecPCMA = Codec{Name: "PCMA", PayloadType: 8, ClockRate: 8000}
)

// DTMFPayloadType is the dynamic payload type we offer for telephone-event.
const DTMFPayloadType uint8 = 101

// SampleRate is the only clock rate v1 handles.
const SampleRate = 8000

// BytesPerSample of 16-bit linear PCM.
const BytesPerSample = 2

// CodecByName resolves a configured codec name.
func CodecByName(name string) (Codec, error) {
	switch name {
	case "PCMU":
		return CodecPCMU, nil
	case "PCMA":
		return CodecPCMA, nil
	}
	return Codec{}, fmt.Errorf("unsupported codec %q", name)
}

// CodecByPayloadType resolves an inbound static payload type.
func CodecByPayloadType(pt uint8) (Codec, bool) {
	switch pt {
	case 0:
		return CodecPCMU, true
	case 8:
		return CodecPCMA, true
	}
	return Codec{}, false
}

// Encode converts 16-bit little-endian PCM to the codec's payload.
func (c Codec) Encode(pcm []byte) []byte {
	if c.PayloadType == CodecPCMA.PayloadType {
		return g711.EncodeAlaw(pcm)
	}
	return g711.EncodeUlaw(pcm)
}

// Decode converts a codec payload back to 16-bit little-endian PCM.
func (c Codec) Decode(payload []byte) []byte {
	if c.PayloadType == CodecPCMA.PayloadType {
		return g711.DecodeAlaw(payload)
	}
	return g711.DecodeUlaw(payload)
}

// FrameSamples is the sample count in one packet at the given ptime.
func FrameSamples(ptimeMS int) int { return SampleRate * ptimeMS / 1000 }

// FrameBytes is the PCM byte count in one packet at the given ptime.
func FrameBytes(ptimeMS int) int { return FrameSamples(ptimeMS) * BytesPerSample }

// applyGain scales PCM in place with clipping protection (FR §3.3 audio gains).
func applyGain(pcm []byte, gain float64) {
	if gain == 1.0 {
		return
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) * gain
		binary.LittleEndian.PutUint16(pcm[i:], uint16(clip16(v)))
	}
}

func clip16(v float64) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	}
	return int16(v)
}
