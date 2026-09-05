// Package control holds the operations a user can perform on the client:
// dialling, answering, holding, transferring and so on.
//
// It exists so that every surface driving the client -- the terminal REPL and
// the web UI -- performs the same operation the same way. These operations are
// more than thin wrappers over the channel manager: dialling picks a channel
// and auto-holds the other, transferring works out which leg is the transferee.
// Implemented per surface, those rules would drift, and the browser and the
// terminal would behave differently for the same action.
//
// Nothing here prints or formats. Callers render the results.
package control

import (
	"context"
	"fmt"

	"sipclient/internal/channel"
	"sipclient/internal/config"
	"sipclient/internal/media"
	"sipclient/internal/sipua"
	"sipclient/internal/transfer"
)

// Controller performs client operations.
type Controller struct {
	cfg  config.Config
	ua   *sipua.UA
	mgr  *channel.Manager
	xfer *transfer.Transferor

	audioDevice string
	webURL      string
}

// New builds a Controller.
func New(cfg config.Config, ua *sipua.UA, mgr *channel.Manager,
	xfer *transfer.Transferor) *Controller {
	return &Controller{cfg: cfg, ua: ua, mgr: mgr, xfer: xfer}
}

// Manager exposes the channel manager for surfaces that need to subscribe to
// events or read channel objects directly.
func (c *Controller) Manager() *channel.Manager { return c.mgr }

// UA exposes the SIP stack, for registration state and target resolution.
func (c *Controller) UA() *sipua.UA { return c.ua }

// Config returns the effective configuration.
func (c *Controller) Config() config.Config { return c.cfg }

// ChannelCount is how many channels exist.
func (c *Controller) ChannelCount() int { return len(c.mgr.Channels()) }

// validChannel rejects an out-of-range channel number with a message naming
// the valid range (FR-9.6).
func (c *Controller) validChannel(id int) error {
	if id < 1 || id > c.ChannelCount() {
		return fmt.Errorf("no channel %d (valid: 1-%d)", id, c.ChannelCount())
	}
	return nil
}

// Dial places a call. channelID of 0 selects a channel automatically: the
// active one when it is idle, otherwise the first idle one. It returns the
// channel used.
func (c *Controller) Dial(ctx context.Context, target string, channelID int) (int, error) {
	uri, err := c.ua.ResolveTarget(target)
	if err != nil {
		return 0, err
	}

	var ch *channel.Channel
	if channelID == 0 {
		if active := c.mgr.Active(); active.State() == channel.Idle {
			ch = active
		} else if idle := c.mgr.FirstIdle(); idle != nil {
			ch = idle
		} else {
			return 0, fmt.Errorf("both channels are busy; hang up one first")
		}
	} else {
		if err := c.validChannel(channelID); err != nil {
			return 0, err
		}
		if ch, err = c.mgr.Get(channelID); err != nil {
			return 0, err
		}
	}

	// Dialling moves the audio to this channel, which auto-holds the other.
	if err := c.mgr.SetActive(ctx, ch.ID); err != nil {
		return 0, err
	}
	if err := ch.Dial(ctx, uri, target); err != nil {
		return 0, err
	}
	return ch.ID, nil
}

// RingingChannel finds the channel a bare answer or reject should act on.
func (c *Controller) RingingChannel() (int, error) {
	for _, ch := range c.mgr.Channels() {
		s := ch.Snapshot()
		if s.State == channel.Ringing && s.Inbound {
			return ch.ID, nil
		}
	}
	return 0, fmt.Errorf("no ringing inbound call")
}

// resolveRinging turns an optional channel number into a concrete one.
func (c *Controller) resolveRinging(channelID int) (*channel.Channel, error) {
	if channelID == 0 {
		id, err := c.RingingChannel()
		if err != nil {
			return nil, err
		}
		channelID = id
	}
	if err := c.validChannel(channelID); err != nil {
		return nil, err
	}
	return c.mgr.Get(channelID)
}

// Answer accepts a ringing inbound call. channelID of 0 picks the ringing one.
func (c *Controller) Answer(ctx context.Context, channelID int) error {
	ch, err := c.resolveRinging(channelID)
	if err != nil {
		return err
	}
	if err := c.mgr.SetActive(ctx, ch.ID); err != nil {
		return err
	}
	return ch.Answer(ctx)
}

// Reject declines a ringing inbound call with the given SIP code.
func (c *Controller) Reject(ctx context.Context, channelID, code int) error {
	if code == 0 {
		code = DefaultRejectCode
	}
	if code < 300 || code > 699 {
		return fmt.Errorf("reject code must be 300-699, got %d", code)
	}
	ch, err := c.resolveRinging(channelID)
	if err != nil {
		return err
	}
	return ch.Reject(ctx, code, "Decline")
}

// DefaultRejectCode is 603 Decline.
const DefaultRejectCode = 603

// ResolveChannelID turns an optional channel number into a concrete one, 0
// meaning the active channel. Surfaces use it to report which channel an
// action landed on.
func (c *Controller) ResolveChannelID(channelID int) (int, error) {
	ch, err := c.resolve(channelID)
	if err != nil {
		return 0, err
	}
	return ch.ID, nil
}

