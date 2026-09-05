// Package config loads and validates the client's config.json (requirements §3).
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved configuration. Absent JSON fields keep the
// values from Default(), which is what makes booleans like Rport default true.
type Config struct {
	SIP      SIP      `json:"sip"`
	Network  Network  `json:"network"`
	Media    Media    `json:"media"`
	Audio    Audio    `json:"audio"`
	Transfer Transfer `json:"transfer"`
	Logging  Logging  `json:"logging"`
	Web      Web      `json:"web"`
	History  History  `json:"history"`
}

// History configures the call log.
type History struct {
	Enabled bool `json:"enabled"`
	// File holds the records. Empty keeps history in memory only, so it is
	// lost on restart.
	File string `json:"file"`
	// MaxEntries caps the log so it cannot grow without bound.
	MaxEntries int `json:"max_entries"`
}

// Web configures the browser control surface (MKSIP-1001). Absent means off,
// so existing configurations behave exactly as before.
type Web struct {
	Enabled bool `json:"enabled"`
	// ListenAddress must be a loopback address. Anyone who can reach this
	// server can place and transfer calls on your extension, so exposing it to
	// a network needs authentication, which does not exist yet.
	ListenAddress string `json:"listen_address"`
	// Port 0 picks a free one, as the SIP and RTP ports do.
	Port int `json:"port"`
}

type SIP struct {
	Username     string `json:"username"`
	AuthUsername string `json:"auth_username"`
	Password     string `json:"password"`
	DisplayName  string `json:"display_name"`
	Domain       string `json:"domain"`
	Server       Server `json:"server"`

	OutboundProxy         string `json:"outbound_proxy"`
	RegisterExpirySeconds int    `json:"register_expiry_seconds"`
	UserAgent             string `json:"user_agent"`

	// SessionExpiresSeconds is the RFC 4028 session interval to propose.
	// 0 disables session timers entirely: the client then neither offers them
	// nor acts as refresher, which is how it behaved before they existed.
	SessionExpiresSeconds int `json:"session_expires_seconds"`
	// MinSESeconds is the shortest session interval we will accept from a peer.
	MinSESeconds int `json:"min_se_seconds"`
}

type Server struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Transport string `json:"transport"`
}

type Network struct {
	// LocalAddress is the address advertised in Via, Contact and SDP. Empty
	// means it is discovered from the route toward the SIP server.
	LocalAddress string `json:"local_address"`
	// ListenAddress is the address the signalling sockets bind to. Empty means
	// all interfaces, so a call routed to this host by any path is accepted.
	// Pin it only to restrict which interface accepts SIP.
	ListenAddress string `json:"listen_address"`
	LocalSIPPort  int    `json:"local_sip_port"`
	PublicAddress string `json:"public_address"`
	STUNServer    string `json:"stun_server"`
	Rport         bool   `json:"rport"`
}

type Media struct {
	// AdvertiseAddress is the address put in the SDP connection line. Empty
	// means the same address as signalling. Set it when RTP and SIP reach this
	// host by different paths -- the same split FreeSWITCH calls ext-sip-ip
	// versus ext-rtp-ip.
	AdvertiseAddress string `json:"advertise_address"`

	// RTPPortStart and RTPPortEnd bound the RTP port range. Both zero (the
	// default) means the operating system picks a free port per call, which
	// needs no configuration and cannot collide. Set a range only when a
	// firewall requires a predictable one.
	RTPPortStart   int      `json:"rtp_port_start"`
	RTPPortEnd     int      `json:"rtp_port_end"`
	Codecs         []string `json:"codecs"`
	PtimeMS        int      `json:"ptime_ms"`
	DTMFMode       string   `json:"dtmf_mode"`
	JitterBufferMS int      `json:"jitter_buffer_ms"`
	RTCPEnabled    bool     `json:"rtcp_enabled"`
}

type Audio struct {
	InputDevice     string  `json:"input_device"`
	OutputDevice    string  `json:"output_device"`
	InputGain       float64 `json:"input_gain"`
	OutputGain      float64 `json:"output_gain"`
	RingbackEnabled bool    `json:"ringback_enabled"`
}

type Transfer struct {
	Mode                 string `json:"mode"`
	NotifyTimeoutSeconds int    `json:"notify_timeout_seconds"`
	HangupAfterSuccess   bool   `json:"hangup_after_success"`
}

type Logging struct {
	Level        string `json:"level"`
	File         string `json:"file"`
	SIPTrace     bool   `json:"sip_trace"`
	SIPTraceFile string `json:"sip_trace_file"`
	ConsoleLevel string `json:"console_level"`
}

