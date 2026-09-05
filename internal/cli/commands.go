// Package cli is the interactive REPL and command set (requirements §9).
package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"sipclient/internal/audio"
	"sipclient/internal/channel"
)

// command is one REPL verb.
type command struct {
	name    string
	usage   string
	summary string
	// minArgs is enforced before run is called.
	minArgs int
	run     func(c *CLI, ctx context.Context, args []string) error
}

func (c *CLI) buildCommands() {
	cmds := []command{
		{
			name: "dial", usage: "dial <target> [channel]", minArgs: 1,
			summary: "place a call to an extension or SIP URI",
			run:     (*CLI).cmdDial,
		},
		{
			name: "answer", usage: "answer [channel]",
			summary: "answer a ringing inbound call",
			run:     (*CLI).cmdAnswer,
		},
		{
			name: "reject", usage: "reject [channel] [code]",
			summary: "decline a ringing call (default 603)",
			run:     (*CLI).cmdReject,
		},
		{
			name: "hangup", usage: "hangup [channel|all]",
			summary: "end a call",
			run:     (*CLI).cmdHangup,
		},
		{
			name: "hold", usage: "hold [channel]",
			summary: "put a call on hold",
			run:     (*CLI).cmdHold,
		},
		{
			name: "unhold", usage: "unhold [channel]",
			summary: "retrieve a held call",
			run:     (*CLI).cmdUnhold,
		},
		{
			name: "swap", usage: "swap",
			summary: "hold the active channel and retrieve the other",
			run:     (*CLI).cmdSwap,
		},
		{
			name: "xfer", usage: "xfer [transferee_channel] [consult_channel]",
			summary: "attended transfer: connect the two parties and drop out",
			run:     (*CLI).cmdXfer,
		},
		{
			name: "bxfer", usage: "bxfer <target> [channel]", minArgs: 1,
			summary: "blind transfer: send the call to target without consulting",
			run:     (*CLI).cmdBxfer,
		},
		{
			name: "cancelxfer", usage: "cancelxfer [consult_channel]",
			summary: "abandon a consultation and return to the held party",
			run:     (*CLI).cmdCancelXfer,
		},
		{
			name: "dtmf", usage: "dtmf <digits> [channel]", minArgs: 1,
			summary: "send DTMF digits",
			run:     (*CLI).cmdDTMF,
		},
		{
			name: "status", usage: "status",
			summary: "show registration and both channels in detail",
			run:     (*CLI).cmdStatus,
		},
		{
			name: "stats", usage: "stats [channel]",
			summary: "show RTP statistics",
			run:     (*CLI).cmdStats,
		},
		{
			name: "register", usage: "register",
			summary: "force a re-registration attempt",
			run:     (*CLI).cmdRegister,
		},
		{
			name: "unregister", usage: "unregister",
			summary: "de-register from the PBX",
			run:     (*CLI).cmdUnregister,
		},
		{
			name: "devices", usage: "devices",
			summary: "list audio input and output devices",
			run:     (*CLI).cmdDevices,
		},
		{
			name: "mute", usage: "mute",
			summary: "mute the microphone",
			run:     (*CLI).cmdMute,
		},
		{
			name: "unmute", usage: "unmute",
			summary: "unmute the microphone",
			run:     (*CLI).cmdUnmute,
		},
		{
			name: "debug", usage: "debug [on|off]",
			summary: "echo SIP packets to the terminal (no argument toggles)",
			run:     (*CLI).cmdDebug,
		},
		{
			name: "config", usage: "config",
			summary: "show the effective configuration (password redacted)",
			run:     (*CLI).cmdConfig,
		},
		{
			name: "sleep", usage: "sleep <milliseconds>", minArgs: 1,
			summary: "pause, for scripted runs",
			run:     (*CLI).cmdSleep,
		},
		{
			name: "wait", usage: "wait <channel> <state> [timeout_ms]", minArgs: 2,
			summary: "block until a channel reaches a state, for scripted runs",
			run:     (*CLI).cmdWait,
		},
		{
			name: "help", usage: "help [command]",
			summary: "show usage",
			run:     (*CLI).cmdHelp,
		},
		{
			name: "quit", usage: "quit",
			summary: "de-register, hang up and exit",
			run:     (*CLI).cmdQuit,
		},
	}

	c.commands = make(map[string]command, len(cmds))
	for _, cmd := range cmds {
		c.commands[cmd.name] = cmd
	}
	c.order = make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		c.order = append(c.order, cmd.name)
	}
}

