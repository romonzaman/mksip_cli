package media

import (
	"encoding/binary"
	"strings"
	"testing"
)

// TestJitterBufferReordersAndPrimes covers FR-8.4's ordering behaviour.
func TestJitterBufferReordersAndPrimes(t *testing.T) {
	jb := NewJitterBuffer(60, 20) // target 3 frames

	frame := func(v int16) []byte {
		b := make([]byte, FrameBytes(20))
		binary.LittleEndian.PutUint16(b, uint16(v))
		return b
	}
	value := func(b []byte) int16 { return int16(binary.LittleEndian.Uint16(b)) }

	// Nothing comes out until the buffer has primed.
	if _, ok := jb.Pop(); ok {
		t.Fatal("Pop returned a frame before priming")
	}

	// Deliver out of order: 100, 102, 101.
	jb.Push(100, frame(1))
	jb.Push(102, frame(3))
	jb.Push(101, frame(2))

	for want := int16(1); want <= 3; want++ {
		got, ok := jb.Pop()
		if !ok {
			t.Fatalf("Pop %d returned nothing", want)
		}
		if value(got) != want {
			t.Errorf("frame order wrong: got %d, want %d", value(got), want)
		}
	}
}

// TestJitterBufferConcealsGap covers the PLC path in FR-8.4.
func TestJitterBufferConcealsGap(t *testing.T) {
	jb := NewJitterBuffer(40, 20) // target 2

	loud := make([]byte, FrameBytes(20))
	for i := 0; i+1 < len(loud); i += 2 {
		binary.LittleEndian.PutUint16(loud[i:], uint16(int16(1000)))
	}

	jb.Push(10, loud)
	jb.Push(12, loud) // 11 is missing

	first, ok := jb.Pop()
	if !ok || int16(binary.LittleEndian.Uint16(first)) != 1000 {
		t.Fatalf("first frame not returned intact")
	}

	// The gap must be concealed from the previous frame at reduced gain,
	// not returned as silence and not skipped.
	concealed, ok := jb.Pop()
	if !ok {
		t.Fatal("gap was not concealed")
	}
	if got := int16(binary.LittleEndian.Uint16(concealed)); got != 500 {
		t.Errorf("concealed sample = %d, want 500 (previous frame at -6dB)", got)
	}

	if _, _, nConcealed, _ := jb.Counters(); nConcealed != 1 {
		t.Errorf("concealed counter = %d, want 1", nConcealed)
	}
}

// TestJitterBufferDropsLateFrames covers the late-arrival rule in FR-8.4.
func TestJitterBufferDropsLateFrames(t *testing.T) {
	jb := NewJitterBuffer(20, 20) // target 1
	f := make([]byte, FrameBytes(20))

	jb.Push(50, f)
	if _, ok := jb.Pop(); !ok {
		t.Fatal("expected the first frame")
	}
	jb.Push(49, f) // arrives after its slot has played

	if _, late, _, _ := jb.Counters(); late != 1 {
		t.Errorf("late counter = %d, want 1", late)
	}
}

