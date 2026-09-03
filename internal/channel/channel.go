package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"sipclient/internal/audio"
	"sipclient/internal/config"
	"sipclient/internal/media"
	"sipclient/internal/sdputil"
	"sipclient/internal/sipua"
)

// Channel is one of the two call slots. It is a stable object: it survives its
// calls and returns to Idle rather than being destroyed (FR-3.1).
//
// State is guarded by mu. Every exported method takes the lock, so CLI
// commands, SIP callbacks and media callbacks cannot interleave on a channel's
// state. Long-running SIP work (waiting for an answer) runs outside the lock
// and re-acquires it to publish the result.
type Channel struct {
	ID int

	cfg  config.Config
	ua   *sipua.UA
	pool *media.PortPool
	leg  *audio.Leg
	log  *slog.Logger

	emit func(Event)
	// onStateChange lets the manager re-evaluate audio routing and refresh the
	// status line whenever anything moves.
	onStateChange func()

	mu    sync.RWMutex
	state State

	dialog sipua.Dialog
	info   sipua.DialogInfo
	// serverDlg is set for inbound calls, so we can answer or decline.
	serverDlg *sipgo.DialogServerSession
	clientDlg *sipgo.DialogClientSession

	session *media.Session
	rtpPort int
	codec   media.Codec

	remote      string
	inbound     bool
	startedAt   time.Time
	connectedAt time.Time

	peerDir    sdputil.Direction
	localHeld  bool
	remoteHeld bool
	active     bool

	sessionVer uint64
	// hangupRequested covers the CANCEL/200 OK race: if the answer wins, the
	// waiting goroutine sends BYE instead of leaving a stray call up (FR-4.3).
	hangupRequested bool
	cancelDial      context.CancelFunc

	// decided is closed once a ringing inbound call has been answered,
	// declined or cancelled. sipgo terminates the INVITE server transaction as
	// soon as its handler returns, so the handler must block on this until the
	// operator has decided -- otherwise the 200 OK has no transaction to go
	// out on.
	decided chan struct{}
}

// New builds an idle channel.
func New(id int, cfg config.Config, ua *sipua.UA, pool *media.PortPool,
	leg *audio.Leg, logger *slog.Logger, emit func(Event), onStateChange func()) *Channel {
	return &Channel{
		ID: id, cfg: cfg, ua: ua, pool: pool, leg: leg,
		log:  logger.With("channel", id),
		emit: emit, onStateChange: onStateChange,
		state: Idle,
	}
}

// State returns the current state.
func (c *Channel) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// Snapshot reads the channel for display.
func (c *Channel) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := Snapshot{
		ID: c.ID, State: c.state, Remote: c.remote, Inbound: c.inbound,
		Active: c.active, LocalHeld: c.localHeld, RemoteHeld: c.remoteHeld,
		RTPPort: c.rtpPort,
	}
	if c.codec.Name != "" {
		s.Codec = c.codec.Name
	}
	if c.session != nil {
		s.Muted = c.session.Muted()
	}
	switch {
	case !c.connectedAt.IsZero():
		s.Duration = time.Since(c.connectedAt)
	case !c.startedAt.IsZero():
		s.Duration = time.Since(c.startedAt)
	}
	return s
}

// Stats returns media statistics, or false when there is no session.
func (c *Channel) Stats() (media.Stats, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session == nil {
		return media.Stats{}, false
	}
	return c.session.Stats(), true
}

// DialogInfo exposes the dialog identity, which the transfer logic needs to
// build a Replaces header.
func (c *Channel) DialogInfo() (sipua.DialogInfo, sipua.Dialog, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.dialog == nil || !c.state.InCall() {
		return sipua.DialogInfo{}, nil, false
	}
	return c.info, c.dialog, true
}

// signalDecidedLocked releases the inbound INVITE handler. Safe to call more
// than once. Caller must hold mu.
func (c *Channel) signalDecidedLocked() {
	if c.decided == nil {
		return
	}
	select {
	case <-c.decided:
	default:
		close(c.decided)
	}
}

// setState publishes a new state and notifies the manager.
func (c *Channel) setState(s State) {
	if c.state == s {
		return
	}
	prev := c.state
	c.state = s
	c.log.Info("state", "from", prev.String(), "to", s.String())
	go func() {
		if c.onStateChange != nil {
			c.onStateChange()
		}
	}()
}

