// Package transfer implements attended and blind transfer over REFER with
// Replaces (requirements §7, RFC 3515/3891 profiled by RFC 5589).
package transfer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/emiago/sipgo/sip"

	"sipclient/internal/channel"
	"sipclient/internal/config"
	"sipclient/internal/sipua"
)

// Outcome is how a transfer ended.
type Outcome int

const (
	// Unknown means no final NOTIFY arrived in time. Neither leg is torn down,
	// because guessing wrong would drop a live call (FR-5.5).
	Unknown Outcome = iota
	Success
	Failed
)

func (o Outcome) String() string {
	switch o {
	case Success:
		return "succeeded"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Result describes a completed transfer attempt.
type Result struct {
	Outcome Outcome
	Code    int
	Reason  string

	// Inferred is set when no final NOTIFY arrived and the outcome was
	// established by probing the dialog instead. Some proxies and PBXs simply
	// never send the refer NOTIFY, so probing is the difference between an
	// actionable answer and "unknown".
	Inferred bool
	// ProbeDetail explains what the probe found, for the operator.
	ProbeDetail string
}

// Transferor performs transfers across the channel set.
type Transferor struct {
	cfg config.Config
	mgr *channel.Manager
	log *slog.Logger
}

// New builds a Transferor.
func New(cfg config.Config, mgr *channel.Manager, logger *slog.Logger) *Transferor {
	return &Transferor{cfg: cfg, mgr: mgr, log: logger}
}

// Attended completes a warm transfer: the party on transfereeID is connected to
// the party we have been consulting on consultID, and we drop out (§7.1).
//
// Both channels must hold established calls. The REFER goes out on the
// transferee's dialog and carries a Replaces pointing at the consult dialog.
func (t *Transferor) Attended(ctx context.Context, transfereeID, consultID int) (Result, error) {
	if transfereeID == consultID {
		return Result{}, fmt.Errorf("cannot transfer channel %d to itself", transfereeID)
	}

	transferee, err := t.mgr.Get(transfereeID)
	if err != nil {
		return Result{}, err
	}
	consult, err := t.mgr.Get(consultID)
	if err != nil {
		return Result{}, err
	}

	transfereeInfo, transfereeDlg, ok := transferee.DialogInfo()
	if !ok {
		return Result{}, fmt.Errorf(
			"channel %d has no established call to transfer (%s)",
			transfereeID, transferee.State())
	}
	consultInfo, _, ok := consult.DialogInfo()
	if !ok {
		return Result{}, fmt.Errorf(
			"channel %d has no established consultation call (%s)",
			consultID, consult.State())
	}
	if transferee.State() == channel.Transferring {
		return Result{}, fmt.Errorf("channel %d already has a transfer in progress", transfereeID)
	}

	target := consultInfo.RemoteTarget
	replaces := consultInfo.ReplacesValue()

	t.log.Info("starting attended transfer",
		"transferee_channel", transfereeID, "consult_channel", consultID,
		"target", (&target).String(), "replaces", replaces)

	// On success the consultation dialog is the one that gets replaced, so it
	// is the dialog to probe if no NOTIFY arrives.
	res, err := t.refer(ctx, transferee, transfereeDlg, transfereeInfo, target, replaces,
		consult,
		fmt.Sprintf("attended transfer of channel %d to channel %d's party",
			transfereeID, consultID))

	switch {
	case res.Outcome == Success && res.Inferred:
		// Established by probe, not by the PBX telling us. The consult leg is
		// already discarded; leave the transferee up and held so the operator
		// can confirm with the parties rather than dropping a live call on an
		// inference.

	case res.Outcome == Success && t.cfg.Transfer.HangupAfterSuccess:
		// The PBX has bridged A and C. Clear whatever it has not already torn
		// down on our side (FR-5.2 step 7).
		t.clear(ctx, transferee, consult)

	case res.Outcome == Failed:
		// Leave both calls up and the transferee on hold so `swap` recovers
		// the conversation. This runs for an outright REFER rejection too,
		// where err is non-nil (FR-5.2 step 8, FR-6.3).
		t.ensureRecoverable(ctx, transferee, consult)

		// Unknown deliberately changes nothing (FR-5.5).
	}
	return res, err
}

// Blind sends the party on chID straight to target without consulting (§7.2).
func (t *Transferor) Blind(ctx context.Context, chID int, target sip.Uri) (Result, error) {
	ch, err := t.mgr.Get(chID)
	if err != nil {
		return Result{}, err
	}
	info, dlg, ok := ch.DialogInfo()
	if !ok {
		return Result{}, fmt.Errorf("channel %d has no established call to transfer (%s)",
			chID, ch.State())
	}

	t.log.Info("starting blind transfer", "channel", chID, "target", (&target).String())

	// On a blind transfer the transferee leaves us, so its own dialog is what
	// disappears on success.
	res, err := t.refer(ctx, ch, dlg, info, target, "", ch,
		fmt.Sprintf("blind transfer of channel %d to %s", chID, (&target).String()))
	if err != nil {
		return res, err
	}
	if res.Outcome == Success && t.cfg.Transfer.HangupAfterSuccess {
		t.clear(ctx, ch)
	}
	return res, nil
}

// refer is the shared REFER-and-await-NOTIFY sequence.
func (t *Transferor) refer(ctx context.Context, ch *channel.Channel, dlg sipua.Dialog,
	info sipua.DialogInfo, target sip.Uri, replaces string,
	probe *channel.Channel, what string) (Result, error) {

	// Subscribe to the refer NOTIFYs before the REFER goes out, so a fast PBX
	// cannot deliver the result before we are listening.
	final := make(chan Result, 4)
	unregister := t.mgr.RegisterNotify(info.CallID, func(req *sip.Request) {
		t.onNotify(ch.ID, req, final)
	})
	defer unregister()

	ch.SetTransferring(true)
	// Any exit other than success returns the channel to its prior state.
	settled := false
	defer func() {
		if !settled {
			ch.SetTransferring(false)
		}
	}()

	res, err := t.mgr.UA().SendREFER(ctx, dlg, info.RemoteTarget, target, replaces)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", what, err)
	}

	switch {
	case res.StatusCode == sip.StatusAccepted || res.StatusCode == sip.StatusOK:
		// Expected: the transfer is under way.
	case res.StatusCode == sip.StatusMethodNotAllowed ||
		res.StatusCode == sip.StatusNotImplemented ||
		res.StatusCode == sip.StatusForbidden:
		// The single most likely interop failure, so name it plainly (FR-5.6).
		return Result{Outcome: Failed, Code: int(res.StatusCode), Reason: res.Reason},
			fmt.Errorf("PBX does not permit REFER-based transfer: %d %s",
				res.StatusCode, res.Reason)
	default:
		return Result{Outcome: Failed, Code: int(res.StatusCode), Reason: res.Reason},
			fmt.Errorf("REFER rejected: %d %s", res.StatusCode, res.Reason)
	}

	t.log.Info("REFER accepted, awaiting NOTIFY",
		"channel", ch.ID, "code", int(res.StatusCode))

	timeout := time.Duration(t.cfg.Transfer.NotifyTimeoutSeconds) * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return Result{Outcome: Unknown}, ctx.Err()

		case <-timer.C:
			// No NOTIFY. Rather than reporting a bare "unknown", ask the peer
			// whether the dialog that a successful transfer would replace still
			// exists (FR-5.5).
			return t.inferOutcome(ctx, probe, what, timeout)

		case r := <-final:
			if r.Outcome == Success {
				settled = true
			}
			return r, nil
		}
	}
}

