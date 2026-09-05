package channel

import (
	"context"
	"fmt"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"sipclient/internal/history"
	"sipclient/internal/media"
	"sipclient/internal/sdputil"
	"sipclient/internal/sipua"
)

// Incoming takes an inbound call onto this channel and rings (FR-4.4).
func (c *Channel) Incoming(ctx context.Context, dlg *sipgo.DialogServerSession) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state != Idle {
		return &StateError{Channel: c.ID, State: c.state, Action: "accept an inbound call"}
	}

	req := dlg.InviteRequest
	c.serverDlg = dlg
	c.dialog = dlg
	c.inbound = true
	c.startedAt = time.Now()
	c.connectedAt = time.Time{}
	c.hangupRequested = false
	c.localHeld, c.remoteHeld = false, false

	c.remote = "unknown"
	if from := req.From(); from != nil {
		c.remote = uriShort(from.Address)
		if from.DisplayName != "" {
			c.remote = fmt.Sprintf("%s <%s>", from.DisplayName, uriShort(from.Address))
		}
		addr := from.Address
		c.remoteURI = (&addr).String()
	}

	c.decided = make(chan struct{})
	c.setState(Ringing)
	c.applyMedia()

	if err := dlg.Respond(sip.StatusRinging, "Ringing", nil); err != nil {
		c.log.Warn("failed to send 180 Ringing", "error", err)
	}
	return nil
}

// WaitInboundDecision blocks until the ringing call has been answered,
// declined or cancelled. The inbound INVITE handler must call it, because
// sipgo terminates the server transaction the moment that handler returns and
// the final response has to be sent while the transaction is still alive.
func (c *Channel) WaitInboundDecision(ctx context.Context) {
	c.mu.RLock()
	decided := c.decided
	dlg := c.serverDlg
	c.mu.RUnlock()

	if decided == nil {
		return
	}

	var dialogDone <-chan struct{}
	if dlg != nil {
		dialogDone = dlg.Context().Done()
	}

	select {
	case <-decided:
	case <-dialogDone:
	case <-ctx.Done():
	}
}

// Answer accepts a ringing inbound call (FR-4.5).
func (c *Channel) Answer(ctx context.Context) error {
	c.mu.Lock()

	if c.state != Ringing || c.serverDlg == nil {
		st := c.state
		c.mu.Unlock()
		return &StateError{Channel: c.ID, State: st, Action: "answer",
			Detail: "no ringing inbound call"}
	}

	dlg := c.serverDlg
	offer := dlg.InviteRequest.Body()
	if len(offer) == 0 {
		c.mu.Unlock()
		_ = dlg.Respond(sip.StatusNotAcceptableHere, "No SDP Offer", nil)
		c.reset("inbound call had no SDP offer")
		return fmt.Errorf("inbound INVITE carried no SDP offer")
	}

	desc, err := sdputil.Parse(offer)
	if err != nil {
		c.mu.Unlock()
		_ = dlg.Respond(sip.StatusNotAcceptableHere, "Bad SDP", nil)
		c.reset("inbound call had unparseable SDP")
		return fmt.Errorf("inbound SDP: %w", err)
	}

	// Session timer (RFC 4028). A peer asking for a shorter interval than we
	// accept must be told our minimum so it can retry, not silently accepted.
	stCfg := c.ua.SessionTimerConfig()
	sessionTimer, timerHeaders, tooSmall := stCfg.NegotiateIncoming(dlg.InviteRequest)
	if tooSmall {
		c.mu.Unlock()
		_ = dlg.WriteResponse(stCfg.TooSmallResponse(dlg.InviteRequest))
		c.reset("caller asked for too short a session interval")
		return fmt.Errorf("session interval below our minimum")
	}

	codecs, err := c.codecList()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	codec, err := sdputil.Negotiate(codecs, desc)
	if err != nil {
		// No overlap: 488 is the correct answer (FR-8.2).
		c.mu.Unlock()
		_ = dlg.Respond(sip.StatusNotAcceptableHere, "Not Acceptable Here", nil)
		c.reset("no common codec with caller")
		return fmt.Errorf("no common codec: caller offered payload types %v", desc.PayloadType)
	}

	if err := c.startMedia(ctx, codec, sdputil.DTMFPayloadFor(desc)); err != nil {
		c.mu.Unlock()
		_ = dlg.Respond(sip.StatusInternalServerError, "Media Setup Failed", nil)
		c.reset("media setup failed")
		return err
	}

	if err := c.applyRemote(desc); err != nil {
		c.mu.Unlock()
		_ = dlg.Respond(sip.StatusInternalServerError, "Bad Media Address", nil)
		c.reset("bad media address")
		return err
	}

	answer, err := c.buildSDP(sdputil.SendRecv)
	if err != nil {
		c.mu.Unlock()
		_ = dlg.Respond(sip.StatusInternalServerError, "SDP Build Failed", nil)
		c.reset("SDP build failed")
		return err
	}

	// Publish the dialog identity and move to Connected *before* sending the
	// 200 OK, and drop the lock first.
	//
	// sipgo's RespondSDP blocks until the ACK arrives, retransmitting the 200
	// meanwhile. That ACK is routed by matching the channel's dialog identity,
	// so if we published it afterwards -- or held the lock across the call --
	// the ACK could never be matched and the answer would hang for 64*T1.
	info, err := sipua.InfoFromUAS(dlg)
	if err != nil {
		c.mu.Unlock()
		_ = dlg.Respond(sip.StatusInternalServerError, "Dialog Error", nil)
		c.reset("cannot track dialog")
		return fmt.Errorf("cannot track dialog: %w", err)
	}
	c.info = info
	c.connectedAt = time.Now()
	c.startSessionTimer(sessionTimer)
	c.setState(Connected)
	c.applyMedia()
	c.mu.Unlock()

	if err := c.respondAnswer(dlg, answer, timerHeaders); err != nil {
		c.reset("failed to send 200 OK")
		return fmt.Errorf("send 200 OK: %w", err)
	}

	// The call is up and acknowledged, so the INVITE handler can let go.
	c.mu.Lock()
	c.signalDecidedLocked()
	c.mu.Unlock()

	c.emit(Event{Channel: c.ID, Text: "answered, codec " + codec.Name})
	go c.watchDialog(dlg)
	return nil
}

