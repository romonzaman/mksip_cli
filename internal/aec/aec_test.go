package aec

import (
	"math"
	"math/rand"
	"testing"
)

const (
	rate  = 8000
	frame = 160 // 20ms
)

func newTest() *Canceller {
	return New(Config{SampleRate: rate, FrameSamples: frame, TailMS: 64})
}

// echoPath models a speaker-to-microphone path: a delay, an attenuation, and a
// couple of reflections. This is what the filter has to learn.
type echoPath struct {
	taps  []float32
	delay []float32
	pos   int
}

func newEchoPath(delaySamples int, gain float32) *echoPath {
	taps := make([]float32, delaySamples+40)
	taps[delaySamples] = gain
	taps[delaySamples+13] = gain * 0.4  // first reflection
	taps[delaySamples+31] = gain * 0.15 // second
	return &echoPath{taps: taps, delay: make([]float32, len(taps))}
}

func (e *echoPath) process(x float32) float32 {
	e.delay[e.pos] = x
	var y float32
	for k, w := range e.taps {
		idx := e.pos - k
		if idx < 0 {
			idx += len(e.delay)
		}
		y += w * e.delay[idx]
	}
	e.pos = (e.pos + 1) % len(e.delay)
	return y
}

// speech makes a signal with speech-like structure: a wandering tone with an
// amplitude envelope. White noise would be unrealistically easy to adapt to.
func speech(n int, seed int64) []float32 {
	r := rand.New(rand.NewSource(seed))
	out := make([]float32, n)
	phase, freq := 0.0, 200.0
	for i := range out {
		if i%400 == 0 {
			freq = 120 + r.Float64()*300
		}
		phase += 2 * math.Pi * freq / rate
		env := 0.35 * (0.6 + 0.4*math.Sin(2*math.Pi*float64(i)/2400))
		out[i] = float32(env * (math.Sin(phase) + 0.25*math.Sin(3*phase)))
	}
	return out
}

func silence(n int) []float32 { return make([]float32, n) }

func rms(x []float32) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

func db(num, den float64) float64 {
	if den == 0 || num == 0 {
		return 0
	}
	return 20 * math.Log10(num/den)
}

// run drives the canceller frame by frame and returns mic and output signals.
func run(c *Canceller, mic, ref []float32) (gotMic, gotOut []float32) {
	out := make([]float32, len(mic))
	for i := 0; i+frame <= len(mic); i += frame {
		c.processFloat(out[i:i+frame], mic[i:i+frame], ref[i:i+frame])
	}
	return mic, out
}

// TestCancelsEcho is the headline: with only far-end audio echoing back, the
// output must end up far quieter than the microphone.
func TestCancelsEcho(t *testing.T) {
	c := newTest()
	n := rate * 6 // 6 seconds to converge

	far := speech(n, 1)
	path := newEchoPath(24, 0.5)
	mic := make([]float32, n)
	for i := range mic {
		mic[i] = path.process(far[i])
	}

	_, out := run(c, mic, far)

	// Judge the last second, once the filter has had time to adapt.
	tailStart := n - rate
	erle := db(rms(mic[tailStart:]), rms(out[tailStart:]))

	t.Logf("ERLE after convergence: %.1f dB (stats %.1f dB)", erle, c.Stats().ERLE)
	if erle < 20 {
		t.Errorf("ERLE = %.1f dB, want >= 20 dB -- the echo is still audible", erle)
	}
	if !c.Converged() {
		t.Error("filter reports not converged after 6s of echo")
	}
}

// TestNearEndPassesThrough is the property that matters most for how the call
// sounds: with no far-end audio there is no echo, so the user's voice must
// arrive intact. An AEC that quietens the near-end talker is worse than the
// echo it removed.
func TestNearEndPassesThrough(t *testing.T) {
	c := newTest()
	n := rate * 3

	near := speech(n, 2)
	ref := silence(n)

	_, out := run(c, near, ref)

	loss := db(rms(near), rms(out))
	t.Logf("near-end level change: %.2f dB", loss)
	if math.Abs(loss) > 1.0 {
		t.Errorf("near-end speech changed by %.2f dB, want it left alone", loss)
	}
}

// TestDoubleTalkPreservesNearEnd: while both ends talk, the near-end voice must
// survive. This is where a naive canceller either diverges or gates the user.
func TestDoubleTalkPreservesNearEnd(t *testing.T) {
	c := newTest()
	n := rate * 8

	far := speech(n, 3)
	near := speech(n, 4)
	path := newEchoPath(24, 0.5)

	// Converge on echo alone for the first 5 seconds, then the near end joins.
	doubleTalkFrom := rate * 5
	mic := make([]float32, n)
	for i := range mic {
		mic[i] = path.process(far[i])
		if i >= doubleTalkFrom {
			mic[i] += near[i]
		}
	}

	_, out := run(c, mic, far)

	// During double-talk the output should resemble the near-end speech, not
	// be crushed towards silence.
	seg := doubleTalkFrom + frame*10 // skip the detector's reaction time
	nearLevel := rms(near[seg:])
	outLevel := rms(out[seg:])

	attenuation := db(nearLevel, outLevel)
	t.Logf("near-end attenuation during double-talk: %.1f dB", attenuation)
	if attenuation > 6 {
		t.Errorf("near-end speech attenuated by %.1f dB during double-talk; "+
			"the user would sound cut off", attenuation)
	}

	// And the filter must not have been wrecked by adapting to the near end.
	if !c.Converged() {
		t.Error("filter lost convergence during double-talk")
	}
}

