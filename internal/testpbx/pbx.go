// Package testpbx is a minimal SIP registrar and UAS used to exercise the
// client without a real PBX. It implements just enough of a PBX to drive the
// acceptance scenarios in requirements.md §13.
package testpbx

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// ReferRecord is one observed REFER, decoded far enough to assert on.
type ReferRecord struct {
	CallID     string
	ReferTo    string // raw header value
	ReferredBy string
	TargetURI  string // Refer-To URI without the Replaces part
	Replaces   string // percent-decoded Replaces value
	ReplacesID string // Call-ID inside Replaces
	ToTag      string
	FromTag    string
}

// InviteRecord is one observed INVITE.
type InviteRecord struct {
	CallID     string
	RequestTo  string
	SDPDir     string // media direction from the offer
	SDPAddress string // connection address from the offer
	InDialog   bool   // true for a re-INVITE

	// Tags are captured when the dialog is created, because a successful
	// transfer tears the dialog down before a test can inspect it. PBXTag is
	// the tag this PBX generated (what a Replaces to-tag must carry);
	// ClientTag is the client's (the from-tag).
	PBXTag    string
	ClientTag string
}

// Records is everything the PBX saw, for assertions.
type Records struct {
	Registers      int
	AuthedRegister int
	Unregisters    int
	Invites        []InviteRecord
	Refers         []ReferRecord
	Byes           int
	Cancels        int
	Notifies       int
}

// PBX is the fake server.
type PBX struct {
	Addr  string
	Realm string

	// Network is the transport the client is expected to register over: "udp"
	// (default) or "tcp". Both are served regardless, as a real proxy does.
	// Must be set before Listen.
	Network string

	// CallNetwork is the transport used for INVITEs this PBX originates.
	// Empty means the same as Network. Set it to the other transport to check
	// a client accepts calls arriving over a transport it did not register on.
	CallNetwork string

	// Users maps username to password for digest validation.
	Users map[string]string

	// Behaviour knobs for the failure-path scenarios.
	ReferStatus       int           // response to REFER (default 202)
	ReferOutcome      int           // final sipfrag status (default 200)
	SendInterimNotify bool          // send a 100 Trying sipfrag first
	NotifyDelay       time.Duration // delay before the final NOTIFY
	SuppressNotify    bool          // never send the final NOTIFY (timeout test)

	// DropDialogAfterRefer silently discards the dialog named in the REFER's
	// Replaces, without sending BYE. Combined with SuppressNotify this
	// reproduces a PBX that performs the transfer but reports nothing, leaving
	// the client to work the outcome out for itself.
	DropDialogAfterRefer bool

	// IgnoreOptions drops in-dialog OPTIONS without responding, so a client's
	// liveness probe gets no answer at all.
	IgnoreOptions bool

	// SessionExpires, when non-zero, makes this PBX negotiate an RFC 4028
	// session timer on calls it answers, naming SessionRefresher as the party
	// responsible. With refresher=uac the client must send refresh re-INVITEs
	// or the PBX tears the call down -- the failure this reproduces.
	SessionExpires   time.Duration
	SessionRefresher string // "uac" or "uas"; empty means "uac"
	// MinSE, when non-zero, makes the PBX answer 422 to any Session-Expires
	// below it, so a client's retry path can be exercised.
	MinSE       time.Duration
	AnswerDelay time.Duration // delay before answering an INVITE

	// NotifyViaContact routes the refer NOTIFY by its Request-URI (our Contact)
	// instead of back down the connection the REFER arrived on. Over TCP that
	// forces a fresh connection to the port we advertise, which is what a proxy
	// does when it cannot reuse the existing one.
	NotifyViaContact bool

	// OnRegistered fires once, after the first successful REGISTER, with the
	// client's Contact and source address. It is how a test drives an inbound
	// call without guessing when the client is ready.
	//
	// Like every knob on this struct, it must be set before Listen: the
	// request handlers read these fields from their own goroutines.
	OnRegistered func(contact sip.Uri, source string)

	log      *slog.Logger
	ua       *sipgo.UserAgent
	client   *sipgo.Client
	server   *sipgo.Server
	dialogUA *sipgo.DialogUA

	mu      sync.Mutex
	records Records
	dialogs map[string]*sipgo.DialogServerSession

	outbound []*sipgo.DialogClientSession

	// refreshes counts session-refresh re-INVITEs received, per Call-ID.
	refreshes map[string]int
	// expired records calls torn down because no refresh arrived.
	expired map[string]bool

	rtpMu      sync.Mutex
	rtpConns   []*net.UDPConn
	rtpSeen    map[int]int // local port -> packets received
	dtmfDigits []string
	dtmfOpen   bool // an event is in progress, so end packets count once

	registeredOnce sync.Once

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start brings the PBX up on an ephemeral UDP port.
func New(logger *slog.Logger) (*PBX, error) {
	p := &PBX{
		Realm:        "testpbx",
		Network:      "udp",
		Users:        map[string]string{},
		ReferStatus:  sip.StatusAccepted,
		ReferOutcome: sip.StatusOK,
		log:          logger,
		dialogs:      map[string]*sipgo.DialogServerSession{},
		refreshes:    map[string]int{},
		expired:      map[string]bool{},
		rtpSeen:      map[int]int{},
	}

	// Reserve a port by binding, then hand it to sipgo. TCP and UDP port
	// spaces are separate, but an ephemeral TCP port is free for UDP too, so
	// one probe serves both.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	p.Addr = probe.LocalAddr().String()
	_ = probe.Close()

	p.ua, err = sipgo.NewUA(sipgo.WithUserAgent("testpbx"))
	if err != nil {
		return nil, err
	}
	host, port, _ := sip.ParseAddr(p.Addr)
	p.client, err = sipgo.NewClient(p.ua,
		sipgo.WithClientHostname(host), sipgo.WithClientPort(port))
	if err != nil {
		return nil, err
	}
	p.server, err = sipgo.NewServer(p.ua)
	if err != nil {
		return nil, err
	}
	p.dialogUA = &sipgo.DialogUA{
		Client: p.client,
		ContactHDR: sip.ContactHeader{
			Address: sip.Uri{Scheme: "sip", User: "pbx", Host: host, Port: port},
		},
	}

	p.routes()
	return p, nil
}

// Listen starts serving until Close.
func (p *PBX) Listen() error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	primary := p.Network
	if primary == "" {
		primary = "udp"
	}
	secondary := "tcp"
	if primary == "tcp" {
		secondary = "udp"
	}

	// Serve both transports, as a real proxy does.
	if err := p.serve(ctx, primary); err != nil {
		return err
	}
	if err := p.serve(ctx, secondary); err != nil {
		p.log.Warn("testpbx: secondary transport unavailable",
			"network", secondary, "error", err)
	}
	return nil
}

