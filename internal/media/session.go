package media

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

// AudioLeg is the media session's view of the audio device. The audio package
// supplies the implementation; declaring it here keeps media free of any
// dependency on the device layer.
type AudioLeg interface {
	// ReadTX fills pcm with microphone audio. It returns false when no audio is
	// available, in which case the session transmits silence.
	ReadTX(pcm []byte) bool
	// WriteRX hands decoded audio to the speaker mixer.
	WriteRX(pcm []byte)
}

// mediaTimeout is how long inbound RTP may be absent before we warn (FR-8.8).
const mediaTimeout = 10 * time.Second

// Stats is a snapshot of one session's counters (FR-8.9).
type Stats struct {
	Codec string

	PacketsSent uint64
	PacketsRecv uint64
	BytesSent   uint64
	BytesRecv   uint64

	Lost      uint64 // sequence gaps observed on arrival
	Late      uint64 // discarded by the jitter buffer
	Concealed uint64 // frames synthesised by PLC
	Overflow  uint64

	JitterMS float64

	RemoteAddr string
	LocalPort  int
}

// SessionConfig configures a media session.
type SessionConfig struct {
	LocalPort      int
	Codec          Codec
	DTMFPayload    uint8
	PtimeMS        int
	JitterBufferMS int
	InputGain      float64
	OutputGain     float64
	RTCPEnabled    bool

	Leg    AudioLeg
	Logger *slog.Logger

	// OnMediaTimeout fires once when inbound RTP has been absent for
	// mediaTimeout while the session expects to receive (FR-8.8).
	OnMediaTimeout func()
	// OnDTMF reports an inbound telephone-event digit.
	OnDTMF func(digit string)
}

// Session is one channel's RTP/RTCP endpoint.
type Session struct {
	cfg SessionConfig

	conn     *net.UDPConn
	rtcpConn *net.UDPConn

	remoteMu sync.RWMutex
	remote   *net.UDPAddr

	jb *JitterBuffer

	// RTP transmit state.
	seq  uint16
	ts   uint32
	ssrc uint32

	localPort int

	sendEnabled atomic.Bool
	recvEnabled atomic.Bool
	muted       atomic.Bool
	ringback    atomic.Bool
	tone        *ToneGen

	dtmfMu  sync.Mutex
	dtmf    *dtmfSender
	dtmfSeq atomic.Bool // true while a digit string is in flight

	// Counters.
	packetsSent, packetsRecv atomic.Uint64
	bytesSent, bytesRecv     atomic.Uint64
	lost                     atomic.Uint64
	lastInbound              atomic.Int64  // unix nanos
	jitterEst                atomic.Uint64 // milliseconds, fixed point x1000

	lastRecvSeq uint16
	haveRecvSeq bool
	lastTransit int64
	haveTransit bool

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// NewSession binds the RTP (and optionally RTCP) sockets.
func NewSession(cfg SessionConfig) (*Session, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.LocalPort})
	if err != nil {
		return nil, fmt.Errorf("bind RTP port %d: %w", cfg.LocalPort, err)
	}
	// With cfg.LocalPort of 0 the OS assigned the port, so read back what we
	// actually got: it is what goes in SDP.
	localPort := conn.LocalAddr().(*net.UDPAddr).Port

	s := &Session{
		cfg:       cfg,
		conn:      conn,
		localPort: localPort,
		jb:        NewJitterBuffer(cfg.JitterBufferMS, cfg.PtimeMS),
		seq:       uint16(rand.Uint32()), // random initial seq (FR-8.3)
		ts:        rand.Uint32(),
		ssrc:      rand.Uint32(),
		tone:      NewRingback(cfg.PtimeMS),
		done:      make(chan struct{}),
	}

	if cfg.RTCPEnabled {
		// RTCP conventionally sits at RTP port + 1. It may be taken, which is
		// not fatal: nothing in v1 sends or reads RTCP yet (see requirements
		// §12), so the socket is reserved only to keep the convention.
		if rc, err := net.ListenUDP("udp", &net.UDPAddr{Port: localPort + 1}); err == nil {
			s.rtcpConn = rc
		} else {
			cfg.Logger.Debug("RTCP port unavailable, continuing without it",
				"port", localPort+1, "error", err)
		}
	}
	return s, nil
}

// LocalPort is the RTP port actually bound.
func (s *Session) LocalPort() int { return s.localPort }

// SetRemote points the session at the peer's RTP address from SDP.
func (s *Session) SetRemote(addr *net.UDPAddr) {
	s.remoteMu.Lock()
	s.remote = addr
	s.remoteMu.Unlock()
}

func (s *Session) remoteAddr() *net.UDPAddr {
	s.remoteMu.RLock()
	defer s.remoteMu.RUnlock()
	return s.remote
}

// SetDirection enables transmit and receive independently, which is how hold is
// applied to media (FR-4.7, FR-8.7).
func (s *Session) SetDirection(send, recv bool) {
	s.sendEnabled.Store(send)
	s.recvEnabled.Store(recv)
}

