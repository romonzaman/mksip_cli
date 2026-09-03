package media

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// dtmfEvent maps a digit to its RFC 4733 event code.
func dtmfEvent(digit rune) (uint8, error) {
	switch {
	case digit >= '0' && digit <= '9':
		return uint8(digit - '0'), nil
	}
	switch digit {
	case '*':
		return 10, nil
	case '#':
		return 11, nil
	case 'A', 'a':
		return 12, nil
	case 'B', 'b':
		return 13, nil
	case 'C', 'c':
		return 14, nil
	case 'D', 'd':
		return 15, nil
	}
	return 0, fmt.Errorf("invalid DTMF digit %q (valid: 0-9 * # A-D)", digit)
}

// ValidateDTMF checks a digit string before it is queued (FR-4.10).
func ValidateDTMF(digits string) error {
	if strings.TrimSpace(digits) == "" {
		return fmt.Errorf("no digits given")
	}
	for _, d := range digits {
		if _, err := dtmfEvent(d); err != nil {
			return err
		}
	}
	return nil
}

// DTMF timing (FR-4.10): each digit is held for Duration, then three packets
// repeat with the end bit set, then the line goes quiet for InterDigitGap.
const (
	dtmfDurationMS   = 200
	dtmfEndPackets   = 3
	dtmfInterDigitMS = 60
	dtmfVolume       = 10 // dBm0 below full scale
)

// dtmfPayload builds the 4-byte telephone-event payload.
func dtmfPayload(event uint8, end bool, durationSamples uint16) []byte {
	b := make([]byte, 4)
	b[0] = event
	b[1] = dtmfVolume
	if end {
		b[1] |= 0x80
	}
	binary.BigEndian.PutUint16(b[2:], durationSamples)
	return b
}

// dtmfSender walks one digit string, yielding one packet per ptime tick.
type dtmfSender struct {
	events []uint8
	ptime  int

	idx       int // current digit
	sent      int // audio-rate packets sent for this digit
	endsSent  int // end packets sent for this digit
	gapTicks  int // remaining silence ticks after a digit
	startTS   uint32
	haveStart bool
}

func newDTMFSender(digits string, ptimeMS int) (*dtmfSender, error) {
	events := make([]uint8, 0, len(digits))
	for _, d := range digits {
		ev, err := dtmfEvent(d)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return &dtmfSender{events: events, ptime: ptimeMS}, nil
}

// next returns the payload for this tick. When ok is false the sender is done.
// freezeTS is true while an event is in progress: every packet of one event
// carries the timestamp of the event's first packet (RFC 4733 §2.5.1).
func (d *dtmfSender) next(currentTS uint32) (payload []byte, ts uint32, freezeTS bool, ok bool) {
	if d.idx >= len(d.events) {
		return nil, 0, false, false
	}

	if d.gapTicks > 0 {
		d.gapTicks--
		if d.gapTicks == 0 {
			d.idx++
			d.sent, d.endsSent, d.haveStart = 0, 0, false
		}
		return nil, 0, false, true // silence between digits
	}

	if !d.haveStart {
		d.startTS, d.haveStart = currentTS, true
	}

	packetsPerDigit := dtmfDurationMS / d.ptime
	if packetsPerDigit < 1 {
		packetsPerDigit = 1
	}

	event := d.events[d.idx]
	switch {
	case d.sent < packetsPerDigit:
		d.sent++
		dur := uint16(d.sent * FrameSamples(d.ptime))
		return dtmfPayload(event, false, dur), d.startTS, true, true

	case d.endsSent < dtmfEndPackets:
		d.endsSent++
		dur := uint16(packetsPerDigit * FrameSamples(d.ptime))
		if d.endsSent == dtmfEndPackets {
			d.gapTicks = dtmfInterDigitMS / d.ptime
			if d.gapTicks < 1 {
				d.gapTicks = 1
			}
		}
		return dtmfPayload(event, true, dur), d.startTS, true, true
	}
	return nil, 0, false, true
}

// done reports whether every digit has been sent.
func (d *dtmfSender) done() bool { return d.idx >= len(d.events) }
