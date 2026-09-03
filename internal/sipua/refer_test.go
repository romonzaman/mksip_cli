package sipua

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestReferToWithReplaces(t *testing.T) {
	info := DialogInfo{
		CallID:    "abc123@10.0.0.5",
		LocalTag:  "ourtag99",
		RemoteTag: "theirtagAA",
	}
	got := info.ReplacesValue()
	want := "abc123@10.0.0.5;to-tag=theirtagAA;from-tag=ourtag99"
	if got != want {
		t.Fatalf("ReplacesValue()\n got %q\nwant %q", got, want)
	}

	target := sip.Uri{Scheme: "sip", User: "2002", Host: "pbx.example.com"}
	hdr := ReferTo(target, got)
	wantHdr := "<sip:2002@pbx.example.com?Replaces=abc123%40" +
		"10.0.0.5%3Bto-tag%3DtheirtagAA%3Bfrom-tag%3Dourtag99>"
	if hdr != wantHdr {
		t.Fatalf("ReferTo()\n got %s\nwant %s", hdr, wantHdr)
	}

	// Blind transfer carries no Replaces.
	if plain := ReferTo(target, ""); plain != "<sip:2002@pbx.example.com>" {
		t.Errorf("blind ReferTo = %s", plain)
	}
}

func TestSipfragStatus(t *testing.T) {
	for _, tc := range []struct {
		body string
		code int
	}{
		{"SIP/2.0 100 Trying\r\n", 100},
		{"SIP/2.0 200 OK\r\n", 200},
		{"SIP/2.0 486 Busy Here\r\n", 486},
	} {
		code, _, ok := SipfragStatus([]byte(tc.body))
		if !ok || code != tc.code {
			t.Errorf("SipfragStatus(%q) = %d, %v", tc.body, code, ok)
		}
	}
	if _, _, ok := SipfragStatus([]byte("garbage")); ok {
		t.Error("expected no status from garbage body")
	}
}