// Default returns the documented defaults from requirements §3.3.
func Default() Config {
	return Config{
		SIP: SIP{
			Server:                Server{Port: 5060, Transport: "udp"},
			RegisterExpirySeconds: 300,
			UserAgent:             "sipclient-cli/1.0",
			SessionExpiresSeconds: 1800,
			MinSESeconds:          90,
		},
		// Port 0 means the OS picks a free SIP port, so the client never
		// collides with a PBX or another SIP process on the same host.
		Network: Network{LocalSIPPort: 0, Rport: true},
		Media: Media{
			// Zero means the OS picks a free port per call (see Media.RTPPortStart).
			RTPPortStart: 0, RTPPortEnd: 0,
			Codecs:  []string{"PCMU", "PCMA"},
			PtimeMS: 20, DTMFMode: "rfc4733",
			JitterBufferMS: 60, RTCPEnabled: true,
		},
		Audio: Audio{InputGain: 1.0, OutputGain: 1.0, RingbackEnabled: true},
		Transfer: Transfer{
			Mode: "refer", NotifyTimeoutSeconds: 30, HangupAfterSuccess: true,
		},
		History: History{
			Enabled:    true,
			File:       "call-history.jsonl",
			MaxEntries: 200,
		},
		Web: Web{
			Enabled:       false,
			ListenAddress: "127.0.0.1",
			Port:          8080,
		},
		Logging: Logging{
			Level: "info", File: "sipclient.log",
			SIPTrace: true, SIPTraceFile: "sip-trace.log",
			ConsoleLevel: "warn",
		},
	}
}