// parseChannel reads an optional channel argument, 0 meaning "the active one".
func parseChannel(args []string, idx int) (int, error) {
	if len(args) <= idx {
		return 0, nil
	}
	n, err := strconv.Atoi(args[idx])
	if err != nil {
		return 0, fmt.Errorf("channel must be a number, got %q", args[idx])
	}
	if n < 1 || n > channel.ChannelCount {
		return 0, fmt.Errorf("no channel %d (valid: 1-%d)", n, channel.ChannelCount)
	}
	return n, nil
}

func (c *CLI) cmdDial(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 1)
	if err != nil {
		return err
	}

	used, err := c.ctl.Dial(ctx, args[0], id)
	if err != nil {
		return err
	}
	c.printf("channel %d: calling %s", used, args[0])
	return nil
}

func (c *CLI) cmdAnswer(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 0)
	if err != nil {
		return err
	}
	return c.ctl.Answer(ctx, id)
}

func (c *CLI) cmdReject(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 0)
	if err != nil {
		return err
	}
	code := 0 // controller applies the default
	if len(args) > 1 {
		if code, err = strconv.Atoi(args[1]); err != nil {
			return fmt.Errorf("reject code must be a number, got %q", args[1])
		}
	}
	return c.ctl.Reject(ctx, id, code)
}

func (c *CLI) cmdHangup(ctx context.Context, args []string) error {
	if len(args) > 0 && strings.EqualFold(args[0], "all") {
		c.ctl.HangupAll(ctx)
		c.printf("all channels cleared")
		return nil
	}
	id, err := parseChannel(args, 0)
	if err != nil {
		return err
	}
	return c.ctl.Hangup(ctx, id)
}

func (c *CLI) cmdHold(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 0)
	if err != nil {
		return err
	}
	return c.ctl.Hold(ctx, id, true)
}

func (c *CLI) cmdUnhold(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 0)
	if err != nil {
		return err
	}
	return c.ctl.Hold(ctx, id, false)
}

func (c *CLI) cmdSwap(ctx context.Context, _ []string) error {
	return c.ctl.Swap(ctx)
}

// cmdXfer runs the attended transfer. With no arguments it does the natural
// thing: transfer the other party to whoever we are consulting on the active
// channel (FR-5.1).
func (c *CLI) cmdXfer(ctx context.Context, args []string) error {
	var transfereeID, consultID int
	var err error
	if len(args) > 0 {
		if transfereeID, err = parseChannel(args, 0); err != nil {
			return err
		}
	}
	if len(args) > 1 {
		if consultID, err = parseChannel(args, 1); err != nil {
			return err
		}
	}

	transfereeID, consultID, err = c.ctl.TransferRoles(transfereeID, consultID)
	if err != nil {
		return err
	}

	c.printf("transferring channel %d to channel %d's party...", transfereeID, consultID)
	res, err := c.ctl.AttendedTransfer(ctx, transfereeID, consultID)
	c.reportTransfer(res, err)
	return nil
}

func (c *CLI) cmdBxfer(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 1)
	if err != nil {
		return err
	}
	used, err := c.ctl.ResolveChannelID(id)
	if err != nil {
		return err
	}
	c.printf("blind transferring channel %d to %s...", used, args[0])
	res, err := c.ctl.BlindTransfer(ctx, id, args[0])
	c.reportTransfer(res, err)
	return nil
}

func (c *CLI) cmdCancelXfer(ctx context.Context, args []string) error {
	consultID, err := parseChannel(args, 0)
	if err != nil {
		return err
	}
	transfereeID, err := c.ctl.CancelConsult(ctx, consultID)
	if err != nil {
		return err
	}
	c.printf("consultation abandoned; back on channel %d", transfereeID)
	return nil
}

func (c *CLI) cmdDTMF(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 1)
	if err != nil {
		return err
	}
	used, err := c.ctl.ResolveChannelID(id)
	if err != nil {
		return err
	}
	if err := c.ctl.SendDTMF(id, args[0]); err != nil {
		return err
	}
	c.printf("channel %d: sent DTMF %s", used, args[0])
	return nil
}

