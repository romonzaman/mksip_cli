package sipua

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// RegState is the registration state shown in the status line (FR-2.6).
type RegState int

const (
	RegUnregistered RegState = iota
	RegRegistering
	RegRegistered
	RegFailed // gave up; needs an explicit `register`
)

func (s RegState) String() string {
	switch s {
	case RegRegistering:
		return "registering"
	case RegRegistered:
		return "registered"
	case RegFailed:
		return "failed"
	}
	return "unregistered"
}

// Registration is a snapshot of registration status.
type Registration struct {
	State       RegState
	GrantedFor  time.Duration
	NextRefresh time.Time
	LastError   string
	LastCode    int
	Attempts    int
}

// maxTerminalAttempts is how many times a terminal rejection (403/404) is
// retried before the loop parks and waits for an explicit register (FR-2.4).
const maxTerminalAttempts = 3

type registrar struct {
	ua *UA

	mu    sync.RWMutex
	state Registration

	trigger chan struct{}
	// callID is reused across refreshes, as RFC 3261 §10.2 requires.
	callID string
	cseq   uint32
}

func (r *registrar) init(u *UA) {
	r.ua = u
	r.trigger = make(chan struct{}, 1)
	r.callID = sip.GenerateTagN(20)
	r.cseq = uint32(rand.IntN(10000)) + 1
}

// Registration returns the current status.
func (u *UA) Registration() Registration {
	u.reg.mu.RLock()
	defer u.reg.mu.RUnlock()
	return u.reg.state
}

// Registered reports whether we currently hold a registration (FR-2.7).
func (u *UA) Registered() bool {
	return u.Registration().State == RegRegistered
}

func (r *registrar) set(mutate func(*Registration)) {
	r.mu.Lock()
	mutate(&r.state)
	r.mu.Unlock()
}

// TriggerRegister asks the registration loop to try again now (FR-2.4).
func (u *UA) TriggerRegister() {
	select {
	case u.reg.trigger <- struct{}{}:
	default:
	}
}

// RunRegistration keeps the client registered until ctx is done (FR-2.2, FR-2.3).
func (u *UA) RunRegistration(ctx context.Context) {
	r := &u.reg
	// parked is the wait used when we have given up: effectively forever, until
	// the operator triggers a retry.
	const parked = 24 * time.Hour

	for {
		granted, code, err := r.attempt(ctx)

		var wait time.Duration
		switch {
		case err == nil:
			refresh := granted / 2
			if refresh < 30*time.Second {
				refresh = 30 * time.Second
			}
			r.set(func(s *Registration) {
				*s = Registration{
					State:       RegRegistered,
					GrantedFor:  granted,
					NextRefresh: time.Now().Add(refresh),
				}
			})
			u.log.Info("registered",
				"user", u.cfg.SIP.Username,
				"domain", u.cfg.SIP.Domain,
				"aor", u.cfg.AOR(),
				"server", u.cfg.ServerAddr(),
				"transport", u.cfg.SIP.Server.Transport,
				"contact", u.ContactURI(),
				"expires", granted.String(),
				"refresh_in", refresh.String())
			wait = refresh

		default:
			terminal := isTerminal(code)
			var attempts int
			r.set(func(s *Registration) {
				s.Attempts++
				attempts = s.Attempts
				s.LastError = err.Error()
				s.LastCode = code
				s.State = RegRegistering
			})

			if terminal && attempts >= maxTerminalAttempts {
				r.set(func(s *Registration) { s.State = RegFailed })
				u.log.Error("registration rejected, giving up",
					"code", code, "error", err.Error(),
					"hint", "fix credentials then run `register`")
				wait = parked
			} else {
				wait = backoff(attempts)
				u.log.Warn("registration failed, retrying",
					"code", code, "error", err.Error(), "retry_in", wait.String())
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-r.trigger:
			r.set(func(s *Registration) { s.Attempts = 0 })
		case <-time.After(wait):
		}
	}
}

// isTerminal reports whether the registrar's rejection is worth retrying.
// A wrong password or unknown user will not fix itself (FR-2.4).
func isTerminal(code int) bool {
	switch code {
	case sip.StatusForbidden, sip.StatusNotFound, sip.StatusUnauthorized,
		sip.StatusProxyAuthRequired:
		return true
	}
	return false
}

// backoff is 2s, 4s, 8s ... capped at 60s with jitter (FR-2.3).
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(1<<min(attempt, 6)) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	jitter := time.Duration(rand.Int64N(int64(d / 4)))
	return d/2 + jitter + d/2
}