// Load reads, strictly decodes and validates the config file (FR-1.1 - FR-1.4).
// InsecurePerms reports whether the file is group/world readable (FR-1.6); the
// caller warns rather than failing.
func Load(path string) (cfg Config, insecurePerms bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return cfg, false, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	if st, serr := f.Stat(); serr == nil && st.Mode().Perm()&0o077 != 0 {
		insecurePerms = true
	}

	// Decode into the defaults so omitted keys keep their documented value.
	cfg = Default()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // FR-1.3
	if err := dec.Decode(&cfg); err != nil {
		return cfg, insecurePerms, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.resolve(); err != nil {
		return cfg, insecurePerms, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, insecurePerms, err
	}
	return cfg, insecurePerms, nil
}

// resolve fills derived values and expands env: password references (FR-1.7).
func (c *Config) resolve() error {
	if c.SIP.AuthUsername == "" {
		c.SIP.AuthUsername = c.SIP.Username
	}
	if c.SIP.DisplayName == "" {
		c.SIP.DisplayName = c.SIP.Username
	}
	if ref, ok := strings.CutPrefix(c.SIP.Password, "env:"); ok {
		v, found := os.LookupEnv(ref)
		if !found {
			return fmt.Errorf("sip.password: environment variable %q is not set", ref)
		}
		if v == "" {
			return fmt.Errorf("sip.password: environment variable %q is empty", ref)
		}
		c.SIP.Password = v
	}
	c.SIP.Server.Transport = strings.ToLower(c.SIP.Server.Transport)
	for i, name := range c.Media.Codecs {
		c.Media.Codecs[i] = strings.ToUpper(name)
	}
	return nil
}

// Validate enforces §3.3. Errors name the JSON path (FR-1.2).
func (c *Config) Validate() error {
	var errs []string
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if c.SIP.Username == "" {
		bad("sip.username: required")
	}
	if c.SIP.Password == "" {
		bad("sip.password: required")
	}
	if c.SIP.Domain == "" {
		bad("sip.domain: required")
	}
	if c.SIP.Server.Host == "" {
		bad("sip.server.host: required")
	}
	if c.SIP.Server.Port < 1 || c.SIP.Server.Port > 65535 {
		bad("sip.server.port: must be 1-65535, got %d", c.SIP.Server.Port)
	}
	if t := c.SIP.Server.Transport; t != "udp" && t != "tcp" {
		bad("sip.server.transport: must be \"udp\" or \"tcp\", got %q", t)
	}
	if e := c.SIP.RegisterExpirySeconds; e < 60 || e > 3600 {
		bad("sip.register_expiry_seconds: must be 60-3600, got %d", e)
	}

	// Session timers: 0 disables them; otherwise RFC 4028 §4 sets 90s as the
	// smallest interval any implementation must accept.
	if e := c.SIP.SessionExpiresSeconds; e != 0 {
		if e < 90 || e > 86400 {
			bad("sip.session_expires_seconds: must be 0 (disabled) or 90-86400, got %d", e)
		}
		if m := c.SIP.MinSESeconds; m < 90 || m > 86400 {
			bad("sip.min_se_seconds: must be 90-86400, got %d", m)
		} else if m > e {
			bad("sip.min_se_seconds (%d) cannot exceed sip.session_expires_seconds (%d)", m, e)
		}
	}

	if p := c.Network.LocalSIPPort; p < 0 || p > 65535 {
		bad("network.local_sip_port: must be 0-65535, got %d", p)
	}

	// A range is optional: both zero means the OS assigns ports per call.
	switch {
	case c.Media.RTPPortStart == 0 && c.Media.RTPPortEnd == 0:
		// Ephemeral mode; nothing to validate.
	case c.Media.RTPPortStart == 0:
		bad("media.rtp_port_start: must be set when media.rtp_port_end is (or " +
			"set both to 0 to let the OS choose)")
	case c.Media.RTPPortEnd == 0:
		bad("media.rtp_port_end: must be set when media.rtp_port_start is (or " +
			"set both to 0 to let the OS choose)")
	default:
		if c.Media.RTPPortStart < 1 || c.Media.RTPPortStart > 65535 {
			bad("media.rtp_port_start: must be 1-65535, got %d", c.Media.RTPPortStart)
		}
		if c.Media.RTPPortEnd > 65535 {
			bad("media.rtp_port_end: must be 1-65535, got %d", c.Media.RTPPortEnd)
		}
		if c.Media.RTPPortEnd < c.Media.RTPPortStart {
			bad("media.rtp_port_end: must be >= media.rtp_port_start")
		} else if c.Media.RTPPortEnd-c.Media.RTPPortStart < 3 {
			// Two channels need two RTP/RTCP pairs (§3.3).
			bad("media.rtp_port_range: need at least 4 ports for 2 channels, got %d",
				c.Media.RTPPortEnd-c.Media.RTPPortStart+1)
		}
	}
	if len(c.Media.Codecs) == 0 {
		bad("media.codecs: at least one codec required")
	}
	for _, name := range c.Media.Codecs {
		if name != "PCMU" && name != "PCMA" {
			bad("media.codecs: %q unsupported in v1 (PCMU, PCMA only)", name)
		}
	}
	if p := c.Media.PtimeMS; p != 20 && p != 30 {
		bad("media.ptime_ms: must be 20 or 30, got %d", p)
	}
	switch c.Media.DTMFMode {
	case "rfc4733", "info", "both":
	default:
		bad("media.dtmf_mode: must be \"rfc4733\", \"info\" or \"both\", got %q", c.Media.DTMFMode)
	}
	if j := c.Media.JitterBufferMS; j < 0 || j > 500 {
		bad("media.jitter_buffer_ms: must be 0-500, got %d", j)
	}

	if g := c.Audio.InputGain; g < 0 || g > 4 {
		bad("audio.input_gain: must be 0.0-4.0, got %v", g)
	}
	if g := c.Audio.OutputGain; g < 0 || g > 4 {
		bad("audio.output_gain: must be 0.0-4.0, got %v", g)
	}

	if c.Transfer.Mode != "refer" {
		bad("transfer.mode: only \"refer\" supported in v1, got %q", c.Transfer.Mode)
	}
	if t := c.Transfer.NotifyTimeoutSeconds; t < 1 || t > 300 {
		bad("transfer.notify_timeout_seconds: must be 1-300, got %d", t)
	}

	if c.History.Enabled {
		if n := c.History.MaxEntries; n < 1 || n > 100000 {
			bad("history.max_entries: must be 1-100000, got %d", n)
		}
	}

	if c.Web.Enabled {
		if c.Web.Port < 0 || c.Web.Port > 65535 {
			bad("web.port: must be 0-65535, got %d", c.Web.Port)
		}
		if !isLoopback(c.Web.ListenAddress) {
			bad("web.listen_address: must be a loopback address (127.0.0.1, ::1 or "+
				"localhost), got %q -- the web UI can place calls on your extension "+
				"and has no authentication", c.Web.ListenAddress)
		}
	}

	for path, lvl := range map[string]string{
		"logging.level":         c.Logging.Level,
		"logging.console_level": c.Logging.ConsoleLevel,
	} {
		switch lvl {
		case "debug", "info", "warn", "error":
		default:
			bad("%s: must be debug|info|warn|error, got %q", path, lvl)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// isLoopback reports whether an address is safe to bind the unauthenticated
// web UI to.
func isLoopback(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// WebAddr is the host:port the web UI binds to.
func (c *Config) WebAddr() string {
	host := c.Web.ListenAddress
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(c.Web.Port))
}

// EphemeralRTPPorts reports whether the OS assigns RTP ports per call.
func (c *Config) EphemeralRTPPorts() bool {
	return c.Media.RTPPortStart == 0 && c.Media.RTPPortEnd == 0
}

// SessionTimer renders the session-timer settings as durations.
func (c *Config) SessionTimer() (expires, minSE time.Duration) {
	return time.Duration(c.SIP.SessionExpiresSeconds) * time.Second,
		time.Duration(c.SIP.MinSESeconds) * time.Second
}

// AOR is the address of record, sip:user@domain.
func (c *Config) AOR() string {
	return fmt.Sprintf("sip:%s@%s", c.SIP.Username, c.SIP.Domain)
}

// ServerAddr is host:port of the registrar/proxy.
func (c *Config) ServerAddr() string {
	return fmt.Sprintf("%s:%d", c.SIP.Server.Host, c.SIP.Server.Port)
}

// Redacted returns a copy safe to print, with the password masked (FR-1.5).
func (c Config) Redacted() Config {
	if c.SIP.Password != "" {
		c.SIP.Password = "***"
	}
	return c
}