// Reject declines a ringing inbound call (FR-4.5).
func (c *Channel) Reject(ctx context.Context, code int, reason string) error {
	c.mu.Lock()
	if c.state != Ringing || c.serverDlg == nil {
		st := c.state
		c.mu.Unlock()
		return &StateError{Channel: c.ID, State: st, Action: "reject",
			Detail: "no ringing inbound call"}
	}
	dlg := c.serverDlg
	c.mu.Unlock()

	c.noteDisposition(history.Rejected, code, reason)
	if err := dlg.Respond(code, reason, nil); err != nil {
		return fmt.Errorf("send %d: %w", code, err)
	}
	c.reset(fmt.Sprintf("rejected with %d %s", code, reason))
	return nil
}

// Hangup ends whatever the channel is doing (FR-4.6).
func (c *Channel) Hangup(ctx context.Context) error {
	c.mu.Lock()
	state := c.state
	c.hangupRequested = true

	switch state {
	case Idle, Terminated:
		c.mu.Unlock()
		return &StateError{Channel: c.ID, State: state, Action: "hang up",
			Detail: "no call in progress"}

	case Calling, Ringing:
		if c.inbound {
			c.mu.Unlock()
			return c.Reject(ctx, sip.StatusGlobalDecline, "Decline")
		}
		// Outbound and not yet answered: cancelling the dial context makes
		// sipgo send CANCEL (FR-4.3).
		cancel := c.cancelDial
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil

	default:
		dlg := c.dialog
		c.mu.Unlock()
		if dlg == nil {
			c.reset("hung up")
			return nil
		}
		byeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := dlg.Bye(byeCtx); err != nil {
			c.log.Warn("BYE failed", "error", err)
		}
		c.reset("hung up")
		return nil
	}
}

// reset returns the channel to Idle and releases everything it held.
func (c *Channel) reset(why string) {
	c.mu.Lock()
	if c.state == Idle {
		c.mu.Unlock()
		return
	}
	c.signalDecidedLocked()

	// Record the finished call before the state that describes it is cleared.
	if rec, ok := c.finishRecord(); ok {
		defer c.recordCall(rec)
	}

	c.stopMedia()
	dlg := c.dialog
	c.dialog, c.clientDlg, c.serverDlg = nil, nil, nil
	c.remote = ""
	c.codec = media.Codec{}
	c.localHeld, c.remoteHeld = false, false
	c.connectedAt = time.Time{}
	c.startedAt = time.Time{}
	c.setState(Idle)
	c.mu.Unlock()

	if dlg != nil {
		_ = dlg.Close()
	}
	if why != "" {
		c.emit(Event{Channel: c.ID, Text: why})
	}
}