// attempt sends one REGISTER, authenticating if challenged.
func (r *registrar) attempt(ctx context.Context) (granted time.Duration, code int, err error) {
	r.set(func(s *Registration) {
		if s.State != RegRegistered {
			s.State = RegRegistering
		}
	})
	return r.send(ctx, r.ua.cfg.SIP.RegisterExpirySeconds)
}

// Unregister sends REGISTER with Expires: 0 (FR-2.5).
func (u *UA) Unregister(ctx context.Context) error {
	_, _, err := u.reg.send(ctx, 0)
	u.reg.set(func(s *Registration) { *s = Registration{State: RegUnregistered} })
	return err
}

// send performs one REGISTER transaction with the given expiry.
func (r *registrar) send(ctx context.Context, expiry int) (time.Duration, int, error) {
	u := r.ua

	registrarURI := sip.Uri{Scheme: "sip", Host: u.cfg.SIP.Domain}
	req := sip.NewRequest(sip.REGISTER, registrarURI)
	req.SetTransport(strings.ToUpper(u.cfg.SIP.Server.Transport))
	req.SetDestination(u.RouteDestination())

	aor := u.AORUri()
	from := &sip.FromHeader{
		DisplayName: u.cfg.SIP.DisplayName,
		Address:     aor,
		Params:      sip.NewParams(),
	}
	from.Params.Add("tag", sip.GenerateTagN(16))
	to := &sip.ToHeader{Address: aor}

	callID := sip.CallIDHeader(r.callID)
	r.cseq++
	cseq := &sip.CSeqHeader{SeqNo: r.cseq, MethodName: sip.REGISTER}

	contact := u.contact
	req.AppendHeader(from)
	req.AppendHeader(to)
	req.AppendHeader(&callID)
	req.AppendHeader(cseq)
	req.AppendHeader(&contact)
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(expiry)))
	req.AppendHeader(sip.NewHeader("Allow",
		"INVITE, ACK, CANCEL, BYE, OPTIONS, INFO, NOTIFY, REFER"))

	res, err := u.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return 0, 0, fmt.Errorf("REGISTER: %w", err)
	}

	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		res, err = u.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: u.cfg.SIP.AuthUsername,
			Password: u.cfg.SIP.Password,
		})
		if err != nil {
			return 0, 0, fmt.Errorf("REGISTER auth: %w", err)
		}
		// DoDigestAuth bumped the CSeq on the request; stay in step for the
		// next refresh so the registrar never sees a replayed sequence.
		if c := req.CSeq(); c != nil {
			r.cseq = c.SeqNo
		}
	}

	if res.StatusCode != sip.StatusOK {
		return 0, int(res.StatusCode), fmt.Errorf("REGISTER rejected: %d %s",
			res.StatusCode, res.Reason)
	}
	if expiry == 0 {
		return 0, sip.StatusOK, nil
	}
	return grantedExpiry(res, expiry), sip.StatusOK, nil
}

// grantedExpiry reads the expiry the server actually granted, preferring the
// expires parameter on our Contact and falling back to the Expires header. We
// refresh on that value, not the one we asked for (FR-2.2).
func grantedExpiry(res *sip.Response, requested int) time.Duration {
	for _, h := range res.GetHeaders("contact") {
		var uri sip.Uri
		params := sip.NewParams()
		if _, err := sip.ParseAddressValue(h.Value(), &uri, &params); err == nil {
			if v, ok := params.Get("expires"); ok {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					return time.Duration(n) * time.Second
				}
			}
			continue
		}
		// Fall back to a raw scan when the header does not parse.
		if v, ok := expiresParam(h.Value()); ok && v > 0 {
			return time.Duration(v) * time.Second
		}
	}
	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(requested) * time.Second
}

func expiresParam(value string) (int, bool) {
	for _, part := range strings.Split(value, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "expires") {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}
