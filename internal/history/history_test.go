package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rec(dir Direction, disp Disposition, remote string) Record {
	return Record{
		Direction: dir, Disposition: disp,
		Remote: remote, RemoteURI: "sip:" + remote + "@pbx",
		StartedAt: time.Now(), EndedAt: time.Now(),
	}
}

func TestNewestFirst(t *testing.T) {
	s, err := Open("", 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{"1001", "1002", "1003"} {
		if err := s.Add(rec(Outbound, Answered, n)); err != nil {
			t.Fatal(err)
		}
	}

	got := s.Recent(0)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	if got[0].Remote != "1003" {
		t.Errorf("newest is %q, want the most recent call 1003", got[0].Remote)
	}
	if got[2].Remote != "1001" {
		t.Errorf("oldest is %q, want 1001", got[2].Remote)
	}

	// Recent(n) truncates from the newest end.
	if two := s.Recent(2); len(two) != 2 || two[0].Remote != "1003" {
		t.Errorf("Recent(2) = %+v", two)
	}
}

// TestCapDiscardsOldest keeps the file from growing without bound.
func TestCapDiscardsOldest(t *testing.T) {
	s, _ := Open("", 3)
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		_ = s.Add(rec(Outbound, Answered, n))
	}

	if s.Len() != 3 {
		t.Fatalf("held %d records, want the cap of 3", s.Len())
	}
	got := s.Recent(0)
	if got[0].Remote != "5" || got[2].Remote != "3" {
		t.Errorf("cap kept the wrong records: %q..%q", got[0].Remote, got[2].Remote)
	}
}

// TestPersistsAcrossRestart is the whole point of writing a file.
func TestPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "call-history.jsonl")

	s, err := Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Add(rec(Inbound, Missed, "5551234"))
	_ = s.Add(rec(Outbound, Answered, "1002"))

	reopened, err := Open(path, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Recent(0)
	if len(got) != 2 {
		t.Fatalf("after restart got %d records, want 2", len(got))
	}
	if got[0].Remote != "1002" || got[0].Direction != Outbound {
		t.Errorf("newest after restart = %+v", got[0])
	}
	if got[1].Disposition != Missed {
		t.Errorf("missed call not preserved: %+v", got[1])
	}
}

// TestHistoryFileIsPrivate: it records who you called.
func TestHistoryFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "call-history.jsonl")
	s, _ := Open(path, 10)
	_ = s.Add(rec(Outbound, Answered, "1002"))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("history file mode is %o, want owner-only", perm)
	}
}

// TestCorruptLineIsSkipped: a damaged entry must not stop the phone starting.
func TestCorruptLineIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "call-history.jsonl")
	body := `{"id":"a","direction":"out","remote":"1001"}
this is not json
{"id":"b","direction":"in","remote":"1002"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, 10)
	if err != nil {
		t.Fatalf("a corrupt line must not fail Open: %v", err)
	}
	if s.Len() != 2 {
		t.Errorf("kept %d records, want the 2 readable ones", s.Len())
	}
}

func TestLastDialledSkipsInbound(t *testing.T) {
	s, _ := Open("", 10)
	_ = s.Add(rec(Outbound, Answered, "1001"))
	_ = s.Add(rec(Inbound, Missed, "5551234")) // newer, but not dialled

	last, ok := s.LastDialled()
	if !ok {
		t.Fatal("expected an outbound record")
	}
	if last.Remote != "1001" {
		t.Errorf("LastDialled = %q, want the last call we placed", last.Remote)
	}

	// With no outbound calls at all there is nothing to redial.
	empty, _ := Open("", 10)
	_ = empty.Add(rec(Inbound, Missed, "5551234"))
	if _, ok := empty.LastDialled(); ok {
		t.Error("LastDialled should be empty when nothing was ever dialled")
	}
}

func TestGetIsOneBased(t *testing.T) {
	s, _ := Open("", 10)
	_ = s.Add(rec(Outbound, Answered, "1001"))
	_ = s.Add(rec(Outbound, Answered, "1002"))

	if r, ok := s.Get(1); !ok || r.Remote != "1002" {
		t.Errorf("Get(1) = %+v, want the newest", r)
	}
	if _, ok := s.Get(0); ok {
		t.Error("Get(0) should be out of range")
	}
	if _, ok := s.Get(99); ok {
		t.Error("Get(99) should be out of range")
	}
}

func TestDialTargetPrefersURI(t *testing.T) {
	withURI := Record{Remote: "Alice", RemoteURI: "sip:1001@pbx"}
	if got := withURI.DialTarget(); got != "sip:1001@pbx" {
		t.Errorf("DialTarget() = %q, want the URI", got)
	}
	bare := Record{Remote: "1001"}
	if got := bare.DialTarget(); got != "1001" {
		t.Errorf("DialTarget() = %q, want the display form as fallback", got)
	}
}

func TestClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "call-history.jsonl")
	s, _ := Open(path, 10)
	_ = s.Add(rec(Outbound, Answered, "1001"))

	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d after Clear", s.Len())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Clear should remove the file")
	}
	// Clearing twice must not error.
	if err := s.Clear(); err != nil {
		t.Errorf("second Clear: %v", err)
	}
}
