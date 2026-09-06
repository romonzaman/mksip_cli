// Package aec cancels acoustic echo: the far end's voice, played from the
// speaker, picked up again by the microphone and sent back to them.
//
// Without it a speakerphone is unusable -- the far end hears themselves, and at
// enough gain the loop howls.
//
// The design rests on one property of the audio layer: the device runs in
// duplex, so each callback hands over the microphone frame and the exact
// speaker frame that accompanies it. The reference is therefore sample-aligned
// with the capture, and none of the usual delay estimation or clock-drift
// compensation between two independent devices is needed. That is what makes a
// straightforward time-domain filter viable here.
//
// Three stages, in order:
//
//  1. An NLMS adaptive filter learns the echo path and subtracts its estimate.
//     This is the part that does the real work.
//  2. A double-talk detector freezes adaptation while the near-end talker is
//     speaking. Without it the filter adapts to the user's own voice and
//     diverges, and the echo comes back worse than before.
//  3. Residual suppression removes what the linear filter could not, which is
//     what makes the difference between "quieter echo" and "no echo".
//
// The overriding rule is that the near-end talker is never suppressed. An
// echo canceller that clips the user's own voice sounds half-duplex, and that
// is worse to talk through than the echo it removed.
package aec

import (
	"encoding/binary"
	"math"
	"sync/atomic"
)

// Config describes the echo canceller.
type Config struct {
	// SampleRate of the audio, in Hz.
	SampleRate int
	// FrameSamples is how many samples arrive per Process call.
	FrameSamples int
	// TailMS is how long an echo path to model. Longer covers more room
	// reflection but converges more slowly and costs more CPU. A laptop
	// speaker to its own microphone is a short path; 128ms is ample.
	TailMS int
}

// DefaultTailMS covers a laptop's speaker-to-microphone path with margin.
const DefaultTailMS = 128

// Stats reports how well cancellation is working.
type Stats struct {
	// ERLE is Echo Return Loss Enhancement in dB: how much quieter the output
	// is than the microphone input while the far end is talking. Above ~20dB
	// is good; near 0 means the filter has not converged or there is no echo.
	ERLE float64
	// Converged is true once the filter has adapted enough to be trusted.
	Converged bool
	// DoubleTalk is true when the near-end talker is currently speaking, and
	// adaptation is therefore frozen.
	DoubleTalk bool
	// FramesProcessed counts frames seen.
	FramesProcessed uint64
	// SuppressionDB is the residual suppression currently applied.
	SuppressionDB float64
}

// Canceller removes echo from a microphone stream.
//
// Process must be called from one goroutine only -- in practice the audio
// callback, which is a single real-time thread. Stats is the exception: it is
// safe from any goroutine, because the values it reports are published
// atomically at the end of each frame rather than read from the working state.
// Reporting must never block the audio thread.
type Canceller struct {
	cfg Config

	// taps is the adaptive filter's impulse response estimate.
	taps []float32
	// ref is a delay line of far-end samples, newest last,長 enough that every
	// tap has a sample to multiply.
	ref []float32

	// refPower tracks the reference energy over the tail, for NLMS
	// normalisation. Maintained incrementally rather than recomputed.
	refPower float64

	// Running energies used for ERLE and for the detectors.
	micEnergy float64
	outEnergy float64
	refEnergy float64

	frames        uint64
	adaptedFrames uint64

	doubleTalk     bool
	doubleTalkHold int
	// echoFloor is the best residual-to-microphone ratio achieved so far, i.e.
	// how well this filter cancels when only echo is present. Near-end speech
	// is what pushes the current ratio above it.
	echoFloor float64
	// echoGain is the microphone-to-reference energy ratio this echo path
	// delivers -- whatever the speaker volume and microphone preamp combine to
	// make it, which is not knowable in advance. It is the baseline the
	// pre-convergence detector compares against, so that detector works at any
	// hardware gain instead of assuming the echo is quieter than the reference.
	echoGain float64

	suppressionGain float32

	// scratch avoids allocating in the audio callback.
	micBuf []float32
	refBuf []float32
	outBuf []float32

	// Published stats. Written by the audio thread at the end of each frame,
	// read by anyone. Atomics rather than a mutex so a slow reader can never
	// stall audio.
	statERLE        atomic.Uint64 // float64 bits
	statSuppression atomic.Uint64 // float64 bits
	statFrames      atomic.Uint64
	statFlags       atomic.Uint32
}

// Bits in statFlags.
const (
	flagConverged  = 1 << 0
	flagDoubleTalk = 1 << 1
)

