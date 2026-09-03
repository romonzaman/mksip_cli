package sdputil

import (
	"strings"
	"testing"

	"sipclient/internal/media"
)

func TestBuildParseRoundTrip(t *testing.T) {
	body, err := Build(Offer{
		Address:     "192.168.1.50",
		Port:        16000,
		Codecs:      []media.Codec{media.CodecPCMU, media.CodecPCMA},
		DTMFPayload: media.DTMFPayloadType,
		PtimeMS:     20,
		Direction:   SendRecv,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Logf("offer:\n%s", body)

	for _, want := range []string{
		"m=audio 16000 RTP/AVP 0 8 101",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"a=ptime:20",
		"a=sendrecv",
		"c=IN IP4 192.168.1.50",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("offer missing %q", want)
		}
	}

	d, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Address != "192.168.1.50" || d.Port != 16000 {
		t.Errorf("got %s:%d", d.Address, d.Port)
	}
	if d.Direction != SendRecv || d.PtimeMS != 20 {
		t.Errorf("got dir=%s ptime=%d", d.Direction, d.PtimeMS)
	}
	if !d.HasDTMF || d.DTMFPayload != 101 {
		t.Errorf("dtmf not parsed: %+v", d)
	}

	c, err := Negotiate([]media.Codec{media.CodecPCMA, media.CodecPCMU}, d)
	if err != nil || c.Name != "PCMA" {
		t.Errorf("Negotiate = %v, %v; want PCMA (our preference order wins)", c.Name, err)
	}
}

// A real Asterisk answer, to prove we parse what a PBX actually sends.
func TestParseAsteriskAnswer(t *testing.T) {
	body := "v=0\r\n" +
		"o=- 1234 2 IN IP4 10.0.0.10\r\n" +
		"s=Asterisk\r\n" +
		"c=IN IP4 10.0.0.10\r\n" +
		"t=0 0\r\n" +
		"m=audio 14002 RTP/AVP 0 101\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\n" +
		"a=fmtp:101 0-16\r\n" +
		"a=ptime:20\r\n" +
		"a=sendonly\r\n"

	d, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Port != 14002 || d.Address != "10.0.0.10" {
		t.Errorf("got %s:%d", d.Address, d.Port)
	}
	if d.Direction != SendOnly {
		t.Errorf("direction = %s, want sendonly", d.Direction)
	}
	// Peer is sendonly, so our hold must become inactive, not sendonly.
	if got := HeldByUs(d.Direction); got != Inactive {
		t.Errorf("HeldByUs(sendonly) = %s, want inactive", got)
	}
	if _, err := Negotiate([]media.Codec{media.CodecPCMA}, d); err == nil {
		t.Error("expected no-common-codec error for PCMA-only against PCMU offer")
	}
}