// Hold puts the peer on hold with a re-INVITE (FR-4.7).
func (c *Channel) Hold(ctx context.Context) error {
	return c.reoffer(ctx, true)
}

// Unhold retrieves a held call (FR-4.7).
func (c *Channel) Unhold(ctx context.Context) error {
	return c.reoffer(ctx, false)
}

// reoffer sends the hold or retrieve re-INVITE, retrying a 491 collision.
func (c *Channel) reoffer(ctx context.Context, hold bool) error {
	action := "unhold"
	if hold {
		action = "hold"
	}

	c.mu.Lock()
	switch {
	case !c.state.InCall():
		st := c.state
		c.mu.Unlock()
		return &StateError{Channel: c.ID, State: st, Action: action,
			Detail: "no established call"}
	case c.state == Transferring:
		c.mu.Unlock()
		return &StateError{Channel: c.ID, State: Transferring, Action: action,
			Detail: "a transfer is in progress"}
	case hold && c.localHeld:
		c.mu.Unlock()
		return fmt.Errorf("channel %d is already on hold", c.ID)
	case !hold && !c.localHeld:
		c.mu.Unlock()
		return fmt.Errorf("channel %d is not on hold", c.ID)
	}

	dir := sdputil.SendRecv
	if hold {
		dir = sdputil.HeldByUs(c.peerDir)
	}
	sdp, err := c.buildSDP(dir)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	dlg, target := c.dialog, c.info.RemoteTarget
	c.mu.Unlock()

	// One retry on 491 Request Pending: the peer had its own re-INVITE in
	// flight, so back off a random interval and try once more (FR-4.7).
	var res *sip.Response
	for attempt := 0; attempt < 2; attempt++ {
		res, err = c.ua.SendReInvite(ctx, dlg, target, sdp)
		if err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
		if res.StatusCode != sip.StatusRequestPending {
			break
		}
		delay := randomDelay()
		c.log.Info("491 Request Pending, retrying re-INVITE", "delay", delay.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s rejected: %d %s", action, res.StatusCode, res.Reason)
	}

	c.mu.Lock()
	c.localHeld = hold
	if len(res.Body()) > 0 {
		if desc, perr := sdputil.Parse(res.Body()); perr == nil {
			c.peerDir = desc.Direction
		}
	}
	if hold {
		c.setState(Held)
	} else {
		c.setState(Connected)
	}
	c.applyMedia()
	c.mu.Unlock()

	c.emit(Event{Channel: c.ID, Text: action + " done"})
	return nil
}

// HandleReInvite answers an in-dialog INVITE from the peer, which is how
// remote hold and retrieve arrive (FR-4.8).
func (c *Channel) HandleReInvite(req *sip.Request, tx sip.ServerTransaction) {
	c.mu.Lock()

	if !c.state.InCall() || c.session == nil {
		c.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		return
	}

	if len(req.Body()) > 0 {
		desc, err := sdputil.Parse(req.Body())
		if err != nil {
			c.mu.Unlock()
			_ = tx.Respond(sip.NewResponseFromRequest(req,
				sip.StatusNotAcceptableHere, "Bad SDP", nil))
			return
		}
		wasHeld := c.remoteHeld
		if err := c.applyRemote(desc); err != nil {
			c.mu.Unlock()
			_ = tx.Respond(sip.NewResponseFromRequest(req,
				sip.StatusNotAcceptableHere, "Bad Media Address", nil))
			return
		}
		if c.remoteHeld != wasHeld {
			text := "remote retrieved the call"
			if c.remoteHeld {
				text = "remote placed us on hold"
			}
			defer c.emit(Event{Channel: c.ID, Text: text})
		}
	}

	dir := sdputil.SendRecv
	if c.localHeld {
		dir = sdputil.HeldByUs(c.peerDir)
	}
	sdp, err := c.buildSDP(dir)
	if err != nil {
		c.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusInternalServerError, "SDP Build Failed", nil))
		return
	}
	c.applyMedia()
	c.mu.Unlock()

	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", sdp)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	contact := c.ua.Contact()
	res.AppendHeader(&contact)

	// A re-INVITE is how the peer refreshes the session, so echo the agreed
	// timer back and restart our watchdog. Without this the call would be torn
	// down mid-conversation even though the peer is refreshing correctly.
	if st := c.SessionTimer(); st.Active() {
		res.AppendHeader(sip.NewHeader("Session-Expires",
			sipua.SessionExpiresValue(st.Interval, st.Refresher)))
		res.AppendHeader(sip.NewHeader("Require", "timer"))
	}

	if err := tx.Respond(res); err != nil {
		c.log.Warn("re-INVITE response failed", "error", err)
	}
	c.noteSessionRefreshed()
}