// Tunables. These are the values that matter, so they are named rather than
// buried as literals in the arithmetic.
const (
	// stepSize is the NLMS adaptation rate. Below 1 for stability; lower is
	// slower to converge but less likely to be disturbed by noise.
	stepSize = 0.3
	// regularisation keeps the NLMS denominator away from zero on quiet input.
	regularisation = 1e-6
	// geigelRise is how far the microphone-to-reference energy ratio must climb
	// above the echo path's own measured gain before near-end speech is
	// declared, while the filter is still converging. 4.0 in energy is 2x in
	// amplitude, about 6dB.
	//
	// It is measured against the path gain, not against the reference level.
	// Comparing the microphone directly to the reference assumes the echo comes
	// back quieter than the signal that produced it; on real hardware it does
	// not, because the microphone preamp applies gain the digital reference
	// knows nothing about. A laptop in a quiet room measures a mic peak several
	// times the reference peak, so that test fired on every single frame and
	// froze adaptation permanently: the filter never converged, the residual
	// detector below never took over, and no echo was ever cancelled.
	geigelRise = 4.0
	// gainFall and gainRise track the measured path gain asymmetrically: drop
	// towards a newly observed quieter ratio quickly, since only echo can be
	// that quiet, but climb back slowly so near-end speech cannot drag the
	// baseline up behind it.
	gainFall = 0.3
	gainRise = 0.001
	// dtdResidualRise is how far the residual-to-microphone ratio must climb
	// above its established floor before near-end speech is declared. Once
	// converged, the filter cancels echo to a steady floor; near-end speech
	// cannot be cancelled, so it shows up immediately as a jump. Roughly 8dB.
	dtdResidualRise = 6.0
	// floorDecay lets the established cancellation floor drift back up, so a
	// changed echo path is not mistaken for permanent double-talk.
	floorDecay = 1.0005
	// doubleTalkHoldFrames keeps adaptation frozen briefly after near-end
	// speech stops, since the filter must not chase the tail of it.
	doubleTalkHoldFrames = 12
	// convergedAfterFrames is how much adaptation must happen before the
	// residual suppressor is allowed to trust the filter.
	convergedAfterFrames = 50
	// refActiveEnergy is the reference energy below which the far end counts
	// as silent, so nothing is suppressed and nothing is adapted.
	refActiveEnergy = 1e-5
	// maxSuppression is the floor for residual suppression, about -18dB. Total
	// gating sounds dead and unnatural, so some signal is always let through.
	maxSuppression = 0.125
	// energySmoothing is the decay for the running energy averages.
	energySmoothing = 0.95
)

// New builds a canceller. A nil return is impossible; an unusable config is
// corrected to something workable rather than failing, because the audio path
// must not be brought down by a tuning value.
func New(cfg Config) *Canceller {
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 8000
	}
	if cfg.FrameSamples <= 0 {
		cfg.FrameSamples = cfg.SampleRate / 50 // 20ms
	}
	if cfg.TailMS <= 0 {
		cfg.TailMS = DefaultTailMS
	}

	tail := cfg.SampleRate * cfg.TailMS / 1000
	c := &Canceller{
		cfg:             cfg,
		taps:            make([]float32, tail),
		ref:             make([]float32, tail+cfg.FrameSamples),
		suppressionGain: 1,
		echoFloor:       1,
		echoGain:        math.Inf(1), // seeded by the first frame that has echo
		micBuf:          make([]float32, cfg.FrameSamples),
		refBuf:          make([]float32, cfg.FrameSamples),
		outBuf:          make([]float32, cfg.FrameSamples),
	}
	return c
}

// TailSamples is the modelled echo path length.
func (c *Canceller) TailSamples() int { return len(c.taps) }

// Reset forgets the learned echo path. Worth doing when the call changes, since
// the previous path no longer applies.
func (c *Canceller) Reset() {
	for i := range c.taps {
		c.taps[i] = 0
	}
	for i := range c.ref {
		c.ref[i] = 0
	}
	c.refPower = 0
	c.micEnergy, c.outEnergy, c.refEnergy = 0, 0, 0
	c.frames, c.adaptedFrames = 0, 0
	c.doubleTalk, c.doubleTalkHold = false, 0
	c.suppressionGain = 1
	c.echoFloor = 1
	c.echoGain = math.Inf(1)
	c.publishStats()
}

// Process removes echo from one frame of microphone audio.
//
// mic and ref are 16-bit little-endian PCM of the same length: the captured
// frame and the speaker frame that accompanied it. The result is written into
// out, which may alias mic. Frames of unexpected length are passed through
// unchanged rather than mangled.
func (c *Canceller) Process(out, mic, ref []byte) {
	n := len(mic) / 2
	if n == 0 || len(ref) != len(mic) || len(out) != len(mic) || n != c.cfg.FrameSamples {
		if !sameSlice(out, mic) {
			copy(out, mic)
		}
		return
	}

	decodePCM(c.micBuf[:n], mic)
	decodePCM(c.refBuf[:n], ref)

	c.processFloat(c.outBuf[:n], c.micBuf[:n], c.refBuf[:n])
	encodePCM(out, c.outBuf[:n])
}

