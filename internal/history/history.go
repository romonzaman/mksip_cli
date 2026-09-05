// Package history records completed calls so the operator can see who rang and
// redial without retyping a URI.
//
// Records are held in memory newest-first and mirrored to a JSON Lines file, so
// history survives a restart. The file is rewritten atomically on each new
// record: calls are infrequent enough that this costs nothing, and it avoids
// the append-then-compact dance and its half-written-line failure mode.
package history

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Direction says which way the call went.
type Direction string

const (
	Inbound  Direction = "in"
	Outbound Direction = "out"
)

// Disposition is how the call ended. Missed is the one people actually look
// for, so it is distinguished from rejected and failed rather than lumped in.
type Disposition string

const (
	// Answered: the call connected and media flowed.
	Answered Disposition = "answered"
	// Missed: an inbound call that rang and was never answered here.
	Missed Disposition = "missed"
	// Rejected: an inbound call this client declined.
	Rejected Disposition = "rejected"
	// Cancelled: an outbound call hung up before it was answered.
	Cancelled Disposition = "cancelled"
	// Failed: the call could not be set up, with a SIP code to say why.
	Failed Disposition = "failed"
)

// Record is one completed call.
type Record struct {
	ID          string      `json:"id"`
	Direction   Direction   `json:"direction"`
	Disposition Disposition `json:"disposition"`

	// Remote is the display form; RemoteURI is what redial dials.
	Remote    string `json:"remote"`
	RemoteURI string `json:"remoteUri,omitempty"`

	Channel    int        `json:"channel"`
	StartedAt  time.Time  `json:"startedAt"`
	EndedAt    time.Time  `json:"endedAt"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`

	// TalkSeconds is time connected, not time ringing.
	TalkSeconds int    `json:"talkSeconds"`
	Codec       string `json:"codec,omitempty"`

	// Code and Reason carry the SIP failure, when there was one.
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Answered reports whether the call connected.
func (r Record) WasAnswered() bool { return r.Disposition == Answered }

// DialTarget is what redialling this record should call. It prefers the URI,
// falling back to the display form when no URI was captured.
func (r Record) DialTarget() string {
	if r.RemoteURI != "" {
		return r.RemoteURI
	}
	return r.Remote
}

// Store holds the call history.
type Store struct {
	mu      sync.RWMutex
	path    string
	max     int
	records []Record // newest first
}

// DefaultMax is how many records are kept when none is configured.
const DefaultMax = 200

// Open loads existing history from path, or starts empty when the file is
// absent. A corrupt line is skipped rather than failing startup: losing one
// history entry must never stop the phone from working.
func Open(path string, max int) (*Store, error) {
	if max <= 0 {
		max = DefaultMax
	}
	s := &Store{path: path, max: max}

	if path == "" {
		return s, nil // memory only
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("history: read %s: %w", path, err)
	}

	// A client killed mid-write leaves its temporary file behind. Sweep the
	// stale ones so they cannot accumulate in the working directory forever.
	cleanStaleTemps(filepath.Dir(path))

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // skip the bad line, keep the rest
		}
		s.records = append(s.records, r)
	}
	// The file is written oldest-first so it reads chronologically; the store
	// keeps newest-first, so flip it.
	for i, j := 0, len(s.records)-1; i < j; i, j = i+1, j-1 {
		s.records[i], s.records[j] = s.records[j], s.records[i]
	}
	s.trim()
	return s, nil
}

// tempPrefix names our atomic-write temporary files.
const tempPrefix = ".history-"

// staleTempAge is how old a temporary file must be before it is assumed
// abandoned. Long enough that a second client writing right now is never
// mistaken for a corpse.
const staleTempAge = 5 * time.Minute

// cleanStaleTemps removes temporary files left by a client that was killed
// before it could rename one into place.
func cleanStaleTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleTempAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// Add records a call and persists the history.
func (s *Store) Add(r Record) error {
	if r.ID == "" {
		r.ID = newID()
	}
	if r.EndedAt.IsZero() {
		r.EndedAt = time.Now()
	}

	s.mu.Lock()
	s.records = append([]Record{r}, s.records...)
	s.trim()
	path := s.path
	snapshot := make([]Record, len(s.records))
	copy(snapshot, s.records)
	s.mu.Unlock()

	if path == "" {
		return nil
	}
	return writeAtomic(path, snapshot)
}

// Recent returns the newest n records, or all of them when n <= 0.
func (s *Store) Recent(n int) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if n <= 0 || n > len(s.records) {
		n = len(s.records)
	}
	out := make([]Record, n)
	copy(out, s.records[:n])
	return out
}

// LastDialled returns the most recent outbound call, for redial.
func (s *Store) LastDialled() (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, r := range s.records {
		if r.Direction == Outbound {
			return r, true
		}
	}
	return Record{}, false
}

// Get returns the nth most recent record, 1-based.
func (s *Store) Get(n int) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if n < 1 || n > len(s.records) {
		return Record{}, false
	}
	return s.records[n-1], true
}

// Len is the number of records held.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records)
}

// Clear discards all history.
func (s *Store) Clear() error {
	s.mu.Lock()
	s.records = nil
	path := s.path
	s.mu.Unlock()

	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("history: clear %s: %w", path, err)
	}
	return nil
}

// trim caps the record count. Caller must hold the write lock.
func (s *Store) trim() {
	if len(s.records) > s.max {
		s.records = s.records[:s.max]
	}
}

// writeAtomic replaces the file via a temporary file and a rename, so a crash
// mid-write cannot leave truncated history behind.
func writeAtomic(path string, records []Record) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("history: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	enc := json.NewEncoder(tmp)
	// Oldest first on disk, so the file reads chronologically.
	for i := len(records) - 1; i >= 0; i-- {
		if err := enc.Encode(records[i]); err != nil {
			tmp.Close()
			return fmt.Errorf("history: encode: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("history: close temp: %w", err)
	}
	// History can contain who you called; keep it to the owner.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("history: chmod: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("history: replace %s: %w", path, err)
	}
	return nil
}

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
