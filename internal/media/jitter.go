package media

import (
	"encoding/binary"
	"sync"
)

// JitterBuffer reorders packets by sequence number, discards late arrivals and
// conceals gaps by replaying the previous frame at reduced gain (FR-8.4).
//
// It is a fixed-delay buffer: it primes with target frames before releasing the
// first one, which trades latency for tolerance to reordering.
type JitterBuffer struct {
	mu     sync.Mutex
	frames map[uint16][]byte

	target int // frames of nominal delay
	next   uint16
	primed bool
	last   []byte // most recent good frame, for concealment

	frameBytes int

	// Counters exposed through Stats (FR-8.9).
	received  uint64
	late      uint64
	concealed uint64
	overflow  uint64
}

// NewJitterBuffer sizes the buffer from the configured delay and ptime.
// A delay of 0 yields a pass-through buffer of one frame.
func NewJitterBuffer(delayMS, ptimeMS int) *JitterBuffer {
	target := delayMS / ptimeMS
	if target < 1 {
		target = 1
	}
	return &JitterBuffer{
		frames:     make(map[uint16][]byte, target*4),
		target:     target,
		frameBytes: FrameBytes(ptimeMS),
	}
}

// seqLess reports whether a precedes b in RTP sequence space.
func seqLess(a, b uint16) bool { return int16(a-b) < 0 }

// Push inserts a decoded frame. Frames already played out are dropped as late.
func (j *JitterBuffer) Push(seq uint16, pcm []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.received++
	if j.primed && seqLess(seq, j.next) {
		j.late++
		return
	}
	// Guard against a stalled reader growing the map without bound.
	if len(j.frames) > j.target*8 {
		j.overflow++
		j.frames = make(map[uint16][]byte, j.target*4)
		j.primed = false
		return
	}
	j.frames[seq] = pcm
}

// Pop returns the next frame for playout. ok is false when the buffer is still
// priming or has run dry, in which case the caller should emit silence.
func (j *JitterBuffer) Pop() (pcm []byte, ok bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if !j.primed {
		if len(j.frames) < j.target {
			return nil, false
		}
		// Start at the oldest sequence we hold.
		first := true
		for seq := range j.frames {
			if first || seqLess(seq, j.next) {
				j.next, first = seq, false
			}
		}
		j.primed = true
	}

	if f, hit := j.frames[j.next]; hit {
		delete(j.frames, j.next)
		j.next++
		j.last = f
		return f, true
	}

	if len(j.frames) == 0 {
		// Underrun: re-prime rather than concealing indefinitely.
		j.primed = false
		return nil, false
	}

	// Gap with later frames waiting: conceal and move on (FR-8.4).
	j.next++
	j.concealed++
	return j.conceal(), true
}

// conceal synthesises a replacement frame from the last good one at -6 dB.
func (j *JitterBuffer) conceal() []byte {
	if len(j.last) == 0 {
		return make([]byte, j.frameBytes)
	}
	out := make([]byte, len(j.last))
	for i := 0; i+1 < len(j.last); i += 2 {
		v := int16(binary.LittleEndian.Uint16(j.last[i:])) / 2
		binary.LittleEndian.PutUint16(out[i:], uint16(v))
	}
	j.last = out
	return out
}

// Counters snapshots the buffer statistics.
func (j *JitterBuffer) Counters() (received, late, concealed, overflow uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.received, j.late, j.concealed, j.overflow
}