func (p *PBX) serve(ctx context.Context, network string) error {
	errc := make(chan error, 1)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		err := p.server.ListenAndServe(ctx, network, p.Addr)
		if err != nil && ctx.Err() == nil {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

// Close shuts the PBX down.
func (p *PBX) Close() {
	if p.cancel != nil {
		p.cancel()
	}
	_ = p.client.Close()
	_ = p.server.Close()
	_ = p.ua.Close()

	p.rtpMu.Lock()
	for _, c := range p.rtpConns {
		_ = c.Close()
	}
	p.rtpMu.Unlock()
	p.wg.Wait()
}

// Records snapshots what the PBX observed.
func (p *PBX) Records() Records {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := p.records
	out.Invites = append([]InviteRecord(nil), p.records.Invites...)
	out.Refers = append([]ReferRecord(nil), p.records.Refers...)
	return out
}

// RTPPacketsReceived totals RTP packets across all call legs, proving media
// actually flowed.
func (p *PBX) RTPPacketsReceived() int {
	p.rtpMu.Lock()
	defer p.rtpMu.Unlock()
	total := 0
	for _, n := range p.rtpSeen {
		total += n
	}
	return total
}

func (p *PBX) routes() {
	p.server.OnRegister(p.onRegister)
	p.server.OnInvite(p.onInvite)
	p.server.OnAck(p.onAck)
	p.server.OnBye(p.onBye)
	p.server.OnCancel(p.onCancel)
	p.server.OnRefer(p.onRefer)
	p.server.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		if p.IgnoreOptions {
			return // no response at all
		}
		// An in-dialog OPTIONS for a dialog we no longer hold must be 481,
		// which is what makes it usable as a liveness probe.
		if p.inDialogButUnknown(req) {
			_ = tx.Respond(sip.NewResponseFromRequest(req,
				sip.StatusCallTransactionDoesNotExists,
				"Call/Transaction Does Not Exist", nil))
			return
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})
	p.server.OnInfo(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})
}