// codecList resolves the configured codec preference order.
func (c *Channel) codecList() ([]media.Codec, error) {
	out := make([]media.Codec, 0, len(c.cfg.Media.Codecs))
	for _, name := range c.cfg.Media.Codecs {
		codec, err := media.CodecByName(name)
		if err != nil {
			return nil, err
		}
		out = append(out, codec)
	}
	return out, nil
}

// startMedia allocates an RTP port and starts a media session.
//
// It tries successive ports rather than failing on the first one that will not
// bind: ranges commonly overlap something else on the host, and a call should
// not die because the first pair in the range is taken.
func (c *Channel) startMedia(ctx context.Context, codec media.Codec, dtmfPT uint8) error {
	const maxAttempts = 8

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		port, err := c.pool.Acquire()
		if err != nil {
			if lastErr != nil {
				return fmt.Errorf("%w (after %d unusable port(s))", err, attempt)
			}
			return err
		}

		sess, err := c.newSession(ctx, port, codec, dtmfPT)
		if err == nil {
			c.session = sess
			// With an OS-assigned port the requested value was 0, so take the
			// port that was actually bound -- it is what goes in SDP.
			c.rtpPort = sess.LocalPort()
			c.codec = codec
			sess.Start(ctx)
			return nil
		}

		// This port cannot be bound; take it out of rotation and try the next.
		c.pool.MarkUnusable(port)
		c.log.Warn("RTP port unavailable, trying the next one",
			"port", port, "error", err)
		lastErr = err
	}
	return fmt.Errorf("could not bind an RTP port after %d attempts: %w",
		maxAttempts, lastErr)
}

// newSession builds a media session on a specific port.
func (c *Channel) newSession(ctx context.Context, port int, codec media.Codec,
	dtmfPT uint8) (*media.Session, error) {

	return media.NewSession(media.SessionConfig{
		LocalPort:      port,
		Codec:          codec,
		DTMFPayload:    dtmfPT,
		PtimeMS:        c.cfg.Media.PtimeMS,
		JitterBufferMS: c.cfg.Media.JitterBufferMS,
		InputGain:      c.cfg.Audio.InputGain,
		OutputGain:     c.cfg.Audio.OutputGain,
		RTCPEnabled:    c.cfg.Media.RTCPEnabled,
		Leg:            c.leg,
		Logger:         c.log,
		OnMediaTimeout: func() {
			c.emit(Event{Kind: EventWarning, Channel: c.ID,
				Text: "no inbound RTP for 10s (one-way audio?)"})
		},
		OnDTMF: func(digit string) {
			c.emit(Event{Kind: EventDTMF, Channel: c.ID, Text: "received " + digit})
		},
	})
}

// stopMedia tears the session down and returns the port to the pool.
func (c *Channel) stopMedia() {
	if c.session != nil {
		if st := c.session.Stats(); st.PacketsSent > 0 || st.PacketsRecv > 0 {
			// Log media statistics at teardown (FR-8.9).
			c.log.Info("media closed",
				"codec", st.Codec, "sent", st.PacketsSent, "recv", st.PacketsRecv,
				"lost", st.Lost, "late", st.Late, "concealed", st.Concealed)
		}
		c.session.Close()
		c.session = nil
	}
	if c.rtpPort != 0 {
		c.pool.Release(c.rtpPort)
		c.rtpPort = 0
	}
	if c.leg != nil {
		c.leg.SetAttached(false, false)
	}
}

// buildSDP renders our current offer or answer.
func (c *Channel) buildSDP(dir sdputil.Direction) ([]byte, error) {
	codecs := []media.Codec{c.codec}
	if c.codec.Name == "" {
		list, err := c.codecList()
		if err != nil {
			return nil, err
		}
		codecs = list
	}
	c.sessionVer++
	return sdputil.Build(sdputil.Offer{
		Address:     c.ua.MediaHost(),
		Port:        c.rtpPort,
		Codecs:      codecs,
		DTMFPayload: media.DTMFPayloadType,
		PtimeMS:     c.cfg.Media.PtimeMS,
		Direction:   dir,
		SessionVer:  c.sessionVer,
	})
}

// applyRemote points media at the peer described by an SDP body.
func (c *Channel) applyRemote(desc sdputil.Description) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", desc.Address, desc.Port))
	if err != nil {
		return fmt.Errorf("resolve remote media address: %w", err)
	}
	c.session.SetRemote(addr)
	c.peerDir = desc.Direction
	c.remoteHeld = desc.Direction == sdputil.SendOnly || desc.Direction == sdputil.Inactive
	return nil
}

