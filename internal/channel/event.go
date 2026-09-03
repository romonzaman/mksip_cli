package channel

import "fmt"

// EventKind classifies an asynchronous notification for the CLI (FR-9.4).
type EventKind int

const (
	EventInfo EventKind = iota
	EventIncoming
	EventStateChange
	EventTransfer
	EventDTMF
	EventWarning
	EventError
)

// Event is an asynchronous notification printed above the prompt.
type Event struct {
	Kind    EventKind
	Channel int // 0 when not channel specific
	Text    string
}

func (e Event) String() string {
	prefix := ""
	switch e.Kind {
	case EventIncoming:
		prefix = "*** incoming call"
	case EventTransfer:
		prefix = "transfer"
	case EventWarning:
		prefix = "warning"
	case EventError:
		prefix = "error"
	case EventDTMF:
		prefix = "dtmf"
	}

	switch {
	case e.Channel > 0 && prefix != "":
		return fmt.Sprintf("[%d] %s: %s", e.Channel, prefix, e.Text)
	case e.Channel > 0:
		return fmt.Sprintf("[%d] %s", e.Channel, e.Text)
	case prefix != "":
		return fmt.Sprintf("%s: %s", prefix, e.Text)
	}
	return e.Text
}