// SetMuted stops microphone transmission without changing SIP state.
func (s *Session) SetMuted(m bool) { s.muted.Store(m) }

// Muted reports the microphone state.
func (s *Session) Muted() bool { return s.muted.Load() }

// SetRingback plays or stops the local ringback tone (FR-8.10).
func (s *Session) SetRingback(on bool) {
	if on {
		s.tone.Reset()
	}
	s.ringback.Store(on)
}

// QueueDTMF schedules a digit string for transmission (FR-4.10).
func (s *Session) QueueDTMF(digits string) error {
	sender, err := newDTMFSender(digits, s.cfg.PtimeMS)
	if err != nil {
		return err
	}
	s.dtmfMu.Lock()
	defer s.dtmfMu.Unlock()
	if s.dtmf != nil && !s.dtmf.done() {
		return fmt.Errorf("DTMF already in progress")
	}
	s.dtmf = sender
	s.dtmfSeq.Store(true)
	return nil
}

// Start launches the transmit, receive and playout loops.
func (s *Session) Start(ctx context.Context) {
	s.lastInbound.Store(time.Now().UnixNano())

	s.wg.Add(3)
	go s.txLoop(ctx)
	go s.rxLoop(ctx)
	go s.playoutLoop(ctx)

	if s.cfg.OnMediaTimeout != nil {
		s.wg.Add(1)
		go s.watchdog(ctx)
	}
}

// txLoop paces outbound packets at the negotiated ptime (FR-8.3).
func (s *Session) txLoop(ctx context.Context) {
	defer s.wg.Done()

	interval := time.Duration(s.cfg.PtimeMS) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	samples := uint32(FrameSamples(s.cfg.PtimeMS))
	pcm := make([]byte, FrameBytes(s.cfg.PtimeMS))
	silence := make([]byte, FrameBytes(s.cfg.PtimeMS))
	markerPending := true

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}

		remote := s.remoteAddr()
		if remote == nil {
			s.ts += samples
			continue
		}

		// A DTMF event in flight takes the slot instead of audio. Every packet of
		// one event carries the event's start timestamp, while the session clock
		// keeps advancing so audio resumes coherently afterwards.
		if payload, eventTS, active := s.nextDTMF(); active {
			if payload != nil {
				s.send(remote, s.cfg.DTMFPayload, payload, eventTS, false)
				s.seq++
			}
			s.ts += samples
			continue
		}

		if !s.sendEnabled.Load() {
			// On hold we stop sending entirely; the timestamp clock keeps running
			// so the peer sees a coherent stream when we come back.
			s.ts += samples
			markerPending = true
			continue
		}

		frame := silence
		if !s.muted.Load() && s.cfg.Leg != nil && s.cfg.Leg.ReadTX(pcm) {
			applyGain(pcm, s.cfg.InputGain)
			frame = pcm
		}

		s.send(remote, s.cfg.Codec.PayloadType, s.cfg.Codec.Encode(frame), s.ts, markerPending)
		markerPending = false
		s.seq++
		s.ts += samples
	}
}

// nextDTMF advances the in-flight DTMF sender, if any.
func (s *Session) nextDTMF() (payload []byte, ts uint32, active bool) {
	if !s.dtmfSeq.Load() {
		return nil, 0, false
	}
	s.dtmfMu.Lock()
	defer s.dtmfMu.Unlock()
	if s.dtmf == nil {
		s.dtmfSeq.Store(false)
		return nil, 0, false
	}
	payload, ts, _, ok := s.dtmf.next(s.ts)
	if !ok {
		s.dtmf = nil
		s.dtmfSeq.Store(false)
		return nil, 0, false
	}
	return payload, ts, true
}

// send marshals and writes one RTP packet.
func (s *Session) send(remote *net.UDPAddr, pt uint8, payload []byte, ts uint32, marker bool) {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    pt,
			SequenceNumber: s.seq,
			Timestamp:      ts,
			SSRC:           s.ssrc,
		},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		s.cfg.Logger.Warn("RTP marshal failed", "error", err)
		return
	}
	if _, err := s.conn.WriteToUDP(raw, remote); err != nil {
		s.cfg.Logger.Debug("RTP send failed", "error", err)
		return
	}
	s.packetsSent.Add(1)
	s.bytesSent.Add(uint64(len(raw)))
}

