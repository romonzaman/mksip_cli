package web

import (
	"context"
	"fmt"
	"strings"

	"sipclient/internal/transfer"
)

// runCommand performs one browser command and replies with exactly one result.
//
// Errors are passed through verbatim: the controller's messages already name
// the channel and its state (FR-9.6), so the browser shows the same sentence
// the terminal would.
func (s *Server) runCommand(ctx context.Context, c *wsClient, cmd Command) {
	text, err := s.dispatch(ctx, cmd)
	if err != nil {
		s.replyErr(c, cmd.ID, err)
		return
	}
	s.reply(c, ResultMessage{Type: TypeResult, ID: cmd.ID, OK: true, Text: text})

	// Push state immediately rather than waiting for a change notification, so
	// the button the operator pressed reflects reality at once.
	s.broadcastState()
}

// dispatch maps an action to a controller operation. The action names mirror
// the REPL verbs exactly, so the two surfaces cannot drift in vocabulary.
func (s *Server) dispatch(ctx context.Context, cmd Command) (string, error) {
	switch strings.ToLower(cmd.Action) {
	case "dial":
		if strings.TrimSpace(cmd.Target) == "" {
			return "", fmt.Errorf("dial needs a target")
		}
		id, err := s.ctl.Dial(ctx, cmd.Target, cmd.Channel)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("channel %d: calling %s", id, cmd.Target), nil

	case "answer":
		if err := s.ctl.Answer(ctx, cmd.Channel); err != nil {
			return "", err
		}
		return "answered", nil

	case "reject":
		if err := s.ctl.Reject(ctx, cmd.Channel, cmd.Code); err != nil {
			return "", err
		}
		return "rejected", nil

	case "hangup":
		if cmd.All {
			s.ctl.HangupAll(ctx)
			return "all channels cleared", nil
		}
		if err := s.ctl.Hangup(ctx, cmd.Channel); err != nil {
			return "", err
		}
		return "hung up", nil

	case "hold":
		if err := s.ctl.Hold(ctx, cmd.Channel, true); err != nil {
			return "", err
		}
		return "on hold", nil

	case "unhold":
		if err := s.ctl.Hold(ctx, cmd.Channel, false); err != nil {
			return "", err
		}
		return "retrieved", nil

	case "swap":
		if err := s.ctl.Swap(ctx); err != nil {
			return "", err
		}
		return "swapped", nil

	case "xfer":
		transferee, consult, err := s.ctl.TransferRoles(cmd.Transferee, cmd.Consult)
		if err != nil {
			return "", err
		}
		res, err := s.ctl.AttendedTransfer(ctx, transferee, consult)
		return transferText(res, err)

	case "bxfer":
		if strings.TrimSpace(cmd.Target) == "" {
			return "", fmt.Errorf("blind transfer needs a target")
		}
		res, err := s.ctl.BlindTransfer(ctx, cmd.Channel, cmd.Target)
		return transferText(res, err)

	case "cancelxfer":
		back, err := s.ctl.CancelConsult(ctx, cmd.Channel)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("consultation abandoned; back on channel %d", back), nil

	case "dtmf":
		if err := s.ctl.SendDTMF(cmd.Channel, cmd.Digits); err != nil {
			return "", err
		}
		return "sent DTMF " + cmd.Digits, nil

	case "mute":
		s.ctl.SetMuted(true)
		return "microphone muted", nil

	case "unmute":
		s.ctl.SetMuted(false)
		return "microphone live", nil
	}

	return "", fmt.Errorf("unknown action %q", cmd.Action)
}

// transferText renders a transfer outcome, keeping the distinction between a
// confirmed success and one inferred from probing (FR-5.5).
func transferText(res transfer.Result, err error) (string, error) {
	switch {
	case err != nil:
		return "", err

	case res.Outcome == transfer.Success && res.Inferred:
		msg := "transfer completed, but the PBX sent no NOTIFY to confirm it"
		if res.ProbeDetail != "" {
			msg += " -- " + res.ProbeDetail
		}
		return msg + "; verify with the parties before hanging up the remaining channel", nil

	case res.Outcome == transfer.Success:
		return fmt.Sprintf("transfer succeeded (%d %s); the parties are talking directly",
			res.Code, res.Reason), nil

	case res.Outcome == transfer.Failed:
		return "", fmt.Errorf("transfer failed: %d %s -- both calls are still up, "+
			"`swap` takes the held party back", res.Code, res.Reason)
	}

	return "", fmt.Errorf("transfer outcome unknown -- both calls are still up")
}