// onRegister challenges once, then validates the digest properly so a wrong
// password really fails.
func (p *PBX) onRegister(req *sip.Request, tx sip.ServerTransaction) {
	p.mu.Lock()
	p.records.Registers++
	p.mu.Unlock()

	expiry := "300"
	if h := req.GetHeader("Expires"); h != nil {
		if strings.TrimSpace(h.Value()) == "0" {
			expiry = "0"
			p.mu.Lock()
			p.records.Unregisters++
			p.mu.Unlock()
		}
	}

	authHdr := req.GetHeader("Authorization")
	if authHdr == nil {
		res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
		chal := digest.Challenge{
			Realm:     p.Realm,
			Nonce:     sip.GenerateTagN(24),
			Algorithm: "MD5",
			QOP:       []string{"auth"},
		}
		res.AppendHeader(sip.NewHeader("WWW-Authenticate", chal.String()))
		_ = tx.Respond(res)
		return
	}

	cred, err := digest.ParseCredentials(authHdr.Value())
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	password, known := p.Users[cred.Username]
	if !known {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotFound, "Not Found", nil))
		return
	}

	// Recompute the digest and compare.
	expected, err := digest.Digest(&digest.Challenge{
		Realm:     cred.Realm,
		Nonce:     cred.Nonce,
		Algorithm: cred.Algorithm,
		QOP:       splitQOP(cred.QOP),
	}, digest.Options{
		Method:   string(req.Method),
		URI:      cred.URI,
		Username: cred.Username,
		Password: password,
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
	})
	if err != nil || expected.Response != cred.Response {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil))
		return
	}

	p.mu.Lock()
	p.records.AuthedRegister++
	p.mu.Unlock()

	if p.OnRegistered != nil && expiry != "0" {
		if c := req.Contact(); c != nil {
			contact, source := c.Address, req.Source()
			p.registeredOnce.Do(func() {
				go p.OnRegistered(contact, source)
			})
		}
	}

	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	if c := req.Contact(); c != nil {
		res.AppendHeader(sip.NewHeader("Contact",
			fmt.Sprintf("%s;expires=%s", c.Value(), expiry)))
	}
	res.AppendHeader(sip.NewHeader("Expires", expiry))
	_ = tx.Respond(res)
}

