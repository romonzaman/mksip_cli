package control

import (
	"time"

	"sipclient/internal/channel"
	"sipclient/internal/sipua"
)

// State is a consistent read of everything a surface needs to render. The JSON
// tags are the web UI's wire format; the CLI reads the fields directly.
type State struct {
	Registration RegistrationState `json:"registration"`
	Channels     []ChannelState    `json:"channels"`
	ActiveID     int               `json:"activeChannel"`
	Muted        bool              `json:"muted"`
	// AudioOnHost is always true today and is sent so the UI can say so: the
	// browser controls the client, it does not carry the audio.
	AudioOnHost bool   `json:"audioOnHost"`
	AudioDevice string `json:"audioDevice"`
}

// RegistrationState describes the SIP registration (FR-2.6).
type RegistrationState struct {
	State     string `json:"state"`
	AOR       string `json:"aor"`
	Server    string `json:"server"`
	Transport string `json:"transport"`
	Contact   string `json:"contact"`
	// RefreshInSeconds is how long until the next refresh, 0 when not registered.
	RefreshInSeconds int    `json:"refreshInSeconds"`
	LastError        string `json:"lastError,omitempty"`
	LastCode         int    `json:"lastCode,omitempty"`
}

// ChannelState describes one channel.
type ChannelState struct {
	ID              int    `json:"id"`
	State           string `json:"state"`
	Remote          string `json:"remote,omitempty"`
	Inbound         bool   `json:"inbound"`
	Active          bool   `json:"active"`
	DurationSeconds int    `json:"durationSeconds"`
	LocalHold       bool   `json:"localHold"`
	RemoteHold      bool   `json:"remoteHold"`
	Muted           bool   `json:"muted"`
	Codec           string `json:"codec,omitempty"`
	RTPPort         int    `json:"rtpPort,omitempty"`

	// Ringing is true only for an inbound call awaiting a decision, which is
	// the case a surface must make impossible to miss (FR-9.5).
	Ringing bool `json:"ringing"`
	// InCall is true when a media session exists, so a surface can enable or
	// disable actions without duplicating the state machine.
	InCall bool `json:"inCall"`
}

// AudioDescriber supplies the audio device description for State.
type AudioDescriber interface{ Description() string }

// Snapshot reads the whole client state.
func (c *Controller) Snapshot() State {
	reg := c.ua.Registration()

	s := State{
		Registration: RegistrationState{
			State:     reg.State.String(),
			AOR:       c.cfg.AOR(),
			Server:    c.cfg.ServerAddr(),
			Transport: c.cfg.SIP.Server.Transport,
			Contact:   c.ua.ContactURI(),
			LastError: reg.LastError,
			LastCode:  reg.LastCode,
		},
		ActiveID:    c.mgr.ActiveID(),
		Muted:       c.mgr.Muted(),
		AudioOnHost: true,
		AudioDevice: c.audioDevice,
	}
	if !reg.NextRefresh.IsZero() {
		if d := time.Until(reg.NextRefresh); d > 0 {
			s.Registration.RefreshInSeconds = int(d.Seconds())
		}
	}

	for _, ch := range c.mgr.Channels() {
		snap := ch.Snapshot()
		s.Channels = append(s.Channels, ChannelState{
			ID:              snap.ID,
			State:           snap.State.String(),
			Remote:          snap.Remote,
			Inbound:         snap.Inbound,
			Active:          snap.Active,
			DurationSeconds: int(snap.Duration.Seconds()),
			LocalHold:       snap.LocalHeld,
			RemoteHold:      snap.RemoteHeld,
			Muted:           snap.Muted,
			Codec:           snap.Codec,
			RTPPort:         snap.RTPPort,
			Ringing:         snap.State == channel.Ringing && snap.Inbound,
			InCall:          snap.State.InCall(),
		})
	}
	return s
}

// SetAudioDevice records the device description for Snapshot.
func (c *Controller) SetAudioDevice(desc string) { c.audioDevice = desc }

// Registration exposes raw registration status for the CLI's status command.
func (c *Controller) Registration() sipua.Registration { return c.ua.Registration() }