// inferOutcome establishes the result of a transfer that produced no NOTIFY,
// by probing the dialog that a successful transfer replaces.
//
// A peer that answers 481 has discarded the dialog, which means the Replaces
// INVITE reached it -- the transfer took effect. A peer that still holds the
// dialog means nothing happened. Anything else is genuinely unknown.
func (t *Transferor) inferOutcome(ctx context.Context, probe *channel.Channel,
	what string, timeout time.Duration) (Result, error) {

	base := fmt.Sprintf("%s: no final NOTIFY within %s", what, timeout)

	if probe == nil || !probe.State().InCall() {
		// The dialog is already gone from our side, which is itself the answer.
		return Result{
			Outcome:     Success,
			Inferred:    true,
			ProbeDetail: "the consulted call had already ended",
		}, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	alive, code, err := probe.ProbeDialog(probeCtx)
	switch {
	case err != nil:
		t.log.Warn("could not probe the dialog after a silent transfer",
			"channel", probe.ID, "error", err)
		return Result{Outcome: Unknown}, fmt.Errorf(
			"%s and the dialog could not be probed (%v) -- outcome unknown, "+
				"both calls left up", base, err)

	case !alive:
		// The peer dropped the dialog, so the transfer reached it.
		t.log.Info("transfer completed without a NOTIFY, confirmed by probe",
			"channel", probe.ID, "code", code)
		probe.DiscardDialog("consulted party handed over by the transfer")
		return Result{
			Outcome:  Success,
			Code:     code,
			Reason:   "inferred",
			Inferred: true,
			ProbeDetail: fmt.Sprintf(
				"channel %d's dialog no longer exists on the PBX (%d), so the "+
					"transfer took effect", probe.ID, code),
		}, nil

	default:
		return Result{Outcome: Unknown, Code: code}, fmt.Errorf(
			"%s and channel %d's call is still established (%d) -- the transfer "+
				"did not take effect, both calls left up", base, probe.ID, code)
	}
}

// onNotify interprets one refer NOTIFY. Interim sipfrags are progress, not the
// result: only a final response ends the transfer (FR-5.4).
func (t *Transferor) onNotify(chID int, req *sip.Request, final chan<- Result) {
	code, reason, ok := sipua.SipfragStatus(req.Body())
	if !ok {
		if sipua.SubscriptionTerminated(req) {
			// Terminated with no parseable status: treat as unknown.
			select {
			case final <- Result{Outcome: Unknown}:
			default:
			}
		}
		return
	}

	switch {
	case code < 200:
		t.log.Info("transfer progress", "channel", chID, "code", code, "reason", reason)

	case code < 300:
		t.log.Info("transfer succeeded", "channel", chID, "code", code)
		select {
		case final <- Result{Outcome: Success, Code: code, Reason: reason}:
		default:
		}

	default:
		t.log.Warn("transfer failed", "channel", chID, "code", code, "reason", reason)
		select {
		case final <- Result{Outcome: Failed, Code: code, Reason: reason}:
		default:
		}
	}
}

// clear hangs up legs the PBX has not already torn down.
func (t *Transferor) clear(ctx context.Context, channels ...*channel.Channel) {
	for _, ch := range channels {
		if !ch.State().Busy() {
			continue // the PBX already sent BYE
		}
		hangupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := ch.Hangup(hangupCtx); err != nil {
			t.log.Debug("post-transfer hangup", "channel", ch.ID, "error", err)
		}
		cancel()
	}
}

// ensureRecoverable leaves a failed transfer in a state the operator can rescue:
// the transferee stays up and on hold, ready for `swap` (FR-5.2 step 8).
func (t *Transferor) ensureRecoverable(ctx context.Context, transferee, consult *channel.Channel) {
	s := transferee.Snapshot()
	if s.State == channel.Connected && !s.LocalHeld {
		if err := transferee.Hold(ctx); err != nil {
			t.log.Warn("could not re-hold transferee after failed transfer",
				"channel", transferee.ID, "error", err)
		}
	}
}

// CancelConsult abandons a consultation and returns to the transferee. This
// path must be reliable: a failed transfer must never leave the transferee
// stranded on hold (FR-6.3).
func (t *Transferor) CancelConsult(ctx context.Context, consultID, transfereeID int) error {
	consult, err := t.mgr.Get(consultID)
	if err != nil {
		return err
	}
	transferee, err := t.mgr.Get(transfereeID)
	if err != nil {
		return err
	}

	if !transferee.State().InCall() {
		return fmt.Errorf("channel %d has no call to return to (%s)",
			transfereeID, transferee.State())
	}

	// Drop the consult leg first, but do not give up on retrieving the
	// transferee if that fails.
	var firstErr error
	if consult.State().Busy() {
		if err := consult.Hangup(ctx); err != nil {
			firstErr = fmt.Errorf("dropping consultation on channel %d: %w", consultID, err)
			t.log.Warn("consult hangup failed, still retrieving transferee", "error", err)
		}
	}

	if err := t.mgr.SetActive(ctx, transfereeID); err != nil && firstErr == nil {
		firstErr = err
	}
	if s := transferee.Snapshot(); s.LocalHeld {
		if err := transferee.Unhold(ctx); err != nil {
			return fmt.Errorf("retrieving channel %d: %w", transfereeID, err)
		}
	}
	return firstErr
}