func splitQOP(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// onInvite answers as the called party, or re-answers a re-INVITE.
func (p *PBX) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := headerValue(req.CallID())
	toTag := ""
	if to := req.To(); to != nil {
		toTag, _ = to.Params.Get("tag")
	}

	dir := sdpDirection(req.Body())
	p.mu.Lock()
	p.records.Invites = append(p.records.Invites, InviteRecord{
		CallID:     callID,
		RequestTo:  (&req.Recipient).String(),
		SDPDir:     dir,
		SDPAddress: sdpAddress(req.Body()),
		InDialog:   toTag != "",
	})
	existing := p.dialogs[callID]
	p.mu.Unlock()

	// A re-INVITE: hold, retrieve, or a session refresh.
	if toTag != "" && existing != nil {
		if _, ok := sessionExpiresSeconds(req); ok {
			p.mu.Lock()
			p.refreshes[callID]++
			p.mu.Unlock()
		}

		port := p.rtpPortFor(callID)
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", p.sdp(port, mirror(dir)))
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		res.AppendHeader(p.contactFor(req))
		if p.SessionExpires > 0 {
			res.AppendHeader(sip.NewHeader("Session-Expires", p.sessionExpiresValue()))
		}
		_ = tx.Respond(res)
		return
	}

	// Enforce a minimum session interval, so a client's 422 retry can be tested.
	if p.MinSE > 0 {
		if want, ok := sessionExpiresSeconds(req); ok && want < int(p.MinSE.Seconds()) {
			res := sip.NewResponseFromRequest(req, sip.StatusIntervalToBrief,
				"Session Interval Too Small", nil)
			res.AppendHeader(sip.NewHeader("Min-SE",
				strconv.Itoa(int(p.MinSE.Seconds()))))
			_ = tx.Respond(res)
			return
		}
	}

	dlg, err := p.dialogUA.ReadInvite(req, tx)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	pbxTag, clientTag := dialogTags(dlg)
	p.mu.Lock()
	p.dialogs[callID] = dlg
	// Backfill the tags onto the record appended above.
	for i := len(p.records.Invites) - 1; i >= 0; i-- {
		if p.records.Invites[i].CallID == callID && !p.records.Invites[i].InDialog {
			p.records.Invites[i].PBXTag = pbxTag
			p.records.Invites[i].ClientTag = clientTag
			break
		}
	}
	p.mu.Unlock()

	_ = dlg.Respond(sip.StatusRinging, "Ringing", nil, p.contactFor(req))
	if p.AnswerDelay > 0 {
		time.Sleep(p.AnswerDelay)
	}

	port := p.rtpPortFor(callID)
	answerHeaders := []sip.Header{
		p.contactFor(req),
		sip.NewHeader("Content-Type", "application/sdp"),
	}
	if p.SessionExpires > 0 {
		answerHeaders = append(answerHeaders,
			sip.NewHeader("Session-Expires", p.sessionExpiresValue()),
			sip.NewHeader("Require", "timer"))
	}
	if err := dlg.Respond(sip.StatusOK, "OK", p.sdp(port, "sendrecv"),
		answerHeaders...,
	); err != nil {
		p.log.Error("testpbx: answer failed", "error", err)
	}

	if p.SessionExpires > 0 && p.refresherIsClient() {
		go p.watchSessionRefresh(callID, dlg)
	}
}

// sessionExpiresValue renders this PBX's Session-Expires header.
func (p *PBX) sessionExpiresValue() string {
	return fmt.Sprintf("%d;refresher=%s", int(p.SessionExpires.Seconds()), p.refresher())
}

func (p *PBX) refresher() string {
	if p.SessionRefresher == "" {
		return "uac"
	}
	return p.SessionRefresher
}

// refresherIsClient reports whether the client is expected to refresh.
func (p *PBX) refresherIsClient() bool { return p.refresher() == "uac" }

// watchSessionRefresh tears the call down if the client fails to refresh in
// time, exactly as a real PBX enforcing RFC 4028 would.
func (p *PBX) watchSessionRefresh(callID string, dlg *sipgo.DialogServerSession) {
	deadline := time.NewTimer(p.SessionExpires)
	defer deadline.Stop()

	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case <-deadline.C:
			p.mu.Lock()
			seen := p.refreshes[callID]
			gone := p.dialogs[callID] == nil
			p.mu.Unlock()

			if gone {
				return // call already ended
			}
			if seen > 0 {
				// Refreshed at least once; restart the window.
				p.mu.Lock()
				p.refreshes[callID] = 0
				p.mu.Unlock()
				deadline.Reset(p.SessionExpires)
				continue
			}

			p.log.Warn("testpbx: session expired without a refresh", "call_id", callID)
			p.mu.Lock()
			p.expired[callID] = true
			delete(p.dialogs, callID)
			p.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = dlg.Bye(ctx)
			cancel()
			return

		case <-poll.C:
			p.mu.Lock()
			gone := p.dialogs[callID] == nil
			p.mu.Unlock()
			if gone {
				return
			}
		}
	}
}

// SessionRefreshes reports how many session-refresh re-INVITEs arrived.
func (p *PBX) SessionRefreshes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, n := range p.refreshes {
		total += n
	}
	return total
}

// SessionExpiredCalls reports how many calls the PBX tore down because no
// refresh arrived.
func (p *PBX) SessionExpiredCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.expired)
}

// sessionExpiresSeconds reads a Session-Expires header, if present.
func sessionExpiresSeconds(msg *sip.Request) (int, bool) {
	h := msg.GetHeader("Session-Expires")
	if h == nil {
		return 0, false
	}
	first, _, _ := strings.Cut(h.Value(), ";")
	n, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		return 0, false
	}
	return n, true
}