// Dial places an outbound call (FR-4.1).
func (c *Channel) Dial(ctx context.Context, target sip.Uri, display string) error {
	c.mu.Lock()
	if c.state != Idle {
		st := c.state
		c.mu.Unlock()
		return &StateError{Channel: c.ID, State: st, Action: "dial",
			Detail: "hang up first"}
	}

	codecs, err := c.codecList()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := c.startMedia(ctx, codecs[0], media.DTMFPayloadType); err != nil {
		c.mu.Unlock()
		return err
	}
	// Offer the full configured list, not just the first codec.
	c.codec = media.Codec{}
	sdp, err := c.buildSDP(sdputil.SendRecv)
	c.codec = codecs[0]
	if err != nil {
		c.stopMedia()
		c.mu.Unlock()
		return err
	}

	c.inbound = false
	c.remote = display
	c.startedAt = time.Now()
	c.connectedAt = time.Time{}
	c.hangupRequested = false
	c.localHeld, c.remoteHeld = false, false
	c.setState(Calling)

	dialCtx, cancel := context.WithCancel(ctx)
	c.cancelDial = cancel
	c.mu.Unlock()

	from := &sip.FromHeader{
		DisplayName: c.cfg.SIP.DisplayName,
		Address:     c.ua.AORUri(),
		Params:      sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))

	// Build the INVITE by hand rather than using DialogUA.Invite, so the
	// request is sent to the proxy/registrar instead of being resolved from the
	// Request-URI host. Without this, a domain that is not also the SIP server
	// (or a configured outbound_proxy) would never be reached.
	invite := sip.NewRequest(sip.INVITE, target)
	invite.SetTransport(strings.ToUpper(c.cfg.SIP.Server.Transport))
	invite.SetDestination(c.ua.RouteDestination())
	invite.AppendHeader(from)
	invite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	invite.AppendHeader(sip.NewHeader("Allow",
		"INVITE, ACK, CANCEL, BYE, OPTIONS, INFO, NOTIFY, REFER"))
	invite.SetBody(sdp)

	dlg, err := c.ua.DialogUA().WriteInvite(dialCtx, invite)
	if err != nil {
		cancel()
		c.mu.Lock()
		c.stopMedia()
		c.setState(Idle)
		c.mu.Unlock()
		return fmt.Errorf("INVITE: %w", err)
	}

	// Route requests to the PBX rather than straight at the Request-URI host.
	go c.waitAnswer(dialCtx, cancel, dlg)
	return nil
}

// waitAnswer follows the outbound INVITE to its conclusion.
func (c *Channel) waitAnswer(ctx context.Context, cancel context.CancelFunc, dlg *sipgo.DialogClientSession) {
	defer cancel()

	ringing := false
	err := dlg.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: c.cfg.SIP.AuthUsername,
		Password: c.cfg.SIP.Password,
		OnResponse: func(res *sip.Response) error {
			switch res.StatusCode {
			case sip.StatusRinging:
				if !ringing {
					ringing = true
					c.onRinging()
				}
			case sip.StatusSessionInProgress:
				c.onEarlyMedia(res)
			}
			return nil
		},
	})

	if err != nil {
		c.onDialFailed(dlg, err)
		return
	}

	// 2xx: acknowledge, then bring media up.
	if err := dlg.Ack(ctx); err != nil {
		c.log.Error("ACK failed", "error", err)
	}
	c.onAnswered(dlg)
}

func (c *Channel) onRinging() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != Calling {
		return
	}
	c.setState(Ringing)
	if c.cfg.Audio.RingbackEnabled && c.session != nil {
		// Local ringback only when the PBX gave us no early media (FR-8.10).
		c.session.SetRingback(true)
	}
	c.applyMedia()
	c.emit(Event{Channel: c.ID, Text: "ringing"})
}

func (c *Channel) onEarlyMedia(res *sip.Response) {
	if len(res.Body()) == 0 {
		return
	}
	desc, err := sdputil.Parse(res.Body())
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return
	}
	c.session.SetRingback(false)
	if err := c.applyRemote(desc); err != nil {
		return
	}
	c.applyMedia() // listen to early media only
	c.emit(Event{Channel: c.ID, Text: "early media"})
}

