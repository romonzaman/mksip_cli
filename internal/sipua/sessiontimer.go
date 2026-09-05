package sipua

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// Session timers, RFC 4028.
//
// A session timer makes both ends agree that the call is periodically refreshed
// with a re-INVITE (or UPDATE). If the refresh does not arrive, the session is
// considered dead and torn down.
//
// The failure this prevents is specific and nasty: when the PBX names *us* as
// the refresher and we never refresh, the PBX tears the call down at the
// interval -- typically 30 minutes -- with no error the user can see. A long
// call simply ends.

// Refresher says which end is responsible for sending the refresh.
type Refresher string

const (
	RefresherNone Refresher = ""
	RefresherUAC  Refresher = "uac"
	RefresherUAS  Refresher = "uas"
)

// RFC 4028 §4: the default session interval is 1800s, and 90s is the lowest
// value any implementation is required to accept.
const (
	DefaultSessionExpires = 1800 * time.Second
	AbsoluteMinSE         = 90 * time.Second
)

// SessionTimer is the negotiated outcome for one dialog.
type SessionTimer struct {
	// Interval is the agreed session length. Zero means no session timer was
	// negotiated, and nothing needs refreshing.
	Interval time.Duration
	// Refresher is the end that must send the refresh.
	Refresher Refresher
	// WeRefresh is true when this client is the refresher.
	WeRefresh bool
}

// Active reports whether a session timer was negotiated at all.
func (s SessionTimer) Active() bool { return s.Interval > 0 }

// RefreshAfter is when to send the next refresh: half the interval, as RFC 4028
// §10 recommends, so a lost refresh still leaves time for another attempt.
func (s SessionTimer) RefreshAfter() time.Duration { return s.Interval / 2 }

// ExpiresAfter is how long to wait for the peer's refresh before giving up. The
// grace is deliberate: tearing a live call down a moment early because a packet
// was slow would be worse than the problem being solved.
func (s SessionTimer) ExpiresAfter() time.Duration { return s.Interval + 10*time.Second }

// String renders the timer for logs.
func (s SessionTimer) String() string {
	if !s.Active() {
		return "none"
	}
	who := "peer refreshes"
	if s.WeRefresh {
		who = "we refresh"
	}
	return fmt.Sprintf("%s, %s", s.Interval, who)
}

// ParseSessionExpires reads a Session-Expires header value, for example
// "1800;refresher=uas". The compact form is "x".
func ParseSessionExpires(value string) (time.Duration, Refresher, error) {
	parts := strings.Split(value, ";")
	secs, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, RefresherNone, fmt.Errorf("Session-Expires %q: %w", value, err)
	}
	if secs <= 0 {
		return 0, RefresherNone, fmt.Errorf("Session-Expires %q: must be positive", value)
	}

	refresher := RefresherNone
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		// RFC 3261 allows linear whitespace around the '=' in a parameter, so
		// trim both halves rather than assuming "refresher=uas".
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "refresher") {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "uac":
			refresher = RefresherUAC
		case "uas":
			refresher = RefresherUAS
		}
	}
	return time.Duration(secs) * time.Second, refresher, nil
}

// SessionExpiresValue renders a Session-Expires header value.
func SessionExpiresValue(d time.Duration, r Refresher) string {
	secs := int(d.Seconds())
	if r == RefresherNone {
		return strconv.Itoa(secs)
	}
	return fmt.Sprintf("%d;refresher=%s", secs, r)
}

// sessionExpiresOf reads Session-Expires from a message, honouring the compact
// form "x" that some proxies emit.
func sessionExpiresOf(msg interface{ GetHeader(string) sip.Header }) (time.Duration, Refresher, bool) {
	for _, name := range []string{"Session-Expires", "x"} {
		if h := msg.GetHeader(name); h != nil {
			d, r, err := ParseSessionExpires(h.Value())
			if err != nil {
				return 0, RefresherNone, false
			}
			return d, r, true
		}
	}
	return 0, RefresherNone, false
}