// contactFor builds the Contact a real PBX would return: one naming the dialed
// extension, so a transferor's Refer-To identifies the party rather than just
// the switch. Asterisk and FreeSWITCH both behave this way.
func (p *PBX) contactFor(req *sip.Request) *sip.ContactHeader {
	host, port, _ := sip.ParseAddr(p.Addr)
	user := req.Recipient.User
	if user == "" {
		user = "pbx"
	}
	return &sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: user, Host: host, Port: port},
	}
}

// clientTarget is where in-dialog requests to the client go: its Contact.
func (p *PBX) clientTarget(dlg *sipgo.DialogServerSession) (sip.Uri, string) {
	if dlg.InviteRequest != nil {
		if c := dlg.InviteRequest.Contact(); c != nil {
			return c.Address, dlg.InviteRequest.Source()
		}
	}
	return sip.Uri{}, ""
}

func (p *PBX) onAck(req *sip.Request, tx sip.ServerTransaction) {
	p.mu.Lock()
	dlg := p.dialogs[headerValue(req.CallID())]
	p.mu.Unlock()
	if dlg != nil {
		_ = dlg.ReadAck(req, tx)
	}
}

func (p *PBX) onBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := headerValue(req.CallID())
	p.mu.Lock()
	p.records.Byes++
	dlg := p.dialogs[callID]
	delete(p.dialogs, callID)
	p.mu.Unlock()

	if dlg != nil {
		if err := dlg.ReadBye(req, tx); err == nil {
			return
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
		return
	}
	_ = tx.Respond(sip.NewResponseFromRequest(req,
		sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
}

// inDialogButUnknown reports whether a request claims to be inside a dialog
// this PBX does not have.
func (p *PBX) inDialogButUnknown(req *sip.Request) bool {
	to := req.To()
	if to == nil {
		return false
	}
	if tag, ok := to.Params.Get("tag"); !ok || tag == "" {
		return false // not in a dialog
	}
	callID := headerValue(req.CallID())
	if callID == "" {
		return false
	}

	p.mu.Lock()
	_, known := p.dialogs[callID]
	p.mu.Unlock()
	return !known
}

func (p *PBX) onCancel(req *sip.Request, tx sip.ServerTransaction) {
	p.mu.Lock()
	p.records.Cancels++
	p.mu.Unlock()
	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
}

// onRefer records the transfer request and reports the outcome by NOTIFY,
// which is the behaviour the client's transfer logic is written against.
func (p *PBX) onRefer(req *sip.Request, tx sip.ServerTransaction) {
	callID := headerValue(req.CallID())
	rec := ReferRecord{CallID: callID}

	if h := req.GetHeader("Refer-To"); h != nil {
		rec.ReferTo = h.Value()
		rec.TargetURI, rec.Replaces = splitReferTo(h.Value())
		rec.ReplacesID, rec.ToTag, rec.FromTag = parseReplaces(rec.Replaces)
	}
	if h := req.GetHeader("Referred-By"); h != nil {
		rec.ReferredBy = h.Value()
	}

	p.mu.Lock()
	p.records.Refers = append(p.records.Refers, rec)
	dlg := p.dialogs[callID]
	p.mu.Unlock()

	status := p.ReferStatus
	if status == 0 {
		status = sip.StatusAccepted
	}
	_ = tx.Respond(sip.NewResponseFromRequest(req, status, statusReason(status), nil))

	if p.DropDialogAfterRefer && rec.ReplacesID != "" {
		// The replaced dialog is gone as far as this PBX is concerned, and it
		// does not bother telling the client.
		p.mu.Lock()
		delete(p.dialogs, rec.ReplacesID)
		p.mu.Unlock()
	}

	if status != sip.StatusAccepted && status != sip.StatusOK {
		return // rejected outright; no NOTIFY follows
	}
	if dlg == nil || p.SuppressNotify {
		return
	}

	go p.sendReferNotifies(dlg)
}

func (p *PBX) sendReferNotifies(dlg *sipgo.DialogServerSession) {
	target, source := p.clientTarget(dlg)
	if p.NotifyViaContact {
		source = "" // resolve from the Request-URI instead
	}

	send := func(sipfrag, state string) {
		req := sip.NewRequest(sip.NOTIFY, target)
		if source != "" {
			req.SetDestination(source)
		}
		req.AppendHeader(sip.NewHeader("Event", "refer"))
		req.AppendHeader(sip.NewHeader("Subscription-State", state))
		req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
		req.SetBody([]byte(sipfrag))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := dlg.Do(ctx, req); err != nil {
			p.log.Debug("testpbx: NOTIFY failed", "error", err)
			return
		}
		p.mu.Lock()
		p.records.Notifies++
		p.mu.Unlock()
	}

	if p.SendInterimNotify {
		send("SIP/2.0 100 Trying\r\n", "active;expires=60")
		time.Sleep(50 * time.Millisecond)
	}
	if p.NotifyDelay > 0 {
		time.Sleep(p.NotifyDelay)
	}

	outcome := p.ReferOutcome
	if outcome == 0 {
		outcome = sip.StatusOK
	}
	send(fmt.Sprintf("SIP/2.0 %d %s\r\n", outcome, statusReason(outcome)),
		"terminated;reason=noresource")
}

// Invite originates a call toward the client at its registered Contact, which
// is what a real PBX does when routing a call to a registered extension.
func (p *PBX) Invite(ctx context.Context, contact sip.Uri, source string) (*sipgo.DialogClientSession, error) {
	req := sip.NewRequest(sip.INVITE, contact)

	// An explicit transport wins over any ;transport= parameter on the
	// Contact, which is how this simulates a proxy choosing the other one.
	callNet := p.CallNetwork
	if callNet == "" {
		callNet = p.Network
	}
	if callNet != "" {
		req.SetTransport(strings.ToUpper(callNet))
	}
	if source != "" {
		req.SetDestination(source)
	}

	from := &sip.FromHeader{
		DisplayName: "Test Caller",
		Address:     sip.Uri{Scheme: "sip", User: "15551234567", Host: p.Realm},
		Params:      sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(from)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(&p.dialogUA.ContactHDR)

	port := p.rtpPortFor("inbound-" + sip.GenerateTagN(6))
	req.SetBody(p.sdp(port, "sendrecv"))

	dlg, err := p.dialogUA.WriteInvite(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := dlg.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		return dlg, err
	}
	if err := dlg.Ack(ctx); err != nil {
		return dlg, err
	}

	p.mu.Lock()
	p.outbound = append(p.outbound, dlg)
	p.mu.Unlock()
	return dlg, nil
}

// HangupAll ends every call the PBX holds, so the far-end teardown path can be
// exercised (acceptance A14).
func (p *PBX) HangupAll(ctx context.Context) {
	p.mu.Lock()
	server := make([]*sipgo.DialogServerSession, 0, len(p.dialogs))
	for _, d := range p.dialogs {
		server = append(server, d)
	}
	p.dialogs = map[string]*sipgo.DialogServerSession{}
	client := p.outbound
	p.outbound = nil
	p.mu.Unlock()

	for _, d := range server {
		_ = d.Bye(ctx)
	}
	for _, d := range client {
		_ = d.Bye(ctx)
	}
}

// DTMFDigits returns the telephone-event digits observed in inbound RTP,
// counted once per event (acceptance A13).
func (p *PBX) DTMFDigits() []string {
	p.rtpMu.Lock()
	defer p.rtpMu.Unlock()
	return append([]string(nil), p.dtmfDigits...)
}

// rtpPortFor opens a discarding RTP sink and returns its port, so the client's
// media path has somewhere real to send to.
func (p *PBX) rtpPortFor(key string) int {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return 40000
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port

	p.rtpMu.Lock()
	p.rtpConns = append(p.rtpConns, conn)
	p.rtpSeen[port] = 0
	p.rtpMu.Unlock()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			p.rtpMu.Lock()
			p.rtpSeen[port]++
			p.noteDTMFLocked(buf[:n])
			p.rtpMu.Unlock()
			// Echo the packet back so the client sees inbound media too.
			_, _ = conn.WriteToUDP(buf[:n], src)
		}
	}()
	return port
}

// noteDTMFLocked records one telephone-event digit per event. RFC 4733 repeats
// each event across many packets, so the digit is counted when the event ends.
// Caller must hold rtpMu.
func (p *PBX) noteDTMFLocked(pkt []byte) {
	const header = 12
	if len(pkt) < header+4 {
		return
	}
	if pkt[1]&0x7f != 101 { // the telephone-event payload type we advertise
		return
	}

	payload := pkt[header:]
	event, end := payload[0], payload[1]&0x80 != 0

	if !end {
		p.dtmfOpen = true
		return
	}
	if !p.dtmfOpen {
		return // repeated end packet for an event already counted
	}
	p.dtmfOpen = false

	const digits = "0123456789*#ABCD"
	if int(event) < len(digits) {
		p.dtmfDigits = append(p.dtmfDigits, string(digits[event]))
	}
}

func (p *PBX) sdp(port int, dir string) []byte {
	return []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=- 1 1 IN IP4 127.0.0.1"),
		"s=testpbx",
		"c=IN IP4 127.0.0.1",
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP 0 101", port),
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"a=ptime:20",
		"a=" + dir,
		"",
	}, "\r\n"))
}