// processFloat is the actual algorithm, separated so tests can drive it with
// float signals and no PCM conversion in the way.
func (c *Canceller) processFloat(out, mic, ref []float32) {
	n := len(mic)
	tail := len(c.taps)

	// Slide the reference delay line and append this frame.
	copy(c.ref, c.ref[n:])
	copy(c.ref[len(c.ref)-n:], ref)

	frameRefEnergy := energy(ref)
	frameMicEnergy := energy(mic)
	c.refEnergy = smooth(c.refEnergy, frameRefEnergy)
	c.micEnergy = smooth(c.micEnergy, frameMicEnergy)

	farActive := c.refEnergy > refActiveEnergy
	c.detectDoubleTalk(farActive, frameMicEnergy, frameRefEnergy)
	adapt := farActive && !c.doubleTalk

	for i := 0; i < n; i++ {
		// The window of reference samples this output sample can echo from,
		// oldest first, ending at the sample played alongside mic[i].
		end := len(c.ref) - n + i + 1
		window := c.ref[end-tail : end]

		// Estimate the echo and subtract it.
		var estimate float32
		for k := 0; k < tail; k++ {
			// taps[k] weights the sample k back from now.
			estimate += c.taps[k] * window[tail-1-k]
		}
		err := mic[i] - estimate
		out[i] = err

		if !adapt {
			continue
		}

		// NLMS: step proportional to the error, normalised by reference power
		// so loud and quiet speech adapt at the same rate.
		power := c.windowPower(window)
		if power < regularisation {
			continue
		}
		scale := stepSize * err / float32(power+regularisation)
		for k := 0; k < tail; k++ {
			c.taps[k] += scale * window[tail-1-k]
		}
	}

	if adapt {
		c.adaptedFrames++
	}
	c.frames++

	c.outEnergy = smooth(c.outEnergy, energy(out))
	c.updateEchoFloor(farActive)
	c.applyResidualSuppression(out, farActive)
	c.publishStats()
}

// windowPower is the reference energy over the filter window, for NLMS
// normalisation.
func (c *Canceller) windowPower(window []float32) float64 {
	var sum float64
	for _, v := range window {
		sum += float64(v) * float64(v)
	}
	return sum
}

// detectDoubleTalk decides whether the near-end talker is speaking, and
// therefore whether adaptation must be frozen. Adapting to the near-end voice
// makes the filter diverge and the echo return worse than before.
//
// Two detectors, because neither works alone:
//
//   - Before convergence there is no residual to reason about, so the
//     microphone is compared against the echo path's own measured gain: near-end
//     speech is what pushes that ratio above where this path normally sits.
//     Comparing against the reference level instead would assume the echo comes
//     back quieter than the signal that produced it, which real hardware does
//     not honour -- and firing on every frame would block the adaptation needed
//     to converge at all, so the detector below could never take over.
//   - Once converged, the filter cancels echo down to a steady floor. Near-end
//     speech cannot be cancelled, so it appears at once as a jump in the
//     residual. This works however loud the echo path is, which is exactly
//     where the Geigel test fails.
func (c *Canceller) detectDoubleTalk(farActive bool, frameMicEnergy, frameRefEnergy float64) {
	if !farActive {
		// With no far end there is no echo to confuse the filter, and nothing
		// to suppress. Whatever the microphone hears is the near end.
		c.doubleTalk = false
		c.doubleTalkHold = 0
		return
	}

	near := false
	if c.Converged() && c.micEnergy > 0 {
		ratio := c.outEnergy / c.micEnergy
		near = ratio > c.echoFloor*dtdResidualRise
	} else if frameRefEnergy > refActiveEnergy {
		// This frame carries real reference audio, so the microphone-to-
		// reference ratio means something. Compare it against what this path
		// has been delivering rather than against the reference itself.
		ratio := frameMicEnergy / frameRefEnergy
		if math.IsInf(c.echoGain, 1) {
			c.echoGain = ratio // first echo seen: nothing to compare against yet
		}
		near = ratio > c.echoGain*geigelRise
		if !near {
			c.trackEchoGain(ratio)
		}
	}

	switch {
	case near:
		c.doubleTalk = true
		c.doubleTalkHold = doubleTalkHoldFrames
	case c.doubleTalkHold > 0:
		c.doubleTalkHold--
		c.doubleTalk = true
	default:
		c.doubleTalk = false
	}
}