// MinSEOf reads a Min-SE header, returning false when absent.
func MinSEOf(msg interface{ GetHeader(string) sip.Header }) (time.Duration, bool) {
	h := msg.GetHeader("Min-SE")
	if h == nil {
		return 0, false
	}
	secs, err := strconv.Atoi(strings.TrimSpace(h.Value()))
	if err != nil || secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// supportsTimer reports whether a message advertises the timer extension.
func supportsTimer(msg interface{ GetHeaders(string) []sip.Header }) bool {
	for _, name := range []string{"Supported", "Require"} {
		for _, h := range msg.GetHeaders(name) {
			for _, tok := range strings.Split(h.Value(), ",") {
				if strings.EqualFold(strings.TrimSpace(tok), "timer") {
					return true
				}
			}
		}
	}
	return false
}

// SessionTimerConfig is what this client is willing to agree to.
type SessionTimerConfig struct {
	// Expires is the interval we propose. Zero disables session timers, and we
	// then neither offer them nor act as refresher.
	Expires time.Duration
	// MinSE is the shortest interval we will accept from a peer.
	MinSE time.Duration
}

// Enabled reports whether we offer session timers.
func (c SessionTimerConfig) Enabled() bool { return c.Expires > 0 }

// ApplyToInvite adds the session-timer headers to an outgoing INVITE.
//
// We advertise support but do not put "timer" in Require: demanding it would
// fail the call against a PBX that does not implement it, and a call without a
// session timer is still a working call.
func (c SessionTimerConfig) ApplyToInvite(req *sip.Request) {
	req.AppendHeader(sip.NewHeader("Supported", "timer"))
	if !c.Enabled() {
		return
	}
	req.AppendHeader(sip.NewHeader("Session-Expires",
		SessionExpiresValue(c.Expires, RefresherNone)))
	req.AppendHeader(sip.NewHeader("Min-SE", strconv.Itoa(int(c.MinSE.Seconds()))))
}

// FromResponse works out the session timer from a 2xx answer to our INVITE.
//
// The peer decides: if it returns no Session-Expires it does not want one, and
// we must not invent one. When it names no refresher, RFC 4028 §7.1 makes the
// UAC responsible, which is us.
func (c SessionTimerConfig) FromResponse(res *sip.Response) SessionTimer {
	interval, refresher, ok := sessionExpiresOf(res)
	if !ok {
		return SessionTimer{}
	}
	if refresher == RefresherNone {
		refresher = RefresherUAC
	}
	return SessionTimer{
		Interval:  interval,
		Refresher: refresher,
		WeRefresh: refresher == RefresherUAC,
	}
}

// ErrSessionIntervalTooSmall carries a peer's 422 rejection, which names the
// smallest interval it will accept.
type ErrSessionIntervalTooSmall struct{ MinSE time.Duration }

func (e *ErrSessionIntervalTooSmall) Error() string {
	return fmt.Sprintf("peer requires a session interval of at least %s", e.MinSE)
}

// CheckTooSmall converts a 422 response into an error carrying the peer's
// Min-SE, so the caller can retry with an acceptable interval.
func CheckTooSmall(res *sip.Response) error {
	if res.StatusCode != sip.StatusIntervalToBrief {
		return nil
	}
	minSE, ok := MinSEOf(res)
	if !ok || minSE <= 0 {
		minSE = AbsoluteMinSE
	}
	return &ErrSessionIntervalTooSmall{MinSE: minSE}
}

// NegotiateIncoming decides the session timer for an INVITE we are answering,
// and returns the headers to put in the 2xx.
//
// tooSmall is set when the peer's requested interval is below our Min-SE, in
// which case the caller must reject with 422 and our Min-SE rather than answer.
func (c SessionTimerConfig) NegotiateIncoming(req *sip.Request) (timer SessionTimer, headers []sip.Header, tooSmall bool) {
	interval, refresher, present := sessionExpiresOf(req)
	if !present {
		// The peer did not ask for a session timer. Offering one unbidden
		// risks a refresh it will not understand, so leave the session alone.
		return SessionTimer{}, nil, false
	}

	if c.MinSE > 0 && interval < c.MinSE {
		return SessionTimer{}, nil, true
	}

	// If the peer expressed no preference, take the job ourselves: we know we
	// implement it, and we cannot know that the peer does.
	if refresher == RefresherNone {
		refresher = RefresherUAS
	}

	timer = SessionTimer{
		Interval:  interval,
		Refresher: refresher,
		WeRefresh: refresher == RefresherUAS,
	}
	headers = []sip.Header{
		sip.NewHeader("Session-Expires", SessionExpiresValue(interval, refresher)),
		sip.NewHeader("Require", "timer"),
	}
	return timer, headers, false
}

// TooSmallResponse builds the 422 telling a peer the shortest interval we take.
func (c SessionTimerConfig) TooSmallResponse(req *sip.Request) *sip.Response {
	res := sip.NewResponseFromRequest(req, sip.StatusIntervalToBrief,
		"Session Interval Too Small", nil)
	res.AppendHeader(sip.NewHeader("Min-SE", strconv.Itoa(int(c.MinSE.Seconds()))))
	return res
}