// respondAnswer sends the 200 OK with SDP plus any negotiated extra headers.
func (c *Channel) respondAnswer(dlg *sipgo.DialogServerSession, sdp []byte,
	extra []sip.Header) error {

	if len(extra) == 0 {
		return dlg.RespondSDP(sdp)
	}
	headers := append([]sip.Header{
		sip.NewHeader("Content-Type", "application/sdp"),
	}, extra...)
	return dlg.Respond(sip.StatusOK, "OK", sdp, headers...)
}

// SessionTimer reports the negotiated session timer for this channel.
func (c *Channel) SessionTimer() sipua.SessionTimer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionTimer
}

// HandleBye answers a peer BYE. sipgo's ReadBye responds 200 and moves the
// dialog to Ended, so watchDialog performs the teardown and reports it once.
func (c *Channel) HandleBye(req *sip.Request, tx sip.ServerTransaction) {
	c.mu.RLock()
	dlg := c.dialog
	c.mu.RUnlock()

	if dlg == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		return
	}
	if err := dlg.ReadBye(req, tx); err != nil {
		c.log.Warn("BYE handling failed", "error", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
		c.reset("call ended by remote")
	}
}

// HandleAck completes an inbound INVITE transaction.
func (c *Channel) HandleAck(req *sip.Request, tx sip.ServerTransaction) {
	c.mu.RLock()
	dlg := c.serverDlg
	c.mu.RUnlock()
	if dlg == nil {
		return // ACK for a re-INVITE we answered, or a stale transaction
	}
	if err := dlg.ReadAck(req, tx); err != nil {
		c.log.Debug("ACK not matched to dialog", "error", err)
	}
}

// HandleCancel aborts a ringing inbound call.
func (c *Channel) HandleCancel(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	if c.State() == Ringing {
		c.reset("caller cancelled")
	}
}

// SendDTMF queues digits on this channel (FR-4.10).
func (c *Channel) SendDTMF(digits string) error {
	if err := media.ValidateDTMF(digits); err != nil {
		return err
	}
	c.mu.RLock()
	sess, state := c.session, c.state
	c.mu.RUnlock()

	if sess == nil || !state.InCall() {
		return &StateError{Channel: c.ID, State: state, Action: "send DTMF",
			Detail: "no established call"}
	}
	if err := sess.QueueDTMF(digits); err != nil {
		return err
	}
	if mode := c.cfg.Media.DTMFMode; mode == "info" || mode == "both" {
		c.sendDTMFInfo(digits)
	}
	return nil
}

// sendDTMFInfo sends digits as SIP INFO bodies for PBXs that want them.
func (c *Channel) sendDTMFInfo(digits string) {
	c.mu.RLock()
	dlg, target := c.dialog, c.info.RemoteTarget
	c.mu.RUnlock()
	if dlg == nil {
		return
	}

	go func() {
		for _, d := range digits {
			req := sip.NewRequest(sip.INFO, target)
			req.AppendHeader(sip.NewHeader("Content-Type", "application/dtmf-relay"))
			req.SetBody([]byte(fmt.Sprintf("Signal=%c\r\nDuration=200\r\n", d)))

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if _, err := dlg.Do(ctx, req); err != nil {
				c.log.Debug("DTMF INFO failed", "digit", string(d), "error", err)
			}
			cancel()
			time.Sleep(260 * time.Millisecond)
		}
	}()
}