// rxLoop reads inbound RTP, applies symmetric-RTP learning and fills the
// jitter buffer (FR-8.4, FR-8.5).
func (s *Session) rxLoop(ctx context.Context) {
	defer s.wg.Done()

	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		default:
		}

		_ = s.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return // socket closed
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			// Malformed RTP is logged and dropped, never fatal (NFR-9).
			s.cfg.Logger.Debug("malformed RTP dropped", "bytes", n, "error", err)
			continue
		}

		s.learnRemote(src)
		s.packetsRecv.Add(1)
		s.bytesRecv.Add(uint64(n))
		s.lastInbound.Store(time.Now().UnixNano())
		s.trackArrival(pkt.SequenceNumber, pkt.Timestamp)

		if pkt.PayloadType == s.cfg.DTMFPayload {
			s.handleInboundDTMF(pkt.Payload)
			continue
		}

		codec, ok := CodecByPayloadType(pkt.PayloadType)
		if !ok || codec.PayloadType != s.cfg.Codec.PayloadType {
			continue // not the negotiated codec; ignore
		}
		s.jb.Push(pkt.SequenceNumber, codec.Decode(pkt.Payload))
	}
}

// learnRemote implements symmetric RTP: once audio arrives, send back to where
// it came from rather than trusting only the SDP address (FR-8.5).
func (s *Session) learnRemote(src *net.UDPAddr) {
	cur := s.remoteAddr()
	if cur != nil && cur.IP.Equal(src.IP) && cur.Port == src.Port {
		return
	}
	s.remoteMu.Lock()
	s.remote = src
	s.remoteMu.Unlock()
	s.cfg.Logger.Debug("symmetric RTP: switched remote", "addr", src.String())
}

// trackArrival maintains the loss count and an RFC 3550 style jitter estimate.
func (s *Session) trackArrival(seq uint16, ts uint32) {
	if s.haveRecvSeq {
		if gap := int16(seq - s.lastRecvSeq); gap > 1 {
			s.lost.Add(uint64(gap - 1))
		}
	}
	if !s.haveRecvSeq || seqLess(s.lastRecvSeq, seq) {
		s.lastRecvSeq, s.haveRecvSeq = seq, true
	}

	arrival := time.Now().UnixNano() / int64(time.Millisecond) * int64(SampleRate) / 1000
	transit := arrival - int64(ts)
	if s.haveTransit {
		d := transit - s.lastTransit
		if d < 0 {
			d = -d
		}
		prev := float64(s.jitterEst.Load()) / 1000
		j := prev + (float64(d)-prev)/16
		s.jitterEst.Store(uint64(j * 1000))
	}
	s.lastTransit, s.haveTransit = transit, true
}

// handleInboundDTMF reports the start of an inbound telephone-event.
func (s *Session) handleInboundDTMF(payload []byte) {
	if len(payload) < 4 || s.cfg.OnDTMF == nil {
		return
	}
	// Report only the end packet so a digit is announced once.
	if payload[1]&0x80 == 0 {
		return
	}
	digits := "0123456789*#ABCD"
	if int(payload[0]) < len(digits) {
		s.cfg.OnDTMF(string(digits[payload[0]]))
	}
}

// playoutLoop drives the speaker at a steady cadence from the jitter buffer,
// or from the ringback generator before the call connects.
func (s *Session) playoutLoop(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(time.Duration(s.cfg.PtimeMS) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}

		if s.cfg.Leg == nil {
			continue
		}
		if s.ringback.Load() {
			s.cfg.Leg.WriteRX(s.tone.Frame(s.cfg.PtimeMS))
			continue
		}
		if !s.recvEnabled.Load() {
			continue // held: inbound audio is dropped, not mixed (FR-8.7)
		}
		if pcm, ok := s.jb.Pop(); ok {
			applyGain(pcm, s.cfg.OutputGain)
			s.cfg.Leg.WriteRX(pcm)
		}
	}
}

// watchdog warns once if inbound media dries up (FR-8.8).
func (s *Session) watchdog(ctx context.Context) {
	defer s.wg.Done()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	warned := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}

		if !s.recvEnabled.Load() || s.remoteAddr() == nil {
			warned = false
			continue
		}
		idle := time.Since(time.Unix(0, s.lastInbound.Load()))
		switch {
		case idle > mediaTimeout && !warned:
			warned = true
			s.cfg.OnMediaTimeout()
		case idle <= mediaTimeout:
			warned = false
		}
	}
}

// Stats snapshots the session counters.
func (s *Session) Stats() Stats {
	_, late, concealed, overflow := s.jb.Counters()
	st := Stats{
		Codec:       s.cfg.Codec.Name,
		PacketsSent: s.packetsSent.Load(),
		PacketsRecv: s.packetsRecv.Load(),
		BytesSent:   s.bytesSent.Load(),
		BytesRecv:   s.bytesRecv.Load(),
		Lost:        s.lost.Load(),
		Late:        late,
		Concealed:   concealed,
		Overflow:    overflow,
		JitterMS:    float64(s.jitterEst.Load()) / 1000 / (SampleRate / 1000),
		LocalPort:   s.localPort,
	}
	if r := s.remoteAddr(); r != nil {
		st.RemoteAddr = r.String()
	}
	return st
}

// Close stops the loops and releases the sockets. It is safe to call twice.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
		if s.rtcpConn != nil {
			_ = s.rtcpConn.Close()
		}
		s.wg.Wait()
	})
}
