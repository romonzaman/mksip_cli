package channel

import (
	"context"
	"time"

	"github.com/emiago/sipgo/sip"

	"sipclient/internal/sdputil"
	"sipclient/internal/sipua"
)

// Session timers (RFC 4028) for one channel.
//
// Two jobs, depending on which end is the refresher:
//
//   - We refresh: send a re-INVITE at half the interval. If we do not, the peer
//     tears the call down -- silently, from the user's point of view.
//   - The peer refreshes: watch for its re-INVITE. If none arrives within the
//     interval plus a grace period, the session is dead and we hang up rather
//     than hold a call that no longer exists at the other end.

// startSessionTimer begins the refresh or watchdog loop for a negotiated timer.
// Caller must hold mu.
func (c *Channel) startSessionTimer(st sipua.SessionTimer) {
	c.stopSessionTimerLocked()

	c.sessionTimer = st
	if !st.Active() {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.stopSessionTimer = cancel

	c.log.Info("session timer negotiated", "timer", st.String())
	go c.runSessionTimer(ctx, st)
}

// stopSessionTimerLocked ends any running timer. Caller must hold mu.
func (c *Channel) stopSessionTimerLocked() {
	if c.stopSessionTimer != nil {
		c.stopSessionTimer()
		c.stopSessionTimer = nil
	}
	c.sessionTimer = sipua.SessionTimer{}
}

// noteSessionRefreshed restarts the watchdog after the peer's refresh arrives.
func (c *Channel) noteSessionRefreshed() {
	c.mu.Lock()
	st := c.sessionTimer
	c.mu.Unlock()

	if !st.Active() || st.WeRefresh {
		return
	}
	// Restarting the whole timer is the simplest correct way to reset the
	// deadline, and refreshes are minutes apart so the churn is irrelevant.
	c.mu.Lock()
	c.startSessionTimer(st)
	c.mu.Unlock()
}

// runSessionTimer is the refresh loop, or the watchdog when the peer refreshes.
func (c *Channel) runSessionTimer(ctx context.Context, st sipua.SessionTimer) {
	if st.WeRefresh {
		c.refreshLoop(ctx, st)
		return
	}

	select {
	case <-ctx.Done():
	case <-time.After(st.ExpiresAfter()):
		// The peer promised to refresh and did not. The call is over at its
		// end, so stop pretending it is up here.
		c.log.Warn("session timer expired with no refresh from the peer",
			"interval", st.Interval.String())
		c.emit(Event{Kind: EventWarning, Channel: c.ID,
			Text: "call ended: the far end stopped refreshing the session"})
		c.hangupExpired()
	}
}

// refreshLoop sends a re-INVITE at half the interval, for as long as the call
// lives.
func (c *Channel) refreshLoop(ctx context.Context, st sipua.SessionTimer) {
	ticker := time.NewTicker(st.RefreshAfter())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := c.sendSessionRefresh(ctx, st); err != nil {
			// One failure is not fatal: there is another half-interval before
			// the peer gives up, so log it and try again on the next tick.
			c.log.Warn("session refresh failed, will retry", "error", err)
			continue
		}
		c.log.Debug("session refreshed", "interval", st.Interval.String())
	}
}

// sendSessionRefresh sends the keep-alive re-INVITE. It carries the current
// SDP unchanged: this refreshes the session, it does not renegotiate media.
func (c *Channel) sendSessionRefresh(ctx context.Context, st sipua.SessionTimer) error {
	c.mu.Lock()
	if !c.state.InCall() || c.dialog == nil {
		c.mu.Unlock()
		return nil // the call ended; the timer is about to be cancelled
	}

	dir := sdputil.SendRecv
	if c.localHeld {
		dir = sdputil.HeldByUs(c.peerDir)
	}
	sdp, err := c.buildSDP(dir)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	dlg, target := c.dialog, c.info.RemoteTarget
	c.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, 32*time.Second)
	defer cancel()

	res, err := c.ua.SendReInvite(reqCtx, dlg, target, sdp,
		sip.NewHeader("Session-Expires",
			sipua.SessionExpiresValue(st.Interval, st.Refresher)),
		sip.NewHeader("Supported", "timer"))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &refreshRejected{code: int(res.StatusCode), reason: res.Reason}
	}
	return nil
}

// refreshRejected reports a non-2xx answer to a session refresh.
type refreshRejected struct {
	code   int
	reason string
}

func (e *refreshRejected) Error() string {
	return "session refresh rejected: " + itoa(e.code) + " " + e.reason
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// hangupExpired tears the call down after the session timer lapsed.
func (c *Channel) hangupExpired() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Hangup(ctx); err != nil {
		// The peer has almost certainly gone; clear our side regardless so the
		// channel does not sit forever showing a call that is not there.
		c.log.Debug("hangup after session expiry", "error", err)
		c.DiscardDialog("session expired")
	}
}