// ProbeDialog asks the peer whether it still holds this channel's dialog.
//
// alive is false only when the peer explicitly says the dialog is gone (481);
// an error or timeout leaves alive true, because "we could not tell" must not
// be mistaken for "it is dead".
func (c *Channel) ProbeDialog(ctx context.Context) (alive bool, code int, err error) {
	c.mu.RLock()
	dlg, target, state := c.dialog, c.info.RemoteTarget, c.state
	c.mu.RUnlock()

	if dlg == nil || !state.InCall() {
		return false, 0, fmt.Errorf("channel %d has no dialog to probe (%s)", c.ID, state)
	}

	res, err := c.ua.SendOptions(ctx, dlg, target)
	if err != nil {
		return true, 0, err
	}
	if sipua.DialogGone(int(res.StatusCode)) {
		return false, int(res.StatusCode), nil
	}
	return true, int(res.StatusCode), nil
}

// DiscardDialog clears the channel without sending BYE, for when the peer has
// already dropped the dialog. Sending BYE would only earn a 481.
func (c *Channel) DiscardDialog(reason string) {
	c.mu.Lock()
	if c.dialog != nil {
		// Stop the state watcher from reporting this a second time.
		c.dialog = nil
		c.clientDlg, c.serverDlg = nil, nil
	}
	c.mu.Unlock()
	c.reset(reason)
}

// SetActive attaches or detaches this channel from the audio device (FR-3.4).
func (c *Channel) SetActive(active bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = active
	c.applyRouting()
}

// mediaDirection works out which way audio should flow, from our own hold
// state and the direction the peer last declared (RFC 3264 §6.1).
//
// Our own hold wins: a held channel neither sends nor receives (FR-8.7). Then
// the peer's attribute is honoured from our side of the stream -- a peer that
// says sendonly is sending to us, so we receive and do not send.
func (c *Channel) mediaDirection() (send, recv bool) {
	switch {
	case c.state == Ringing && !c.inbound:
		// Before answer we only listen, for ringback or early media.
		return false, true

	case !c.state.InCall():
		return false, false

	case c.localHeld:
		return false, false
	}

	switch c.peerDir {
	case sdputil.SendOnly:
		return false, true
	case sdputil.RecvOnly:
		return true, false
	case sdputil.Inactive:
		return false, false
	}
	return true, true
}

// applyMedia reconciles both the RTP session direction and the audio device
// attachment with the call state. Every state change that can affect media
// routes through here, so the two can never disagree.
func (c *Channel) applyMedia() {
	send, recv := c.mediaDirection()

	if c.session != nil {
		c.session.SetDirection(send, recv)
	}
	if c.leg == nil {
		return
	}
	if !c.active {
		// Only the active channel is wired to the microphone and speakers.
		c.leg.SetAttached(false, false)
		return
	}
	c.leg.SetAttached(send, recv)
}

// applyRouting is retained as the name used by state transitions.
func (c *Channel) applyRouting() { c.applyMedia() }

// RefreshRouting re-applies audio routing after an external state change.
func (c *Channel) RefreshRouting() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyMedia()
}

// SetMuted mutes the microphone for this channel.
func (c *Channel) SetMuted(m bool) error {
	c.mu.RLock()
	sess := c.session
	c.mu.RUnlock()
	if sess == nil {
		return fmt.Errorf("channel %d has no active call to mute", c.ID)
	}
	sess.SetMuted(m)
	return nil
}

// SetTransferring marks the channel as having a REFER outstanding (FR-5.7).
func (c *Channel) SetTransferring(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case on && c.state.InCall():
		c.setState(Transferring)
	case !on && c.state == Transferring:
		if c.localHeld {
			c.setState(Held)
		} else {
			c.setState(Connected)
		}
	}
	c.applyMedia()
}

// Terminate is called on shutdown: hang up without emitting chatter.
func (c *Channel) Terminate(ctx context.Context) {
	if !c.State().Busy() {
		return
	}
	_ = c.Hangup(ctx)
}

// MatchesCallID reports whether this channel's call carries the given Call-ID.
// Used to match a CANCEL, which arrives before any dialog tags exist.
func (c *Channel) MatchesCallID(callID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.info.CallID == callID {
		return true
	}
	// A ringing inbound call has no captured identity yet.
	if c.serverDlg != nil && c.serverDlg.InviteRequest != nil {
		if h := c.serverDlg.InviteRequest.CallID(); h != nil {
			return h.Value() == callID
		}
	}
	return false
}
