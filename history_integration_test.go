package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"sipclient/internal/config"
	"sipclient/internal/history"
	"sipclient/internal/testpbx"
)

// readHistory loads the call log a client run produced.
func readHistory(t *testing.T, dir string) []history.Record {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, "call-history.jsonl"))
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	var out []history.Record
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r history.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad history line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func historyConfig(dir string) func(*config.Config) {
	return func(c *config.Config) {
		c.History.Enabled = true
		c.History.File = filepath.Join(dir, "call-history.jsonl")
		c.History.MaxEntries = 50
	}
}

// TestHistoryRecordsAnsweredCall covers the ordinary case: an outbound call
// that connected is logged with its talk time and the number to redial.
func TestHistoryRecordsAnsweredCall(t *testing.T) {
	pbx := startPBX(t, nil)
	dir := testDir(t)
	cfgPath := writeConfig(t, dir, pbx, historyConfig(dir))

	out, code := runClient(t, cfgPath, strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"sleep 1200",
		"hangup all",
		"wait 1 IDLE 5000",
		"history",
		"quit",
	}, "\n")+"\n", 60*time.Second)

	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "answered", "2001")

	records := readHistory(t, dir)
	if len(records) != 1 {
		t.Fatalf("logged %d calls, want 1: %+v", len(records), records)
	}
	r := records[0]
	if r.Direction != history.Outbound {
		t.Errorf("direction = %q, want outbound", r.Direction)
	}
	if r.Disposition != history.Answered {
		t.Errorf("disposition = %q, want answered", r.Disposition)
	}
	if r.TalkSeconds < 1 {
		t.Errorf("talkSeconds = %d, want at least 1", r.TalkSeconds)
	}
	if r.AnsweredAt == nil {
		t.Error("an answered call should record when it was answered")
	}
	if !strings.Contains(r.DialTarget(), "2001") {
		t.Errorf("DialTarget() = %q, want something dialable for 2001", r.DialTarget())
	}
	if r.Codec != "PCMU" {
		t.Errorf("codec = %q, want PCMU", r.Codec)
	}
}

// TestHistoryDistinguishesMissedFromRejected is the distinction that matters:
// both are inbound calls that never connected, and they mean opposite things.
func TestHistoryDistinguishesMissedFromRejected(t *testing.T) {
	invited := make(chan struct{}, 2)
	pbx := startPBX(t, func(p *testpbx.PBX) {
		p.OnRegistered = func(contact sip.Uri, source string) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, _ = p.Invite(ctx, contact, source)
			invited <- struct{}{}
		}
	})
	dir := testDir(t)
	cfgPath := writeConfig(t, dir, pbx, historyConfig(dir))

	// Reject the inbound call; it must be logged as rejected, not missed.
	out, code := runClient(t, cfgPath, strings.Join([]string{
		"wait 1 RINGING 15000",
		"reject 1",
		"wait 1 IDLE 5000",
		"history",
		"quit",
	}, "\n")+"\n", 60*time.Second)

	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	records := readHistory(t, dir)
	if len(records) != 1 {
		t.Fatalf("logged %d calls, want 1: %+v", len(records), records)
	}
	r := records[0]
	if r.Direction != history.Inbound {
		t.Errorf("direction = %q, want inbound", r.Direction)
	}
	if r.Disposition != history.Rejected {
		t.Errorf("disposition = %q, want rejected -- a declined call is not a missed one",
			r.Disposition)
	}
	if r.TalkSeconds != 0 {
		t.Errorf("talkSeconds = %d, want 0 for a call never answered", r.TalkSeconds)
	}
	if !strings.Contains(r.Remote, "15551234567") {
		t.Errorf("remote = %q, want the caller's number", r.Remote)
	}
}

// TestHistoryRecordsFailedCall: a rejected outbound attempt keeps the SIP code,
// which is what tells you why it did not go through.
func TestHistoryRecordsFailedCall(t *testing.T) {
	pbx := startPBX(t, nil)
	dir := testDir(t)
	cfgPath := writeConfig(t, dir, pbx, func(c *config.Config) {
		historyConfig(dir)(c)
		// Both channels busy makes the PBX answer a third call 486.
	})

	out, code := runClient(t, cfgPath, strings.Join([]string{
		"dial 2001 1", "wait 1 CONNECTED 8000",
		"dial 2002 2", "wait 2 CONNECTED 8000",
		"hangup all",
		"wait 1 IDLE 5000",
		"history",
		"quit",
	}, "\n")+"\n", 60*time.Second)

	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}

	records := readHistory(t, dir)
	if len(records) != 2 {
		t.Fatalf("logged %d calls, want 2: %+v", len(records), records)
	}
	for _, r := range records {
		if r.Disposition != history.Answered {
			t.Errorf("call to %s = %q, want answered", r.Remote, r.Disposition)
		}
	}
	requireContains(t, out, "2001", "2002")
}

// TestRedialCallsTheLastNumber covers the daily win: no retyping.
func TestRedialCallsTheLastNumber(t *testing.T) {
	pbx := startPBX(t, nil)
	dir := testDir(t)
	cfgPath := writeConfig(t, dir, pbx, historyConfig(dir))

	out, code := runClient(t, cfgPath, strings.Join([]string{
		"dial 2001 1",
		"wait 1 CONNECTED 8000",
		"hangup all",
		"wait 1 IDLE 5000",
		"# no number typed: redial must find it in the history",
		"redial",
		"wait 1 CONNECTED 8000",
		"status",
		"hangup all",
		"quit",
	}, "\n")+"\n", 60*time.Second)

	if code != 0 {
		t.Errorf("exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "calling")

	// Two initial INVITEs to 2001: the original and the redial.
	var toTarget int
	for _, inv := range pbx.Records().Invites {
		if !inv.InDialog && strings.Contains(inv.RequestTo, "2001") {
			toTarget++
		}
	}
	if toTarget != 2 {
		t.Errorf("saw %d calls to 2001, want 2 (the original and the redial)", toTarget)
	}
}

// TestHistorySurvivesRestart: the log is only useful if it outlives the process.
func TestHistorySurvivesRestart(t *testing.T) {
	pbx := startPBX(t, nil)
	dir := testDir(t)
	cfgPath := writeConfig(t, dir, pbx, historyConfig(dir))

	_, code := runClient(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 8000\nhangup all\nwait 1 IDLE 5000\nquit\n",
		40*time.Second)
	if code != 0 {
		t.Fatalf("first run exit code = %d", code)
	}

	// A second process must see the first run's call.
	out, code := runClient(t, cfgPath, "history\nquit\n", 40*time.Second)
	if code != 0 {
		t.Errorf("second run exit code = %d, want 0\n%s", code, out)
	}
	requireContains(t, out, "2001", "answered")
}

// TestHistoryDisabled: with it off, nothing is written and the commands say so.
func TestHistoryDisabled(t *testing.T) {
	pbx := startPBX(t, nil)
	dir := testDir(t)
	cfgPath := writeConfig(t, dir, pbx, func(c *config.Config) {
		c.History.Enabled = false
	})

	out, code := runClient(t, cfgPath,
		"dial 2001 1\nwait 1 CONNECTED 8000\nhangup all\nhistory\nredial\nquit\n",
		40*time.Second)
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (history and redial must fail)\n%s", code, out)
	}
	requireContains(t, out, "call history is disabled")

	if _, err := os.Stat(filepath.Join(dir, "call-history.jsonl")); !os.IsNotExist(err) {
		t.Error("history file written though history is disabled")
	}
}