// mirror returns the direction a peer should answer with.
func mirror(dir string) string {
	switch dir {
	case "sendonly":
		return "recvonly"
	case "recvonly":
		return "sendonly"
	case "inactive":
		return "inactive"
	}
	return "sendrecv"
}

// sdpAddress returns the connection address from an SDP body.
func sdpAddress(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "c=IN IP4 "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func sdpDirection(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		for _, d := range []string{"sendrecv", "sendonly", "recvonly", "inactive"} {
			if line == "a="+d {
				return d
			}
		}
	}
	return ""
}

// splitReferTo separates the target URI from the Replaces parameter and
// percent-decodes the latter.
func splitReferTo(value string) (target, replaces string) {
	v := strings.TrimSpace(value)
	v = strings.TrimPrefix(v, "<")
	v = strings.TrimSuffix(v, ">")

	uri, query, found := strings.Cut(v, "?")
	if !found {
		return uri, ""
	}
	for _, part := range strings.Split(query, "&") {
		k, val, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(k, "Replaces") {
			return uri, percentDecode(val)
		}
	}
	return uri, ""
}

func percentDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseReplaces(v string) (callID, toTag, fromTag string) {
	parts := strings.Split(v, ";")
	if len(parts) == 0 {
		return "", "", ""
	}
	callID = parts[0]
	for _, part := range parts[1:] {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "to-tag":
			toTag = val
		case "from-tag":
			fromTag = val
		}
	}
	return callID, toTag, fromTag
}