func (c *CLI) cmdStatus(_ context.Context, _ []string) error {
	reg := c.ua.Registration()
	c.printf("registration: %s", describeRegistration(reg))
	c.printf("audio: %s%s", c.deviceDesc, mutedSuffix(c.ctl.Muted()))
	c.printf("debug: %s", c.debugState())
	if url := c.ctl.WebURL(); url != "" {
		c.printf("web:   %s", url)
	}

	for _, ch := range c.mgr.Channels() {
		s := ch.Snapshot()
		c.printf("  %s", s.Label())
		if s.State.InCall() {
			detail := fmt.Sprintf("      codec=%s rtp_port=%d", s.Codec, s.RTPPort)
			if s.LocalHeld {
				detail += " local_hold"
			}
			if s.RemoteHeld {
				detail += " remote_hold"
			}
			c.printf("%s", detail)
		}
	}
	return nil
}

// debugState describes where SIP packets are currently going.
func (c *CLI) debugState() string {
	if c.tracer == nil {
		return "unavailable"
	}
	switch {
	case c.tracer.ConsoleEnabled() && c.tracer.FileEnabled():
		return "on (terminal + " + c.cfg.Logging.SIPTraceFile + ")"
	case c.tracer.ConsoleEnabled():
		return "on (terminal only)"
	case c.tracer.FileEnabled():
		return "off (SIP trace in " + c.cfg.Logging.SIPTraceFile + "; `debug on` to show here)"
	}
	return "off (no SIP trace; `debug on` to show here)"
}

func mutedSuffix(muted bool) string {
	if muted {
		return "  [MIC MUTED]"
	}
	return ""
}

func (c *CLI) cmdStats(_ context.Context, args []string) error {
	id, err := parseChannel(args, 0)
	if err != nil {
		return err
	}

	targets := c.mgr.Channels()
	if id != 0 {
		ch, err := c.mgr.Get(id)
		if err != nil {
			return err
		}
		targets = []*channel.Channel{ch}
	}

	any := false
	for _, ch := range targets {
		st, ok := ch.Stats()
		if !ok {
			continue
		}
		any = true
		c.printf("channel %d: codec=%s local_port=%d remote=%s",
			ch.ID, st.Codec, st.LocalPort, st.RemoteAddr)
		c.printf("  sent=%d (%d B)  recv=%d (%d B)",
			st.PacketsSent, st.BytesSent, st.PacketsRecv, st.BytesRecv)
		c.printf("  lost=%d  late=%d  concealed=%d  jitter=%.1fms",
			st.Lost, st.Late, st.Concealed, st.JitterMS)
	}
	if !any {
		return fmt.Errorf("no active media session")
	}
	return nil
}

func (c *CLI) cmdRegister(ctx context.Context, _ []string) error {
	c.ctl.Register()
	c.printf("registration attempt requested")
	return nil
}

func (c *CLI) cmdUnregister(ctx context.Context, _ []string) error {
	if err := c.ctl.Unregister(ctx); err != nil {
		return err
	}
	c.printf("de-registered")
	return nil
}

func (c *CLI) cmdDevices(_ context.Context, _ []string) error {
	devices, err := audio.List()
	if err != nil {
		return err
	}
	sort.SliceStable(devices, func(i, j int) bool {
		return devices[i].IsCapture && !devices[j].IsCapture
	})
	for _, d := range devices {
		kind := "output"
		if d.IsCapture {
			kind = "input "
		}
		mark := ""
		if d.IsDefault {
			mark = "  (default)"
		}
		c.printf("  %s  %s%s", kind, d.Name, mark)
	}
	c.printf("in use: %s", c.deviceDesc)
	return nil
}

func (c *CLI) cmdMute(_ context.Context, _ []string) error {
	c.ctl.SetMuted(true)
	c.printf("microphone muted")
	return nil
}

func (c *CLI) cmdUnmute(_ context.Context, _ []string) error {
	c.ctl.SetMuted(false)
	c.printf("microphone live")
	return nil
}

