// Package channel implements the two-channel call model and its state machine
// (requirements §5, §6).
package channel

import (
	"fmt"
	"time"
)

// State is a channel's call state (FR-3.3).
type State int

const (
	Idle State = iota
	Calling
	Ringing
	Connected
	Held
	Transferring
	Terminated
)

func (s State) String() string {
	switch s {
	case Calling:
		return "CALLING"
	case Ringing:
		return "RINGING"
	case Connected:
		return "CONNECTED"
	case Held:
		return "HELD"
	case Transferring:
		return "TRANSFERRING"
	case Terminated:
		return "TERMINATED"
	}
	return "IDLE"
}

// Busy reports whether the channel is doing anything at all.
func (s State) Busy() bool { return s != Idle && s != Terminated }

// InCall reports whether a media session exists.
func (s State) InCall() bool {
	return s == Connected || s == Held || s == Transferring
}

// Snapshot is a consistent read of a channel for display (FR-9.2).
type Snapshot struct {
	ID       int
	State    State
	Remote   string
	Inbound  bool
	Active   bool
	Duration time.Duration

	LocalHeld  bool
	RemoteHeld bool
	Muted      bool
	Codec      string
	RTPPort    int
}

// Label renders the channel for the status line.
func (s Snapshot) Label() string {
	marker := " "
	if s.Active {
		marker = "*"
	}
	if s.State == Idle {
		return fmt.Sprintf("[%d]%s IDLE", s.ID, marker)
	}

	// Arrow shows call direction, as specified in FR-9.2.
	arrow := "\u2192"
	if s.Inbound {
		arrow = "\u2190"
	}
	out := fmt.Sprintf("[%d]%s %s %s %s", s.ID, marker, s.State, arrow, s.Remote)
	if s.State.InCall() {
		out += fmt.Sprintf("  %s", fmtDuration(s.Duration))
	}
	if s.RemoteHeld {
		out += " (remote hold)"
	}
	if s.Muted && s.Active {
		out += " (muted)"
	}
	return out
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// StateError reports a command issued against a channel that cannot accept it,
// naming the state so the message is actionable (FR-9.6).
type StateError struct {
	Channel int
	State   State
	Action  string
	Detail  string
}

func (e *StateError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("channel %d is %s, cannot %s: %s",
			e.Channel, e.State, e.Action, e.Detail)
	}
	return fmt.Sprintf("channel %d is %s, cannot %s", e.Channel, e.State, e.Action)
}