// TestSeqLessHandlesWraparound guards the RTP sequence comparison.
func TestSeqLessHandlesWraparound(t *testing.T) {
	cases := []struct {
		a, b uint16
		want bool
	}{
		{1, 2, true},
		{2, 1, false},
		{65535, 0, true}, // wraps forward
		{0, 65535, false},
		{5, 5, false},
	}
	for _, tc := range cases {
		if got := seqLess(tc.a, tc.b); got != tc.want {
			t.Errorf("seqLess(%d, %d) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestG711RoundTrip checks the codec wiring in both directions.
func TestG711RoundTrip(t *testing.T) {
	pcm := make([]byte, FrameBytes(20))
	for i := 0; i+1 < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(i*7-2000)))
	}

	for _, codec := range []Codec{CodecPCMU, CodecPCMA} {
		payload := codec.Encode(pcm)
		if len(payload) != FrameSamples(20) {
			t.Errorf("%s payload = %d bytes, want %d",
				codec.Name, len(payload), FrameSamples(20))
		}
		back := codec.Decode(payload)
		if len(back) != len(pcm) {
			t.Errorf("%s decode = %d bytes, want %d", codec.Name, len(back), len(pcm))
		}
		// G.711 is lossy; check the samples land close rather than exact.
		for i := 0; i+1 < len(pcm); i += 2 {
			orig := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
			got := int(int16(binary.LittleEndian.Uint16(back[i:])))
			if diff := orig - got; diff > 400 || diff < -400 {
				t.Fatalf("%s sample %d: got %d, want ~%d", codec.Name, i/2, got, orig)
			}
		}
	}
}

// TestDTMFValidation covers the digit set in FR-4.10.
func TestDTMFValidation(t *testing.T) {
	for _, ok := range []string{"0123456789", "*#", "ABCD", "abcd", "1234#"} {
		if err := ValidateDTMF(ok); err != nil {
			t.Errorf("ValidateDTMF(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "  ", "1E", "9X", "1,2"} {
		if err := ValidateDTMF(bad); err == nil {
			t.Errorf("ValidateDTMF(%q) = nil, want an error", bad)
		}
	}
}

// TestDTMFSenderEmitsEndPackets checks the RFC 4733 packet shape: audio-rate
// packets for the duration, then three with the end bit set.
func TestDTMFSenderEmitsEndPackets(t *testing.T) {
	s, err := newDTMFSender("5", 20)
	if err != nil {
		t.Fatal(err)
	}

	var normal, ends int
	for i := 0; i < 40; i++ {
		payload, _, _, ok := s.next(1000)
		if !ok {
			break
		}
		if payload == nil {
			continue // inter-digit gap
		}
		if payload[0] != 5 {
			t.Fatalf("event = %d, want 5", payload[0])
		}
		if payload[1]&0x80 != 0 {
			ends++
		} else {
			normal++
		}
	}

	if normal != dtmfDurationMS/20 {
		t.Errorf("audio-rate packets = %d, want %d", normal, dtmfDurationMS/20)
	}
	if ends != dtmfEndPackets {
		t.Errorf("end packets = %d, want %d", ends, dtmfEndPackets)
	}
	if !s.done() {
		t.Error("sender should be finished after one digit")
	}
}

// TestDTMFSenderFreezesTimestamp checks every packet of one event carries the
// event's start timestamp (RFC 4733 §2.5.1).
func TestDTMFSenderFreezesTimestamp(t *testing.T) {
	s, _ := newDTMFSender("1", 20)

	var seen []uint32
	clock := uint32(5000)
	for i := 0; i < 15; i++ {
		payload, ts, _, ok := s.next(clock)
		clock += uint32(FrameSamples(20)) // the session clock keeps running
		if !ok {
			break
		}
		if payload != nil {
			seen = append(seen, ts)
		}
	}

	if len(seen) == 0 {
		t.Fatal("no DTMF packets produced")
	}
	for i, ts := range seen {
		if ts != seen[0] {
			t.Fatalf("packet %d timestamp = %d, want the frozen start %d", i, ts, seen[0])
		}
	}
}

// TestPortPoolQuarantine covers FR-3.6: a released port is withheld so stray
// RTP from a finished call cannot land in a new session.
func TestPortPoolQuarantine(t *testing.T) {
	pool := NewPortPool(30000, 30007) // four even ports

	first, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if first%2 != 0 {
		t.Errorf("port %d is not even; RTCP needs port+1", first)
	}

	second, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if second == first || second%2 != 0 {
		t.Errorf("second port %d clashes or is odd (first %d)", second, first)
	}

	pool.Release(first)
	next, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if next == first {
		t.Errorf("port %d was reissued during its quarantine", first)
	}
}

// TestPortPoolExhaustion must fail clearly rather than hand out a bad port.
func TestPortPoolExhaustion(t *testing.T) {
	pool := NewPortPool(31000, 31001) // exactly one pair
	if _, err := pool.Acquire(); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := pool.Acquire(); err == nil {
		t.Error("expected exhaustion error from a single-pair pool")
	}
}

// TestApplyGainClips checks gain never wraps around (FR §3.3).
func TestApplyGainClips(t *testing.T) {
	pcm := make([]byte, 4)
	binary.LittleEndian.PutUint16(pcm[0:], uint16(int16(30000)))
	var neg int16 = -30000
	binary.LittleEndian.PutUint16(pcm[2:], uint16(neg))

	applyGain(pcm, 4.0)

	if got := int16(binary.LittleEndian.Uint16(pcm[0:])); got != 32767 {
		t.Errorf("positive clip = %d, want 32767", got)
	}
	if got := int16(binary.LittleEndian.Uint16(pcm[2:])); got != -32768 {
		t.Errorf("negative clip = %d, want -32768", got)
	}
}

// TestRingbackCadence checks the tone is on then off, per FR-8.10.
func TestRingbackCadence(t *testing.T) {
	tone := NewRingback(20)

	energy := func(b []byte) int64 {
		var sum int64
		for i := 0; i+1 < len(b); i += 2 {
			v := int64(int16(binary.LittleEndian.Uint16(b[i:])))
			sum += v * v
		}
		return sum
	}

	// First 2s (100 frames at 20ms) should carry tone.
	if energy(tone.Frame(20)) == 0 {
		t.Error("ringback started silent")
	}
	for i := 0; i < 99; i++ {
		tone.Frame(20)
	}
	// Now in the 4s silent part of the cadence.
	if got := energy(tone.Frame(20)); got != 0 {
		t.Errorf("expected silence in the off phase, got energy %d", got)
	}
}

// TestPortPoolSkipsUnusable covers the case where another process on the host
// holds ports inside the configured range: those pairs must be taken out of
// rotation, not retried forever.
func TestPortPoolSkipsUnusable(t *testing.T) {
	pool := NewPortPool(32000, 32007) // pairs 32000, 32002, 32004, 32006

	first, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	pool.MarkUnusable(first)

	second, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("Acquire returned the unusable port %d again", first)
	}
	if pool.Unusable() != 1 {
		t.Errorf("Unusable() = %d, want 1", pool.Unusable())
	}

	// An unusable port stays out even after Release.
	pool.Release(first)
	for i := 0; i < 3; i++ {
		p, err := pool.Acquire()
		if err != nil {
			break
		}
		if p == first {
			t.Fatalf("unusable port %d came back into rotation", first)
		}
	}
}

// TestPortPoolExhaustionMentionsHeldPorts: when the range is unusable rather
// than merely busy, the error should say so and point at the fix.
func TestPortPoolExhaustionMentionsHeldPorts(t *testing.T) {
	pool := NewPortPool(33000, 33003) // pairs 33000, 33002

	for i := 0; i < 2; i++ {
		p, err := pool.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		pool.MarkUnusable(p)
	}

	_, err := pool.Acquire()
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	for _, want := range []string{"held by another process", "rtp_port_start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\ngot: %v", want, err)
		}
	}
}
