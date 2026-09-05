package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"

	"sipclient/internal/applog"
	"sipclient/internal/channel"
	"sipclient/internal/config"
	"sipclient/internal/sipua"
	"sipclient/internal/transfer"
)

// CLI is the interactive shell.
type CLI struct {
	cfg  config.Config
	ua   *sipua.UA
	mgr  *channel.Manager
	xfer *transfer.Transferor

	tracer     *applog.Tracer
	deviceDesc string

	commands map[string]command
	order    []string

	rl *readline.Instance
	// out is where command output goes: the readline instance in interactive
	// mode (so it redraws the prompt), plain stdout when scripted.
	outMu sync.Mutex
	out   io.Writer

	// lastStatus dedupes the status line so it is printed only when something
	// actually changed.
	lastStatus        string
	unsubscribeChange func()

	// events is subscribed at construction, not when the pump goroutine runs.
	// An inbound call can arrive before the REPL is scheduled, and subscribing
	// later would lose that announcement.
	events            <-chan channel.Event
	unsubscribeEvents func()

	quitOnce sync.Once
	quit     chan struct{}

	// scriptFailed records a scripted command error for the exit code (FR-9.9).
	scriptFailed bool
}

// Options configures the CLI.
type Options struct {
	Config     config.Config
	UA         *sipua.UA
	Manager    *channel.Manager
	Transferor *transfer.Transferor
	Tracer     *applog.Tracer
	DeviceDesc string
}

// New builds the CLI.
func New(opt Options) *CLI {
	c := &CLI{
		cfg:        opt.Config,
		ua:         opt.UA,
		mgr:        opt.Manager,
		xfer:       opt.Transferor,
		tracer:     opt.Tracer,
		deviceDesc: opt.DeviceDesc,
		out:        os.Stdout,
		quit:       make(chan struct{}),
	}
	c.buildCommands()
	c.events, c.unsubscribeEvents = opt.Manager.Subscribe()
	return c
}

// Writer returns the sink the logger should use for console records, so log
// lines are printed above the prompt rather than through it (FR-9.4).
func (c *CLI) Writer() io.Writer { return writerFunc(c.writeRaw) }

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func (c *CLI) writeRaw(p []byte) (int, error) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	return c.out.Write(p)
}

// printf writes one line of command output.
func (c *CLI) printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, _ = c.writeRaw([]byte(line))
}

func (c *CLI) requestQuit() {
	c.quitOnce.Do(func() { close(c.quit) })
}

// Quit exposes the quit signal so main can wait on it.
func (c *CLI) Quit() <-chan struct{} { return c.quit }

// ScriptFailed reports whether a scripted command failed (FR-9.9 exit code 3).
func (c *CLI) ScriptFailed() bool { return c.scriptFailed }

// Run drives the REPL until quit, EOF or ctx cancellation. It picks
// interactive or scripted mode from whether stdin is a terminal (FR-9.8).
func (c *CLI) Run(ctx context.Context) error {
	go c.pumpEvents(ctx)

	if !readline.DefaultIsTerminal() {
		return c.runScript(ctx)
	}
	return c.runInteractive(ctx)
}

func (c *CLI) runInteractive(ctx context.Context) error {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          c.prompt(),
		HistoryFile:     historyPath(),
		AutoComplete:    c.completer(),
		InterruptPrompt: "^C",
		EOFPrompt:       "quit",
	})
	if err != nil {
		return fmt.Errorf("cli: start readline: %w", err)
	}
	defer rl.Close()
	defer func() {
		if c.unsubscribeChange != nil {
			c.unsubscribeChange()
		}
	}()

	c.outMu.Lock()
	c.rl = rl
	c.out = rl.Stdout()
	c.outMu.Unlock()

	// SIP packet echo must print above the prompt like any other output.
	if c.tracer != nil {
		c.tracer.SetConsole(c.Writer())
	}

	c.unsubscribeChange = c.mgr.AddChangeListener(c.refreshStatus)

	c.printf("sipclient ready. `help` for commands, `status` for state.")
	c.refreshStatus()

	// interrupts counts consecutive Ctrl-C presses: the first is a graceful
	// shutdown, a second forces exit (FR-9.7).
	interrupts := 0

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.quit:
			return nil
		default:
		}

		line, err := rl.Readline()
		switch {
		case err == readline.ErrInterrupt:
			interrupts++
			if interrupts >= 2 {
				c.printf("interrupted twice, exiting immediately")
				os.Exit(1)
			}
			c.printf("interrupt: shutting down (Ctrl-C again to force)")
			c.requestQuit()
			return nil
		case err == io.EOF:
			c.requestQuit()
			return nil
		case err != nil:
			return fmt.Errorf("cli: read line: %w", err)
		}
		interrupts = 0

		if err := c.dispatch(ctx, line); err != nil {
			c.printf("error: %s", err)
		}

		select {
		case <-c.quit:
			return nil
		default:
		}
		c.refreshStatus()
	}
}

// runScript executes piped commands in order and exits at EOF (FR-9.8).
func (c *CLI) runScript(ctx context.Context) error {
	if c.tracer != nil {
		c.tracer.SetConsole(c.Writer())
	}
	c.printf("sipclient: reading commands from stdin")
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case <-c.quit:
			return nil
		default:
		}

		c.printf("> %s", line)
		if err := c.dispatch(ctx, line); err != nil {
			c.printf("error: %s", err)
			c.scriptFailed = true
		}
		select {
		case <-c.quit:
			return nil
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("cli: read stdin: %w", err)
	}
	c.requestQuit()
	return nil
}

