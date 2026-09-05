package sipua

import (
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestParseSessionExpires(t *testing.T) {
	tests := []struct {
		value     string
		wantSecs  int
		wantRefr  Refresher
		wantError bool
	}{
		{"1800", 1800, RefresherNone, false},
		{"1800;refresher=uas", 1800, RefresherUAS, false},
		{"1800;refresher=uac", 1800, RefresherUAC, false},
		{" 900 ; refresher = UAS ", 900, RefresherUAS, false},
		{"1800;other=x;refresher=uas", 1800, RefresherUAS, false},
		{"notanumber", 0, RefresherNone, true},
		{"0", 0, RefresherNone, true},
		{"-5", 0, RefresherNone, true},
	}

	for _, tc := range tests {
		d, r, err := ParseSessionExpires(tc.value)
		if tc.wantError {
			if err == nil {
				t.Errorf("ParseSessionExpires(%q) = %v, %v; want an error", tc.value, d, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSessionExpires(%q): %v", tc.value, err)
			continue
		}
		if int(d.Seconds()) != tc.wantSecs || r != tc.wantRefr {
			t.Errorf("ParseSessionExpires(%q) = %v, %q; want %ds, %q",
				tc.value, d, r, tc.wantSecs, tc.wantRefr)
		}
	}
}

func TestSessionExpiresValueRoundTrip(t *testing.T) {
	for _, r := range []Refresher{RefresherNone, RefresherUAC, RefresherUAS} {
		v := SessionExpiresValue(1800*time.Second, r)
		d, got, err := ParseSessionExpires(v)
		if err != nil {
			t.Fatalf("round trip of %q: %v", v, err)
		}
		if d != 1800*time.Second {
			t.Errorf("interval = %v, want 1800s", d)
		}
		if got != r {
			t.Errorf("refresher = %q, want %q (from %q)", got, r, v)
		}
	}
}

// TestFromResponseDefaultsRefresherToUs covers RFC 4028 §7.1: when the answer
// names no refresher, the UAC is responsible. Getting this backwards is exactly
// how a call dies silently at the interval.
func TestFromResponseDefaultsRefresherToUs(t *testing.T) {
	cfg := SessionTimerConfig{Expires: 1800 * time.Second, MinSE: 90 * time.Second}

	res := sip.NewResponse(200, "OK")
	res.AppendHeader(sip.NewHeader("Session-Expires", "1800"))

	st := cfg.FromResponse(res)
	if !st.Active() {
		t.Fatal("expected an active session timer")
	}
	if !st.WeRefresh {
		t.Error("with no refresher named, the UAC (us) must refresh")
	}
	if st.RefreshAfter() != 900*time.Second {
		t.Errorf("RefreshAfter() = %v, want half the interval", st.RefreshAfter())
	}
}

func TestFromResponseHonoursNamedRefresher(t *testing.T) {
	cfg := SessionTimerConfig{Expires: 1800 * time.Second, MinSE: 90 * time.Second}

	res := sip.NewResponse(200, "OK")
	res.AppendHeader(sip.NewHeader("Session-Expires", "600;refresher=uas"))

	st := cfg.FromResponse(res)
	if st.WeRefresh {
		t.Error("peer named itself refresher; we must not refresh")
	}
	if st.Interval != 600*time.Second {
		t.Errorf("interval = %v, want 600s", st.Interval)
	}
	// We must still expect the peer's refresh, with a little grace.
	if st.ExpiresAfter() <= st.Interval {
		t.Error("ExpiresAfter must allow grace beyond the interval")
	}
}

// TestNoSessionExpiresMeansNoTimer: a peer that does not want session timers
// must not get refreshes it never asked for.
func TestNoSessionExpiresMeansNoTimer(t *testing.T) {
	cfg := SessionTimerConfig{Expires: 1800 * time.Second, MinSE: 90 * time.Second}

	if st := cfg.FromResponse(sip.NewResponse(200, "OK")); st.Active() {
		t.Error("no Session-Expires in the answer must mean no session timer")
	}

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "1001", Host: "pbx"})
	timer, headers, tooSmall := cfg.NegotiateIncoming(req)
	if timer.Active() || headers != nil || tooSmall {
		t.Error("an INVITE without Session-Expires must negotiate no timer")
	}
}

func TestNegotiateIncoming(t *testing.T) {
	cfg := SessionTimerConfig{Expires: 1800 * time.Second, MinSE: 90 * time.Second}

	// Peer asks, names no refresher: we take the job.
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "1001", Host: "pbx"})
	req.AppendHeader(sip.NewHeader("Session-Expires", "600"))

	timer, headers, tooSmall := cfg.NegotiateIncoming(req)
	if tooSmall {
		t.Fatal("600s is above our 90s Min-SE")
	}
	if !timer.WeRefresh {
		t.Error("with no refresher named on an inbound INVITE, we should refresh")
	}
	if len(headers) != 2 {
		t.Fatalf("expected Session-Expires and Require headers, got %d", len(headers))
	}
	if got := headers[0].Value(); got != "600;refresher=uas" {
		t.Errorf("Session-Expires = %q, want 600;refresher=uas", got)
	}

	// Peer asks for less than we accept: must be told, not silently accepted.
	tooSmallReq := sip.NewRequest(sip.INVITE, sip.Uri{User: "1001", Host: "pbx"})
	tooSmallReq.AppendHeader(sip.NewHeader("Session-Expires", "30"))
	if _, _, tooSmall := cfg.NegotiateIncoming(tooSmallReq); !tooSmall {
		t.Error("30s is below our 90s Min-SE and must be rejected")
	}

	res := cfg.TooSmallResponse(tooSmallReq)
	if res.StatusCode != sip.StatusIntervalToBrief {
		t.Errorf("status = %d, want 422", res.StatusCode)
	}
	if h := res.GetHeader("Min-SE"); h == nil || h.Value() != "90" {
		t.Error("the 422 must carry our Min-SE so the peer can retry")
	}
}

