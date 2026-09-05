package channel

import (
	"time"

	"sipclient/internal/history"
)

// Recording a finished call.
//
// The disposition is what the operator actually scans for, so it is worked out
// carefully rather than lumped into "ended". A missed call and a call the far
// end never answered look identical in the state machine but mean opposite
// things to the person reading the log.

// finishRecord builds the history entry for a call that is ending, or returns
// false when there is nothing worth recording. Caller must hold mu.
func (c *Channel) finishRecord() (history.Record, bool) {
	if c.recordCall == nil || c.startedAt.IsZero() {
		return history.Record{}, false
	}

	r := history.Record{
		Direction: history.Outbound,
		Remote:    c.remote,
		RemoteURI: c.remoteURI,
		Channel:   c.ID,
		StartedAt: c.startedAt,
		EndedAt:   time.Now(),
		Codec:     c.codec.Name,
		Code:      c.hintCode,
		Reason:    c.hintReason,
	}
	if c.inbound {
		r.Direction = history.Inbound
	}
	if !c.connectedAt.IsZero() {
		answered := c.connectedAt
		r.AnsweredAt = &answered
		r.TalkSeconds = int(time.Since(answered).Seconds())
	}

	r.Disposition = c.disposition()
	return r, true
}

// disposition decides how the call ended. Caller must hold mu.
func (c *Channel) disposition() history.Disposition {
	// An explicit hint always wins: only the code path that rejected a call or
	// saw a failure response knows which it was.
	if c.dispositionHint != "" {
		return c.dispositionHint
	}
	if !c.connectedAt.IsZero() {
		return history.Answered
	}
	// Never connected. Inbound means it rang here and nobody took it; outbound
	// means we gave up before the far end answered.
	if c.inbound {
		return history.Missed
	}
	return history.Cancelled
}

// noteDisposition records how a call ended, for paths that know more than the
// state machine can infer. Caller must NOT hold mu.
func (c *Channel) noteDisposition(d history.Disposition, code int, reason string) {
	c.mu.Lock()
	c.dispositionHint = d
	c.hintCode = code
	c.hintReason = reason
	c.mu.Unlock()
}

// clearRecordState resets the per-call recording fields. Caller must hold mu.
func (c *Channel) clearRecordState() {
	c.dispositionHint = ""
	c.hintCode = 0
	c.hintReason = ""
	c.remoteURI = ""
}
