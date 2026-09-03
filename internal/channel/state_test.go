package channel

import (
	"strings"
	"testing"
	"time"
)

// TestSnapshotLabel pins the status-line format from FR-9.2.
func TestSnapshotLabel(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want string
	}{
		{
			name: "idle",
			snap: Snapshot{ID: 1, State: Idle},
			want: "[1]  IDLE",
		},
		{
			name: "active outbound connected",
			snap: Snapshot{
				ID: 2, State: Connected, Remote: "2002", Active: true,
				Duration: 18 * time.Second,
			},
			want: "[2]* CONNECTED → 2002  00:18",
		},
		{
			name: "held inbound shows caller and no active marker",
			snap: Snapshot{
				ID: 1, State: Held, Remote: "+15551234567", Inbound: true,
				Duration: 151 * time.Second,
			},
			want: "[1]  HELD ← +15551234567  02:31",
		},
		{
			name: "remote hold and mute are called out",
			snap: Snapshot{
				ID: 1, State: Connected, Remote: "2001", Active: true,
				Duration: time.Second, RemoteHeld: true, Muted: true,
			},
			want: "[1]* CONNECTED → 2001  00:01 (remote hold) (muted)",
		},
		{
			name: "ringing has no duration",
			snap: Snapshot{ID: 2, State: Ringing, Remote: "2003"},
			want: "[2]  RINGING → 2003",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.Label(); got != tc.want {
				t.Errorf("Label()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestStateErrorMessage checks that refusals name the state, per FR-9.6.
func TestStateErrorMessage(t *testing.T) {
	err := &StateError{Channel: 2, State: Idle, Action: "hold"}
	if got := err.Error(); got != "channel 2 is IDLE, cannot hold" {
		t.Errorf("Error() = %q", got)
	}

	err = &StateError{Channel: 1, State: Calling, Action: "dial", Detail: "hang up first"}
	if got := err.Error(); !strings.Contains(got, "hang up first") {
		t.Errorf("Error() = %q, want the detail included", got)
	}
}

// TestStatePredicates guards the state machine helpers the manager relies on.
func TestStatePredicates(t *testing.T) {
	inCall := map[State]bool{Connected: true, Held: true, Transferring: true}
	busy := map[State]bool{
		Calling: true, Ringing: true, Connected: true, Held: true, Transferring: true,
	}

	for _, s := range []State{Idle, Calling, Ringing, Connected, Held, Transferring, Terminated} {
		if got := s.InCall(); got != inCall[s] {
			t.Errorf("%s.InCall() = %v, want %v", s, got, inCall[s])
		}
		if got := s.Busy(); got != busy[s] {
			t.Errorf("%s.Busy() = %v, want %v", s, got, busy[s])
		}
	}
}