// Hangup ends a call. channelID of 0 acts on the active channel.
func (c *Controller) Hangup(ctx context.Context, channelID int) error {
	ch, err := c.resolve(channelID)
	if err != nil {
		return err
	}
	return ch.Hangup(ctx)
}

// HangupAll clears every channel.
func (c *Controller) HangupAll(ctx context.Context) {
	c.mgr.HangupAll(ctx)
}

// resolve turns an optional channel number into a channel, 0 meaning active.
func (c *Controller) resolve(channelID int) (*channel.Channel, error) {
	if channelID == 0 {
		return c.mgr.Active(), nil
	}
	if err := c.validChannel(channelID); err != nil {
		return nil, err
	}
	return c.mgr.Get(channelID)
}

// Hold puts a call on hold, or retrieves it when hold is false. Retrieving
// also moves the audio to that channel.
func (c *Controller) Hold(ctx context.Context, channelID int, hold bool) error {
	ch, err := c.resolve(channelID)
	if err != nil {
		return err
	}
	if hold {
		return ch.Hold(ctx)
	}
	if err := c.mgr.SetActive(ctx, ch.ID); err != nil {
		return err
	}
	return ch.Unhold(ctx)
}

// Swap holds the active channel and retrieves the other.
func (c *Controller) Swap(ctx context.Context) error { return c.mgr.Swap(ctx) }

// TransferRoles works out which channel holds the transferee and which the
// consultation. Zero for either means "infer it": the consultation is the
// active channel and the transferee is the other.
func (c *Controller) TransferRoles(transfereeID, consultID int) (int, int, error) {
	switch {
	case transfereeID == 0 && consultID == 0:
		consultID = c.mgr.ActiveID()
		for _, ch := range c.mgr.Channels() {
			if ch.ID != consultID {
				transfereeID = ch.ID
			}
		}
	case consultID == 0:
		for _, ch := range c.mgr.Channels() {
			if ch.ID != transfereeID {
				consultID = ch.ID
			}
		}
	case transfereeID == 0:
		for _, ch := range c.mgr.Channels() {
			if ch.ID != consultID {
				transfereeID = ch.ID
			}
		}
	}

	if transfereeID == 0 || consultID == 0 {
		return 0, 0, fmt.Errorf(
			"could not determine channels; name both, as in `xfer <transferee> <consult>`")
	}
	if err := c.validChannel(transfereeID); err != nil {
		return 0, 0, err
	}
	if err := c.validChannel(consultID); err != nil {
		return 0, 0, err
	}
	return transfereeID, consultID, nil
}

// AttendedTransfer connects the transferee to the consulted party (§7.1).
func (c *Controller) AttendedTransfer(ctx context.Context, transfereeID, consultID int) (transfer.Result, error) {
	transfereeID, consultID, err := c.TransferRoles(transfereeID, consultID)
	if err != nil {
		return transfer.Result{}, err
	}
	return c.xfer.Attended(ctx, transfereeID, consultID)
}

// BlindTransfer sends a call to target without consulting (§7.2).
func (c *Controller) BlindTransfer(ctx context.Context, channelID int, target string) (transfer.Result, error) {
	ch, err := c.resolve(channelID)
	if err != nil {
		return transfer.Result{}, err
	}
	uri, err := c.ua.ResolveTarget(target)
	if err != nil {
		return transfer.Result{}, err
	}
	return c.xfer.Blind(ctx, ch.ID, uri)
}

// CancelConsult abandons a consultation and returns to the held party.
// consultID of 0 uses the active channel.
func (c *Controller) CancelConsult(ctx context.Context, consultID int) (transfereeID int, err error) {
	if consultID == 0 {
		consultID = c.mgr.ActiveID()
	}
	if err := c.validChannel(consultID); err != nil {
		return 0, err
	}
	for _, ch := range c.mgr.Channels() {
		if ch.ID != consultID {
			transfereeID = ch.ID
		}
	}
	if transfereeID == 0 {
		return 0, fmt.Errorf("no other channel to return to")
	}
	return transfereeID, c.xfer.CancelConsult(ctx, consultID, transfereeID)
}

// SendDTMF sends digits on a channel.
func (c *Controller) SendDTMF(channelID int, digits string) error {
	ch, err := c.resolve(channelID)
	if err != nil {
		return err
	}
	return ch.SendDTMF(digits)
}

// SetMuted mutes or unmutes the microphone.
func (c *Controller) SetMuted(muted bool) { c.mgr.SetMuted(muted) }

// Muted reports the microphone state.
func (c *Controller) Muted() bool { return c.mgr.Muted() }

// Register asks the registration loop to try again now.
func (c *Controller) Register() { c.ua.TriggerRegister() }

// Unregister de-registers from the PBX.
func (c *Controller) Unregister(ctx context.Context) error { return c.ua.Unregister(ctx) }

// Stats returns media statistics for a channel.
func (c *Controller) Stats(channelID int) (media.Stats, bool, error) {
	ch, err := c.resolve(channelID)
	if err != nil {
		return media.Stats{}, false, err
	}
	st, ok := ch.Stats()
	return st, ok, nil
}
