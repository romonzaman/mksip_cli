// Package audio owns the capture/playback device and routes audio between the
// microphone, the speakers and the per-channel media sessions (requirements §8
// FR-8.6, FR-8.7).
package audio

import (
	"encoding/binary"

	"sipclient/internal/aec"
	"sync"
	"sync/atomic"
)

// ring is a fixed-capacity byte ring that drops the oldest data on overflow.
// Dropping old audio is the right failure mode here: stale frames are useless
// and unbounded growth would show up as ever-growing latency.
type ring struct {
	mu   sync.Mutex
	buf  []byte
	r, w int
	size int
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]byte, capacity+1)}
}

func (rb *ring) Write(p []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for _, b := range p {
		rb.buf[rb.w] = b
		rb.w = (rb.w + 1) % len(rb.buf)
		if rb.size == len(rb.buf)-1 {
			rb.r = (rb.r + 1) % len(rb.buf) // overwrite oldest
		} else {
			rb.size++
		}
	}
}

// Read fills p only if enough data is buffered, so callers never get a
// partially filled audio frame.
func (rb *ring) Read(p []byte) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.size < len(p) {
		return false
	}
	for i := range p {
		p[i] = rb.buf[rb.r]
		rb.r = (rb.r + 1) % len(rb.buf)
	}
	rb.size -= len(p)
	return true
}

func (rb *ring) Reset() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.r, rb.w, rb.size = 0, 0, 0
}

// Leg is one channel's attachment to the audio device. It satisfies
// media.AudioLeg.
type Leg struct {
	ID int

	tx *ring // microphone -> network
	rx *ring // network -> speaker

	send atomic.Bool
	recv atomic.Bool
}

// ReadTX fills pcm with captured microphone audio.
func (l *Leg) ReadTX(pcm []byte) bool {
	if !l.send.Load() {
		return false
	}
	return l.tx.Read(pcm)
}

// WriteRX queues decoded audio for the speaker mixer.
func (l *Leg) WriteRX(pcm []byte) {
	if !l.recv.Load() {
		return
	}
	l.rx.Write(pcm)
}

// SetAttached connects or disconnects this leg from the device. A held or
// inactive channel is detached, which is what keeps audio on the active
// channel only (FR-3.4, FR-8.7).
func (l *Leg) SetAttached(send, recv bool) {
	if !send && l.send.Load() {
		l.tx.Reset()
	}
	if !recv && l.recv.Load() {
		l.rx.Reset()
	}
	l.send.Store(send)
	l.recv.Store(recv)
}

// Attached reports the current routing.
func (l *Leg) Attached() (send, recv bool) { return l.send.Load(), l.recv.Load() }

// Router mixes all attached legs into one device stream.
type Router struct {
	mu   sync.RWMutex
	legs []*Leg

	muted atomic.Bool

	frameBytes   int
	bufferFrames int
	scratch      []byte // reused inside the playback callback only

	// echo removes the far end's voice from the microphone when the device is
	// running in duplex. Nil when echo cancellation is off or unavailable.
	echo *aec.Canceller
	// echoScratch holds the cancelled microphone frame; reused so the audio
	// callback never allocates.
	echoScratch []byte
}

// NewRouter builds a router whose per-leg buffers hold bufferFrames frames.
// bufferFrames bounds added latency: at 20 ms frames, 8 frames is 160 ms.
func NewRouter(frameBytes, bufferFrames int) *Router {
	if bufferFrames < 2 {
		bufferFrames = 2
	}
	return &Router{
		frameBytes:   frameBytes,
		bufferFrames: bufferFrames,
		scratch:      make([]byte, frameBytes*bufferFrames),
	}
}

// AddLeg registers a channel with the router.
func (r *Router) AddLeg(id int) *Leg {
	l := &Leg{
		ID: id,
		tx: newRing(r.frameBytes * r.bufferFrames),
		rx: newRing(r.frameBytes * r.bufferFrames),
	}
	r.mu.Lock()
	r.legs = append(r.legs, l)
	r.mu.Unlock()
	return l
}

// SetEchoCanceller installs the echo canceller. It must be called before the
// device starts, and only when the device runs in duplex: without a reference
// frame aligned to the capture there is nothing to cancel against.
func (r *Router) SetEchoCanceller(c *aec.Canceller) {
	r.mu.Lock()
	r.echo = c
	r.echoScratch = make([]byte, r.frameBytes*r.bufferFrames)
	r.mu.Unlock()
}

// EchoStats reports echo cancellation performance, and false when it is off.
func (r *Router) EchoStats() (aec.Stats, bool) {
	r.mu.RLock()
	c := r.echo
	r.mu.RUnlock()
	if c == nil {
		return aec.Stats{}, false
	}
	return c.Stats(), true
}

// ProcessDuplex is the duplex device callback: it fills the speaker buffer and
// then removes that same audio from the microphone frame.
//
// The speaker buffer is filled first so it can serve as the echo reference.
// The frame the microphone just captured cannot contain audio that has not
// been played yet, so the filter simply learns a delay of at least one frame --
// it is given the history and works out the real path itself.
func (r *Router) ProcessDuplex(out, in []byte) {
	r.ProcessPlayback(out)

	r.mu.RLock()
	echo := r.echo
	scratch := r.echoScratch
	r.mu.RUnlock()

	if echo == nil || len(in) == 0 || len(in) != len(out) || len(scratch) < len(in) {
		r.ProcessCapture(in)
		return
	}

	cleaned := scratch[:len(in)]
	echo.Process(cleaned, in, out)
	r.ProcessCapture(cleaned)
}

// SetMuted mutes the microphone globally (FR-9.3 mute).
func (r *Router) SetMuted(m bool) { r.muted.Store(m) }

// Muted reports the microphone state.
func (r *Router) Muted() bool { return r.muted.Load() }

// ProcessCapture is the capture callback: it distributes microphone audio to
// every attached leg. It runs on the audio thread, so it must not block.
func (r *Router) ProcessCapture(in []byte) {
	if len(in) == 0 || r.muted.Load() {
		return
	}

	r.mu.RLock()
	legs := r.legs
	r.mu.RUnlock()

	for _, l := range legs {
		if l.send.Load() {
			l.tx.Write(in)
		}
	}
}

// ProcessPlayback is the playback callback: it mixes every attached leg into
// the speaker buffer. It runs on the audio thread, so it must not block.
func (r *Router) ProcessPlayback(out []byte) {
	for i := range out {
		out[i] = 0
	}
	if len(out) == 0 {
		return
	}

	r.mu.RLock()
	legs := r.legs
	r.mu.RUnlock()

	if len(out) > len(r.scratch) {
		r.scratch = make([]byte, len(out))
	}
	buf := r.scratch[:len(out)]

	for _, l := range legs {
		if !l.recv.Load() {
			continue
		}
		if !l.rx.Read(buf) {
			continue // underrun on this leg: contribute silence
		}
		mixInto(out, buf)
	}
}

// mixInto sums src into dst as 16-bit PCM with clipping protection, which is
// what makes a 3-way consult possible (FR-7.4).
func mixInto(dst, src []byte) {
	for i := 0; i+1 < len(dst) && i+1 < len(src); i += 2 {
		a := int32(int16(binary.LittleEndian.Uint16(dst[i:])))
		b := int32(int16(binary.LittleEndian.Uint16(src[i:])))
		binary.LittleEndian.PutUint16(dst[i:], uint16(clip16(a+b)))
	}
}

func clip16(v int32) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	}
	return int16(v)
}