// trackEchoGain updates the measured echo path gain from a frame believed to
// carry echo only. Falling fast and rising slowly means a quiet frame settles
// the baseline at once, while near-end speech -- which can only push the ratio
// up -- cannot drag it along and blind the detector.
func (c *Canceller) trackEchoGain(ratio float64) {
	if ratio <= 0 {
		return
	}
	rate := gainRise
	if ratio < c.echoGain {
		rate = gainFall
	}
	c.echoGain += rate * (ratio - c.echoGain)
}

// updateEchoFloor records how well the filter cancels when only echo is
// present, which is the reference the double-talk detector compares against.
func (c *Canceller) updateEchoFloor(farActive bool) {
	if !farActive || c.doubleTalk || c.micEnergy <= 0 {
		return
	}
	ratio := c.outEnergy / c.micEnergy
	if ratio < c.echoFloor {
		c.echoFloor = ratio
		return
	}
	// Drift upwards slowly, so a genuine change in the room does not look
	// like permanent double-talk.
	c.echoFloor *= floorDecay
	if c.echoFloor > 1 {
		c.echoFloor = 1
	}
}

// applyResidualSuppression attenuates whatever echo the linear filter left
// behind.
//
// It runs only when the far end is talking, the near end is not, and the
// filter has converged enough to be believed. Those conditions are what keep
// it from ever quietening the near-end talker, which would make the call sound
// half-duplex.
func (c *Canceller) applyResidualSuppression(out []float32, farActive bool) {
	target := float32(1.0)

	if farActive && !c.doubleTalk && c.Converged() {
		// How much of the microphone signal survived cancellation. A small
		// ratio means the filter is working and the remainder is residual
		// echo, so it can be pushed down hard.
		ratio := 1.0
		if c.micEnergy > 0 {
			ratio = c.outEnergy / c.micEnergy
		}
		target = float32(math.Sqrt(clamp(ratio, 0, 1)))
		if target < maxSuppression {
			target = maxSuppression
		}
	}

	// Move towards the target rather than jumping, so the gain does not
	// modulate audibly between frames.
	c.suppressionGain += 0.25 * (target - c.suppressionGain)
	if c.suppressionGain > 1 {
		c.suppressionGain = 1
	}

	if c.suppressionGain < 0.999 {
		for i := range out {
			out[i] *= c.suppressionGain
		}
	}
}

// Converged reports whether the filter has adapted enough to be trusted.
func (c *Canceller) Converged() bool { return c.adaptedFrames >= convergedAfterFrames }

// publishStats snapshots performance for readers on other goroutines. Called
// at the end of each frame, from the audio thread.
func (c *Canceller) publishStats() {
	erle := 0.0
	if c.outEnergy > 0 && c.micEnergy > 0 {
		erle = 10 * math.Log10(c.micEnergy/c.outEnergy)
	}
	suppression := 0.0
	if c.suppressionGain > 0 {
		suppression = 20 * math.Log10(float64(c.suppressionGain))
	}

	var flags uint32
	if c.Converged() {
		flags |= flagConverged
	}
	if c.doubleTalk {
		flags |= flagDoubleTalk
	}

	c.statERLE.Store(math.Float64bits(erle))
	c.statSuppression.Store(math.Float64bits(suppression))
	c.statFrames.Store(c.frames)
	c.statFlags.Store(flags)
}

// Stats reports current performance. Safe from any goroutine.
func (c *Canceller) Stats() Stats {
	flags := c.statFlags.Load()
	return Stats{
		ERLE:            math.Float64frombits(c.statERLE.Load()),
		SuppressionDB:   math.Float64frombits(c.statSuppression.Load()),
		FramesProcessed: c.statFrames.Load(),
		Converged:       flags&flagConverged != 0,
		DoubleTalk:      flags&flagDoubleTalk != 0,
	}
}

// --- helpers ---

func decodePCM(dst []float32, src []byte) {
	for i := range dst {
		dst[i] = float32(int16(binary.LittleEndian.Uint16(src[i*2:]))) / 32768
	}
}

func encodePCM(dst []byte, src []float32) {
	for i, v := range src {
		s := v * 32768
		switch {
		case s > 32767:
			s = 32767
		case s < -32768:
			s = -32768
		}
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(s)))
	}
}

func energy(x []float32) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, v := range x {
		sum += float64(v) * float64(v)
	}
	return sum / float64(len(x))
}

func smooth(prev, now float64) float64 {
	return energySmoothing*prev + (1-energySmoothing)*now
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sameSlice(a, b []byte) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}