// dispatch parses and runs one command line.
func (c *CLI) dispatch(ctx context.Context, line string) error {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return nil
	}

	name := strings.ToLower(fields[0])
	cmd, ok := c.commands[name]
	if !ok {
		return fmt.Errorf("unknown command %q; try `help`", fields[0])
	}

	args := fields[1:]
	if len(args) < cmd.minArgs {
		return fmt.Errorf("usage: %s", cmd.usage)
	}
	return cmd.run(c, ctx, args)
}

// pumpEvents prints asynchronous notifications above the prompt (FR-9.4).
func (c *CLI) pumpEvents(ctx context.Context) {
	defer c.unsubscribeEvents()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.quit:
			return
		case e := <-c.events:
			if e.Kind == channel.EventStateChange {
				c.refreshStatus()
				continue
			}
			c.printf("%s", e.String())
			c.refreshStatus()
		}
	}
}

// statusLine renders registration plus both channels (FR-9.2).
func (c *CLI) statusLine() string {
	reg := c.ua.Registration()
	parts := []string{registrationShort(reg)}
	for _, ch := range c.mgr.Channels() {
		parts = append(parts, ch.Snapshot().Label())
	}
	if c.mgr.Muted() {
		parts = append(parts, "MIC MUTED")
	}
	return strings.Join(parts, "  \u2502  ")
}

func (c *CLI) prompt() string { return "sip> " }

// refreshStatus prints the status line above the prompt when it has changed.
//
// The status line is deliberately NOT part of the prompt. readline redraws the
// prompt after every write, so a multi-line command output (`help`) would
// reprint the status line once per line written.
func (c *CLI) refreshStatus() {
	line := c.statusLine()

	c.outMu.Lock()
	if line == c.lastStatus {
		c.outMu.Unlock()
		return
	}
	c.lastStatus = line
	c.outMu.Unlock()

	c.printf("%s", line)
}

// registrationShort renders registration for the status line.
func registrationShort(r sipua.Registration) string {
	switch r.State {
	case sipua.RegRegistered:
		if !r.NextRefresh.IsZero() {
			return fmt.Sprintf("REG ok (refresh %s)", shortDuration(time.Until(r.NextRefresh)))
		}
		return "REG ok"
	case sipua.RegRegistering:
		if r.LastCode != 0 {
			return fmt.Sprintf("REG retrying (last %d)", r.LastCode)
		}
		return "REG ..."
	case sipua.RegFailed:
		if r.LastCode != 0 {
			return fmt.Sprintf("REG FAILED (%d)", r.LastCode)
		}
		return "REG FAILED"
	}
	return "REG none"
}

// describeRegistration is the verbose form used by `status` (FR-2.6).
func describeRegistration(r sipua.Registration) string {
	switch r.State {
	case sipua.RegRegistered:
		return fmt.Sprintf("registered, granted %s, refresh in %s",
			r.GrantedFor, shortDuration(time.Until(r.NextRefresh)))
	case sipua.RegRegistering:
		if r.LastError != "" {
			return fmt.Sprintf("retrying after %d attempt(s): %s", r.Attempts, r.LastError)
		}
		return "in progress"
	case sipua.RegFailed:
		return fmt.Sprintf("FAILED after %d attempt(s): %s -- fix credentials then run `register`",
			r.Attempts, r.LastError)
	}
	return "not registered"
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// completer offers command names and channel numbers (FR-9.1).
func (c *CLI) completer() readline.AutoCompleter {
	channels := []readline.PrefixCompleterInterface{
		readline.PcItem("1"), readline.PcItem("2"),
	}

	items := make([]readline.PrefixCompleterInterface, 0, len(c.order))
	for _, name := range c.order {
		switch name {
		case "answer", "reject", "hangup", "hold", "unhold", "stats", "cancelxfer", "xfer":
			items = append(items, readline.PcItem(name, channels...))
		case "debug":
			items = append(items, readline.PcItem(name,
				readline.PcItem("on"), readline.PcItem("off")))
		case "help":
			names := make([]readline.PrefixCompleterInterface, 0, len(c.order))
			for _, n := range c.order {
				names = append(names, readline.PcItem(n))
			}
			items = append(items, readline.PcItem(name, names...))
		default:
			items = append(items, readline.PcItem(name))
		}
	}
	return readline.NewPrefixCompleter(items...)
}

func historyPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ".sipclient_history"
	}
	return dir + "/.sipclient_history"
}

// reportTransfer renders a transfer outcome. Anything other than success
// leaves both calls up, so it always says so and how to recover -- an operator
// mid-transfer needs to know the caller is still there (FR-5.5, FR-5.6).
func (c *CLI) reportTransfer(res transfer.Result, err error) {
	if err == nil && res.Outcome == transfer.Success {
		if res.Inferred {
			// No NOTIFY arrived; say so, and say how we know.
			c.printf("transfer completed, but the PBX sent no NOTIFY to confirm it")
			if res.ProbeDetail != "" {
				c.printf("       %s", res.ProbeDetail)
			}
			c.printf("       verify with the parties before hanging up the remaining channel")
			return
		}
		c.printf("transfer succeeded (%d %s); the parties are talking directly",
			res.Code, res.Reason)
		return
	}

	switch {
	case err != nil:
		c.printf("error: %s", err)
	case res.Outcome == transfer.Failed:
		c.printf("transfer failed: %d %s", res.Code, res.Reason)
	default:
		c.printf("transfer outcome unknown")
	}

	c.printf("       both calls are still up; `status` shows them and `swap` " +
		"takes the held party back")
	c.scriptFailed = true
}
