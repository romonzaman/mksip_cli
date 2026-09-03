package audio

import (
	"encoding/binary"
	"testing"
)

func pcm(samples ...int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

func samples(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// TestRingReadRequiresFullFrame: a partial read would produce a clicking
// half-frame, so Read must refuse rather than return short.
func TestRingReadRequiresFullFrame(t *testing.T) {
	r := newRing(8)
	r.Write([]byte{1, 2, 3})

	buf := make([]byte, 4)
	if r.Read(buf) {
		t.Error("Read succeeded with only 3 of 4 bytes buffered")
	}

	r.Write([]byte{4})
	if !r.Read(buf) {
		t.Fatal("Read failed with a full frame buffered")
	}
	if buf[0] != 1 || buf[3] != 4 {
		t.Errorf("Read returned %v, want [1 2 3 4]", buf)
	}
}

// TestRingDropsOldestOnOverflow: stale audio must be discarded rather than
// allowed to grow into ever-increasing latency.
func TestRingDropsOldestOnOverflow(t *testing.T) {
	r := newRing(4)
	r.Write([]byte{1, 2, 3, 4})
	r.Write([]byte{5, 6}) // pushes 1,2 out

	buf := make([]byte, 4)
	if !r.Read(buf) {
		t.Fatal("Read failed after overflow")
	}
	want := []byte{3, 4, 5, 6}
	for i := range want {
		if buf[i] != want[i] {
			t.Fatalf("after overflow got %v, want %v", buf, want)
		}
	}
}

// TestMixIntoClips guards the 3-way consult mix against wraparound (FR-7.4).
func TestMixIntoClips(t *testing.T) {
	dst := pcm(20000, -20000, 100)
	src := pcm(20000, -20000, 50)

	mixInto(dst, src)

	got := samples(dst)
	want := []int16{32767, -32768, 150}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mix[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestRouterRoutesOnlyAttachedLegs is the core of FR-8.7: a detached (held or
// inactive) leg must neither receive the microphone nor reach the speaker.
func TestRouterRoutesOnlyAttachedLegs(t *testing.T) {
	frame := 4
	r := NewRouter(frame, 4)
	a := r.AddLeg(1)
	b := r.AddLeg(2)

	a.SetAttached(true, true)
	b.SetAttached(false, false)

	mic := pcm(500, 600)
	r.ProcessCapture(mic)

	buf := make([]byte, frame)
	if !a.tx.Read(buf) {
		t.Error("attached leg did not receive microphone audio")
	}
	if b.tx.Read(buf) {
		t.Error("detached leg received microphone audio")
	}

	// Playback: only the attached leg contributes.
	a.WriteRX(pcm(1000, 1000))
	b.WriteRX(pcm(7000, 7000)) // dropped, leg is detached

	out := make([]byte, frame)
	r.ProcessPlayback(out)
	if got := samples(out); got[0] != 1000 {
		t.Errorf("playback = %v, want only the attached leg's 1000", got)
	}
}

// TestRouterMuteStopsCapture covers the mute command.
func TestRouterMuteStopsCapture(t *testing.T) {
	r := NewRouter(4, 4)
	leg := r.AddLeg(1)
	leg.SetAttached(true, true)

	r.SetMuted(true)
	r.ProcessCapture(pcm(900, 900))

	if leg.tx.Read(make([]byte, 4)) {
		t.Error("microphone audio reached a leg while muted")
	}

	r.SetMuted(false)
	r.ProcessCapture(pcm(900, 900))
	if !leg.tx.Read(make([]byte, 4)) {
		t.Error("microphone audio blocked after unmute")
	}
}

// TestDetachFlushesBuffers: stale audio must not replay when a held channel is
// retrieved.
func TestDetachFlushesBuffers(t *testing.T) {
	r := NewRouter(4, 4)
	leg := r.AddLeg(1)
	leg.SetAttached(true, true)

	leg.WriteRX(pcm(1234, 1234))
	leg.SetAttached(false, false) // hold
	leg.SetAttached(true, true)   // retrieve

	out := make([]byte, 4)
	r.ProcessPlayback(out)
	if got := samples(out); got[0] != 0 {
		t.Errorf("stale audio replayed after retrieve: %v", got)
	}
}

// TestProcessPlaybackZeroesOutput: an underrun must be silence, not whatever
// the device buffer held.
func TestProcessPlaybackZeroesOutput(t *testing.T) {
	r := NewRouter(4, 4)
	out := []byte{9, 9, 9, 9}
	r.ProcessPlayback(out)
	for i, b := range out {
		if b != 0 {
			t.Fatalf("output[%d] = %d, want 0", i, b)
		}
	}
}

// TestMatchDevice covers the substring resolution rules.
func TestMatchDevice(t *testing.T) {
	devices := []string{"MacBook Pro Microphone", "Microsoft Teams Audio", "USB Mic"}
	id := func(s string) string { return s }

	if idx, err := matchDevice("", devices, id, "input"); err != nil || idx != -1 {
		t.Errorf("empty selector should mean system default, got %d, %v", idx, err)
	}
	if idx, err := matchDevice("teams", devices, id, "input"); err != nil || idx != 1 {
		t.Errorf("case-insensitive match failed: %d, %v", idx, err)
	}
	if _, err := matchDevice("nonexistent", devices, id, "input"); err == nil {
		t.Error("expected an error naming the candidates")
	}
	// "Mic" matches both "Microphone" and "USB Mic": ambiguity must be an error.
	_, err := matchDevice("Mic", devices, id, "input")
	if err == nil {
		t.Error("expected an ambiguity error")
	}
}