// TestCheckTooSmall covers the retry path: a 422 must surface the peer's Min-SE.
func TestCheckTooSmall(t *testing.T) {
	ok := sip.NewResponse(200, "OK")
	if err := CheckTooSmall(ok); err != nil {
		t.Errorf("a 200 must not be an interval error: %v", err)
	}

	res := sip.NewResponse(sip.StatusIntervalToBrief, "Session Interval Too Small")
	res.AppendHeader(sip.NewHeader("Min-SE", "1200"))

	err := CheckTooSmall(res)
	if err == nil {
		t.Fatal("a 422 must produce an error")
	}
	tooSmall, ok2 := err.(*ErrSessionIntervalTooSmall)
	if !ok2 {
		t.Fatalf("error type = %T, want *ErrSessionIntervalTooSmall", err)
	}
	if tooSmall.MinSE != 1200*time.Second {
		t.Errorf("MinSE = %v, want 1200s", tooSmall.MinSE)
	}

	// A 422 with no Min-SE still has to yield something usable.
	bare := sip.NewResponse(sip.StatusIntervalToBrief, "Session Interval Too Small")
	err = CheckTooSmall(bare)
	if err == nil {
		t.Fatal("a bare 422 must still produce an error")
	}
	if err.(*ErrSessionIntervalTooSmall).MinSE != AbsoluteMinSE {
		t.Errorf("a 422 without Min-SE should fall back to %v", AbsoluteMinSE)
	}
}

func TestApplyToInvite(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "1001", Host: "pbx"})
	SessionTimerConfig{Expires: 1800 * time.Second, MinSE: 90 * time.Second}.ApplyToInvite(req)

	if h := req.GetHeader("Session-Expires"); h == nil || h.Value() != "1800" {
		t.Error("INVITE should offer Session-Expires")
	}
	if h := req.GetHeader("Min-SE"); h == nil || h.Value() != "90" {
		t.Error("INVITE should carry Min-SE")
	}
	if !supportsTimer(req) {
		t.Error("INVITE should advertise Supported: timer")
	}
	// Requiring timer would fail the call against a PBX that lacks it.
	for _, h := range req.GetHeaders("Require") {
		if h.Value() == "timer" {
			t.Error("timer must be advertised as Supported, never Required")
		}
	}

	// Disabled: advertise support but propose nothing.
	off := sip.NewRequest(sip.INVITE, sip.Uri{User: "1001", Host: "pbx"})
	SessionTimerConfig{}.ApplyToInvite(off)
	if off.GetHeader("Session-Expires") != nil {
		t.Error("a disabled session timer must not propose an interval")
	}
}
