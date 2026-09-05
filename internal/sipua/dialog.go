package sipua

import (
	"context"
	"fmt"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Dialog is the behaviour shared by sipgo's UAC and UAS sessions, so a channel
// can hold either one without caring which side originated the call.
type Dialog interface {
	Do(ctx context.Context, req *sip.Request) (*sip.Response, error)
	TransactionRequest(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error)
	WriteRequest(req *sip.Request) error
	// ReadBye answers a peer BYE, which also ends the dialog so state watchers
	// observe the teardown.
	ReadBye(req *sip.Request, tx sip.ServerTransaction) error
	Bye(ctx context.Context) error
	Close() error
	LoadState() sip.DialogState
	StateRead() <-chan sip.DialogState
}

// Compile-time proof that both sipgo session types fit.
var (
	_ Dialog = (*sipgo.DialogClientSession)(nil)
	_ Dialog = (*sipgo.DialogServerSession)(nil)
)

// DialogInfo is the dialog identity we need later, captured once at
// establishment so it is never read from sipgo's non-thread-safe fields.
type DialogInfo struct {
	CallID string
	// LocalTag is the tag we generated, RemoteTag the peer's.
	LocalTag  string
	RemoteTag string
	// RemoteTarget is the peer's Contact: where in-dialog requests are sent.
	RemoteTarget sip.Uri
	// RemoteURI is the peer's AOR, for display.
	RemoteURI sip.Uri
	Outbound  bool
}

// InfoFromUAC captures identity for a call we placed.
func InfoFromUAC(s *sipgo.DialogClientSession) (DialogInfo, error) {
	req, res := s.InviteRequest, s.InviteResponse
	if req == nil || res == nil {
		return DialogInfo{}, fmt.Errorf("dialog not established")
	}
	info := DialogInfo{Outbound: true}
	if h := req.CallID(); h != nil {
		info.CallID = h.Value()
	}
	if h := req.From(); h != nil {
		info.LocalTag, _ = h.Params.Get("tag")
	}
	if h := res.To(); h != nil {
		info.RemoteTag, _ = h.Params.Get("tag")
		info.RemoteURI = h.Address
	}
	info.RemoteTarget = remoteTarget(res.Contact(), info.RemoteURI)
	return info, info.validate()
}

// InfoFromUAS captures identity for a call we answered.
func InfoFromUAS(s *sipgo.DialogServerSession) (DialogInfo, error) {
	req := s.InviteRequest
	if req == nil {
		return DialogInfo{}, fmt.Errorf("dialog not established")
	}
	info := DialogInfo{}
	if h := req.CallID(); h != nil {
		info.CallID = h.Value()
	}
	// ReadInvite pre-generated our To tag on the stored invite request.
	if h := req.To(); h != nil {
		info.LocalTag, _ = h.Params.Get("tag")
	}
	if h := req.From(); h != nil {
		info.RemoteTag, _ = h.Params.Get("tag")
		info.RemoteURI = h.Address
	}
	info.RemoteTarget = remoteTarget(req.Contact(), info.RemoteURI)
	return info, info.validate()
}

func remoteTarget(contact *sip.ContactHeader, fallback sip.Uri) sip.Uri {
	if contact != nil && contact.Address.Host != "" {
		return contact.Address
	}
	return fallback
}

func (d DialogInfo) validate() error {
	var missing []string
	if d.CallID == "" {
		missing = append(missing, "call-id")
	}
	if d.LocalTag == "" {
		missing = append(missing, "local tag")
	}
	if d.RemoteTag == "" {
		missing = append(missing, "remote tag")
	}
	if len(missing) > 0 {
		return fmt.Errorf("incomplete dialog identity: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// ReplacesValue builds the Replaces parameter identifying this dialog for a
// third party to replace (RFC 3891).
//
// Tag orientation matters and is easy to get backwards: the tags are written
// from the perspective of the UA whose dialog is being replaced. That UA's own
// tag is our RemoteTag, so it becomes to-tag, and our LocalTag becomes
// from-tag. Getting this the wrong way round is the classic reason a PBX
// answers a transfer with 481 Call/Transaction Does Not Exist (FR-5.3).
func (d DialogInfo) ReplacesValue() string {
	return fmt.Sprintf("%s;to-tag=%s;from-tag=%s", d.CallID, d.RemoteTag, d.LocalTag)
}

// escapeURIHeader percent-encodes a value so it survives inside the
// "?Replaces=..." part of a Refer-To URI. sipgo writes URI headers verbatim,
// so the escaping has to happen here or the ';' separators corrupt the header.
func escapeURIHeader(s string) string {
	const unreserved = "abcdefghijklmnopqrstuvwxyz" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.!~*'()"
	var b strings.Builder
	b.Grow(len(s) * 3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// ReferTo renders a Refer-To header value, optionally carrying Replaces for an
// attended transfer.
func ReferTo(target sip.Uri, replaces string) string {
	uri := (&target).String()
	if replaces == "" {
		return "<" + uri + ">"
	}
	return fmt.Sprintf("<%s?Replaces=%s>", uri, escapeURIHeader(replaces))
}

// SendREFER sends a REFER inside an established dialog and returns the final
// response. A 202 Accepted only means the REFER was taken on; the outcome
// arrives later by NOTIFY (FR-5.2).
//
// requestURI must be the dialog's remote target. sipgo fills in the dialog
// headers but not the Request-URI, so leaving it empty produces a malformed
// "REFER sip:" request line that a strict PBX will reject.
func (u *UA) SendREFER(ctx context.Context, dlg Dialog, requestURI, target sip.Uri,
	replaces string) (*sip.Response, error) {

	req := sip.NewRequest(sip.REFER, requestURI)
	req.AppendHeader(sip.NewHeader("Refer-To", ReferTo(target, replaces)))
	aor := u.AORUri()
	req.AppendHeader(sip.NewHeader("Referred-By", "<"+(&aor).String()+">"))
	// Ask for the implicit refer subscription that carries the result.
	req.AppendHeader(sip.NewHeader("Event", "refer"))

	res, err := dlg.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("REFER: %w", err)
	}
	return res, nil
}

// SendReInvite sends an in-dialog INVITE with new SDP, used for hold and
// retrieve, and acknowledges the answer.
//
// sipgo's Do handles the INVITE transaction but deliberately does not ACK,
// since it cannot know whether the request is an initial INVITE or a
// re-INVITE, so the ACK is built here against the re-INVITE's own CSeq.
func (u *UA) SendReInvite(ctx context.Context, dlg Dialog, target sip.Uri, sdp []byte,
	extra ...sip.Header) (*sip.Response, error) {

	req := sip.NewRequest(sip.INVITE, target)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	for _, h := range extra {
		req.AppendHeader(h)
	}
	req.SetBody(sdp)

	res, err := dlg.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("re-INVITE: %w", err)
	}

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		ack := sip.NewRequest(sip.ACK, target)
		// buildReq reuses the dialog's last CSeq for ACK, which is the
		// re-INVITE's, exactly as RFC 3261 §13.2.2.4 requires.
		if err := dlg.WriteRequest(ack); err != nil {
			return res, fmt.Errorf("re-INVITE ACK: %w", err)
		}
	}
	return res, nil
}

// SendOptions sends an in-dialog OPTIONS, used to test whether the peer still
// has the dialog. A UAS with no matching dialog answers 481, which is what
// makes this a liveness probe; any other response means the dialog is alive,
// even a 405 or 501 from a peer that dislikes in-dialog OPTIONS.
func (u *UA) SendOptions(ctx context.Context, dlg Dialog, requestURI sip.Uri) (*sip.Response, error) {
	req := sip.NewRequest(sip.OPTIONS, requestURI)

	res, err := dlg.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("in-dialog OPTIONS: %w", err)
	}
	return res, nil
}

// DialogGone reports whether a response to an in-dialog request means the peer
// has discarded the dialog.
func DialogGone(code int) bool {
	return code == sip.StatusCallTransactionDoesNotExists
}

// RespondNotify acknowledges an inbound NOTIFY.
func RespondNotify(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
}

// SipfragStatus extracts the status code from a message/sipfrag NOTIFY body,
// which is how a REFER reports its progress and final outcome (FR-5.4).
func SipfragStatus(body []byte) (code int, reason string, ok bool) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "SIP/2.0") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(fields[1], "%d", &n); err != nil {
			continue
		}
		return n, strings.Join(fields[2:], " "), true
	}
	return 0, "", false
}

// SubscriptionTerminated reports whether a NOTIFY closed the refer
// subscription, which marks the end of the transfer regardless of outcome.
func SubscriptionTerminated(req *sip.Request) bool {
	h := req.GetHeader("Subscription-State")
	return h != nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Value())), "terminated")
}