// TestDoubleTalkIsDetected checks the detector itself fires.
func TestDoubleTalkIsDetected(t *testing.T) {
	c := newTest()
	n := rate * 2

	far := speech(n, 5)
	path := newEchoPath(24, 0.4)

	// Echo only: no double-talk should be reported.
	mic := make([]float32, n)
	for i := range mic {
		mic[i] = path.process(far[i])
	}
	run(c, mic, far)
	if c.Stats().DoubleTalk {
		t.Error("double-talk reported when only the far end is talking")
	}

	// Add a loud near-end talker: it must be detected.
	loud := speech(n, 6)
	for i := range mic {
		mic[i] += loud[i] * 2
	}
	run(c, mic, far)
	if !c.Stats().DoubleTalk {
		t.Error("double-talk not detected with a loud near-end talker")
	}
}

// TestSilenceIsStable guards against NaN and drift on quiet input, which is
// what a phone hears most of the time.
func TestSilenceIsStable(t *testing.T) {
	c := newTest()
	n := rate * 3

	_, out := run(c, silence(n), silence(n))

	for i, v := range out {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("output sample %d is %v on silent input", i, v)
		}
		if v != 0 {
			t.Fatalf("silent input produced %v at sample %d", v, i)
		}
	}
	for i, w := range c.taps {
		if math.IsNaN(float64(w)) || math.IsInf(float64(w), 0) {
			t.Fatalf("filter tap %d is %v after silence", i, w)
		}
	}
}

// TestLoudReferenceDoesNotDiverge: a very loud far end must not blow the filter
// up. NLMS normalisation is what prevents this.
func TestLoudReferenceDoesNotDiverge(t *testing.T) {
	c := newTest()
	n := rate * 4

	far := speech(n, 7)
	for i := range far {
		far[i] *= 3 // deliberately clipping-loud
	}
	path := newEchoPath(20, 0.8)
	mic := make([]float32, n)
	for i := range mic {
		mic[i] = path.process(far[i])
	}

	_, out := run(c, mic, far)

	for _, v := range out {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatal("filter diverged on a loud reference")
		}
	}
	if erle := db(rms(mic[n-rate:]), rms(out[n-rate:])); erle < 10 {
		t.Errorf("ERLE = %.1f dB with a loud reference, want >= 10", erle)
	}
}

// TestPCMRoundTrip checks the byte-level entry point, including that a
// mismatched frame is passed through rather than mangled.
func TestPCMRoundTrip(t *testing.T) {
	c := newTest()

	mic := make([]byte, frame*2)
	ref := make([]byte, frame*2)
	out := make([]byte, frame*2)
	for i := 0; i < frame; i++ {
		v := int16(1000 * math.Sin(2*math.Pi*float64(i)/40))
		mic[i*2] = byte(uint16(v))
		mic[i*2+1] = byte(uint16(v) >> 8)
	}

	c.Process(out, mic, ref)
	// With a silent reference nothing is cancelled, so the signal survives.
	var nonZero int
	for _, b := range out {
		if b != 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Error("a silent reference should leave the microphone signal intact")
	}

	// A short frame must pass through untouched, not be corrupted.
	short := []byte{1, 2, 3, 4}
	shortOut := make([]byte, 4)
	c.Process(shortOut, short, []byte{0, 0, 0, 0})
	for i := range short {
		if shortOut[i] != short[i] {
			t.Fatalf("unexpected frame size was altered at byte %d", i)
		}
	}
}

// TestResetForgetsThePath: a new call is a new room.
func TestResetForgetsThePath(t *testing.T) {
	c := newTest()
	n := rate * 3

	far := speech(n, 8)
	path := newEchoPath(24, 0.5)
	mic := make([]float32, n)
	for i := range mic {
		mic[i] = path.process(far[i])
	}
	run(c, mic, far)

	if !c.Converged() {
		t.Fatal("expected convergence before reset")
	}
	c.Reset()

	if c.Converged() {
		t.Error("still reports converged after Reset")
	}
	if s := c.Stats(); s.FramesProcessed != 0 || s.DoubleTalk {
		t.Errorf("stats not cleared by Reset: %+v", s)
	}
	for i, w := range c.taps {
		if w != 0 {
			t.Fatalf("tap %d = %v after Reset, want 0", i, w)
		}
	}
}

// BenchmarkProcess measures the cost of one 20ms frame. The audio callback has
// 20ms of wall clock to work with, so anything far under that is safe; this is
// the number that decides whether a time-domain filter is affordable at all.
func BenchmarkProcess(b *testing.B) {
	c := New(Config{SampleRate: rate, FrameSamples: frame, TailMS: DefaultTailMS})
	mic := make([]byte, frame*2)
	ref := make([]byte, frame*2)
	out := make([]byte, frame*2)
	for i := 0; i < frame; i++ {
		v := uint16(int16(3000 * math.Sin(float64(i)/7)))
		ref[i*2], ref[i*2+1] = byte(v), byte(v>>8)
		mic[i*2], mic[i*2+1] = byte(v/2), byte((v/2)>>8)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Process(out, mic, ref)
	}
	frameMS := float64(frame) / float64(rate) * 1000
	b.ReportMetric(frameMS, "ms/frame-budget")
}