func headerValue(h *sip.CallIDHeader) string {
	if h == nil {
		return ""
	}
	return h.Value()
}

func statusReason(code int) string {
	switch code {
	case sip.StatusOK:
		return "OK"
	case sip.StatusAccepted:
		return "Accepted"
	case sip.StatusTrying:
		return "Trying"
	case sip.StatusMethodNotAllowed:
		return "Method Not Allowed"
	case sip.StatusNotImplemented:
		return "Not Implemented"
	case sip.StatusBusyHere:
		return "Busy Here"
	case sip.StatusGlobalDecline:
		return "Decline"
	case sip.StatusForbidden:
		return "Forbidden"
	}
	return "Unknown"
}

// dialogTags reads the tag pair from a freshly created dialog.
func dialogTags(dlg *sipgo.DialogServerSession) (pbxTag, clientTag string) {
	if dlg == nil || dlg.InviteRequest == nil {
		return "", ""
	}
	if to := dlg.InviteRequest.To(); to != nil {
		pbxTag, _ = to.Params.Get("tag")
	}
	if from := dlg.InviteRequest.From(); from != nil {
		clientTag, _ = from.Params.Get("tag")
	}
	return pbxTag, clientTag
}

// DialogTags returns the tags recorded for a call leg: the PBX's own (which a
// Replaces header must carry as to-tag) and the client's (from-tag). Reading
// from the records rather than live dialog state means it still works after a
// successful transfer has torn the dialog down.
func (p *PBX) DialogTags(callID string) (pbxTag, clientTag string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, inv := range p.records.Invites {
		if inv.CallID == callID && inv.PBXTag != "" {
			return inv.PBXTag, inv.ClientTag, inv.ClientTag != ""
		}
	}
	return "", "", false
}