func (c *Channel) onDialFailed(dlg *sipgo.DialogClientSession, err error) {
	c.mu.Lock()
	wasHangup := c.hangupRequested
	c.stopMedia()
	c.clientDlg = nil
	c.dialog = nil
	c.setState(Idle)
	c.remote = ""
	c.mu.Unlock()

	_ = dlg.Close()

	var resErr *sipgo.ErrDialogResponse
	switch {
	case errors.As(err, &resErr) && resErr.Res != nil:
		c.emit(Event{Kind: EventInfo, Channel: c.ID,
			Text: fmt.Sprintf("call failed: %d %s", resErr.Res.StatusCode, resErr.Res.Reason)})
	case wasHangup || errors.Is(err, context.Canceled):
		c.emit(Event{Channel: c.ID, Text: "call cancelled"})
	default:
		c.emit(Event{Kind: EventError, Channel: c.ID, Text: "call failed: " + err.Error()})
	}
}

func (c *Channel) onAnswered(dlg *sipgo.DialogClientSession) {
	info, infoErr := sipua.InfoFromUAC(dlg)

	c.mu.Lock()
	if c.hangupRequested {
		// The answer beat our CANCEL, so tear the call down properly (FR-4.3).
		c.mu.Unlock()
		c.log.Info("answer raced hangup, sending BYE")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dlg.Bye(ctx)
		_ = dlg.Close()
		c.mu.Lock()
		c.stopMedia()
		c.setState(Idle)
		c.remote = ""
		c.mu.Unlock()
		return
	}

	if infoErr != nil {
		c.mu.Unlock()
		c.emit(Event{Kind: EventError, Channel: c.ID,
			Text: "cannot track dialog: " + infoErr.Error()})
		return
	}

	c.dialog = dlg
	c.clientDlg = dlg
	c.info = info

	if c.session != nil {
		c.session.SetRingback(false)
	}

	res := dlg.InviteResponse
	if res == nil || len(res.Body()) == 0 {
		c.mu.Unlock()
		c.emit(Event{Kind: EventError, Channel: c.ID, Text: "answer carried no SDP"})
		c.Hangup(context.Background())
		return
	}

	desc, err := sdputil.Parse(res.Body())
	if err != nil {
		c.mu.Unlock()
		c.emit(Event{Kind: EventError, Channel: c.ID, Text: "bad answer SDP: " + err.Error()})
		c.Hangup(context.Background())
		return
	}

	codecs, _ := c.codecList()
	codec, err := sdputil.Negotiate(codecs, desc)
	if err != nil {
		c.mu.Unlock()
		c.emit(Event{Kind: EventError, Channel: c.ID, Text: "no common codec in answer"})
		c.Hangup(context.Background())
		return
	}
	c.codec = codec

	if err := c.applyRemote(desc); err != nil {
		c.mu.Unlock()
		c.emit(Event{Kind: EventError, Channel: c.ID, Text: err.Error()})
		return
	}

	c.connectedAt = time.Now()
	if u := c.info.RemoteURI; u.Host != "" {
		c.remote = uriShort(u)
	}
	c.setState(Connected)
	// Bring media up now that the codec and remote address are known. Without
	// this the session would stay muted in both directions.
	c.applyMedia()
	c.mu.Unlock()

	c.emit(Event{Channel: c.ID, Text: "connected, codec " + codec.Name})
	go c.watchDialog(dlg)
}

// watchDialog reacts to the dialog ending from the far side (FR-4.14 teardown).
func (c *Channel) watchDialog(dlg sipua.Dialog) {
	for state := range dlg.StateRead() {
		if state == sip.DialogStateEnded {
			break
		}
	}
	c.mu.Lock()
	if c.dialog != dlg {
		c.mu.Unlock()
		return // superseded
	}
	ended := c.state.InCall()
	if ended {
		c.stopMedia()
		c.dialog, c.clientDlg, c.serverDlg = nil, nil, nil
		c.setState(Idle)
		c.remote = ""
		c.localHeld, c.remoteHeld = false, false
		c.codec = media.Codec{}
		c.connectedAt = time.Time{}
	}
	c.mu.Unlock()
	if ended {
		c.emit(Event{Channel: c.ID, Text: "call ended by remote"})
	}
	_ = dlg.Close()
}

func uriShort(u sip.Uri) string {
	if u.User != "" {
		return u.User
	}
	return u.Host
}

// hostPortOf renders a target for logging.
func targetString(u sip.Uri) string { return (&u).String() }

// randomDelay is the jittered pause before retrying a 491 collision (FR-4.7).
func randomDelay() time.Duration {
	return time.Duration(500+rand.IntN(1500)) * time.Millisecond
}

// trimAngle removes surrounding angle brackets from a header value.
func trimAngle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	return strings.TrimSuffix(s, ">")
}