// cmdDebug toggles echoing SIP packets to the terminal. The trace file is
// unaffected: this is purely an extra sink, so turning debug off never loses
// the record of what happened.
func (c *CLI) cmdDebug(_ context.Context, args []string) error {
	if c.tracer == nil {
		return fmt.Errorf("SIP tracing is unavailable")
	}

	var on bool
	switch {
	case len(args) == 0:
		on = !c.tracer.ConsoleEnabled() // bare `debug` toggles
	case strings.EqualFold(args[0], "on"), strings.EqualFold(args[0], "true"):
		on = true
	case strings.EqualFold(args[0], "off"), strings.EqualFold(args[0], "false"):
		on = false
	default:
		return fmt.Errorf("usage: debug [on|off]")
	}

	c.tracer.EnableConsole(on)
	if on {
		c.printf("debug on: SIP packets will be shown here")
		if !c.tracer.FileEnabled() {
			c.printf("       (logging.sip_trace is off, so packets are shown here only)")
		}
		return nil
	}
	c.printf("debug off: SIP packets no longer shown here")
	if c.tracer.FileEnabled() {
		c.printf("       (still recorded in %s)", c.cfg.Logging.SIPTraceFile)
	}
	return nil
}

func (c *CLI) cmdConfig(_ context.Context, _ []string) error {
	r := c.cfg.Redacted()
	c.printf("sip:      %s@%s -> %s/%s  (auth user %s, password %s)",
		r.SIP.Username, r.SIP.Domain, c.cfg.ServerAddr(), r.SIP.Server.Transport,
		r.SIP.AuthUsername, r.SIP.Password)
	c.printf("network:  listening %s  advertised %s  rport=%v",
		c.ua.ListenAddr(), c.ua.AdvertisedAddr(), r.Network.Rport)
	c.printf("media:    advertised %s  codecs=%s ptime=%dms jitter=%dms dtmf=%s rtp_ports=%d-%d",
		c.ua.MediaHost(),
		strings.Join(r.Media.Codecs, ","), r.Media.PtimeMS, r.Media.JitterBufferMS,
		r.Media.DTMFMode, r.Media.RTPPortStart, r.Media.RTPPortEnd)
	c.printf("transfer: mode=%s notify_timeout=%ds hangup_after_success=%v",
		r.Transfer.Mode, r.Transfer.NotifyTimeoutSeconds, r.Transfer.HangupAfterSuccess)
	c.printf("logging:  %s (level %s), sip trace %s",
		r.Logging.File, r.Logging.Level, r.Logging.SIPTraceFile)
	return nil
}

func (c *CLI) cmdSleep(ctx context.Context, args []string) error {
	ms, err := strconv.Atoi(args[0])
	if err != nil || ms < 0 {
		return fmt.Errorf("sleep needs a millisecond count, got %q", args[0])
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(ms) * time.Millisecond):
	}
	return nil
}

// cmdWait blocks until a channel reaches a state, so scripted transfer
// scenarios do not need arbitrary sleeps (FR-9.8).
func (c *CLI) cmdWait(ctx context.Context, args []string) error {
	id, err := parseChannel(args, 0)
	if err != nil {
		return err
	}
	if id == 0 {
		return fmt.Errorf("wait needs an explicit channel number")
	}
	ch, err := c.mgr.Get(id)
	if err != nil {
		return err
	}

	want := strings.ToUpper(args[1])
	valid := map[string]channel.State{
		"IDLE": channel.Idle, "CALLING": channel.Calling, "RINGING": channel.Ringing,
		"CONNECTED": channel.Connected, "HELD": channel.Held,
		"TRANSFERRING": channel.Transferring,
	}
	target, ok := valid[want]
	if !ok {
		return fmt.Errorf("unknown state %q (valid: IDLE CALLING RINGING CONNECTED HELD TRANSFERRING)", args[1])
	}

	timeout := 30 * time.Second
	if len(args) > 2 {
		ms, err := strconv.Atoi(args[2])
		if err != nil || ms <= 0 {
			return fmt.Errorf("timeout must be a positive millisecond count, got %q", args[2])
		}
		timeout = time.Duration(ms) * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ch.State() == target {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("channel %d did not reach %s within %s (still %s)",
		id, want, timeout, ch.State())
}

func (c *CLI) cmdHelp(_ context.Context, args []string) error {
	if len(args) > 0 {
		cmd, ok := c.commands[strings.ToLower(args[0])]
		if !ok {
			return fmt.Errorf("unknown command %q", args[0])
		}
		c.printf("%s", cmd.usage)
		c.printf("    %s", cmd.summary)
		return nil
	}
	for _, name := range c.order {
		cmd := c.commands[name]
		c.printf("  %-46s %s", cmd.usage, cmd.summary)
	}
	return nil
}

func (c *CLI) cmdQuit(_ context.Context, _ []string) error {
	c.requestQuit()
	return nil
}
