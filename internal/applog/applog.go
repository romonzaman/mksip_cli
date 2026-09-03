// Package applog provides the structured application log and the verbatim SIP
// trace (requirements §11, NFR-6 and NFR-7).
package applog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Logger bundles the app log with the console handler and the SIP tracer.
type Logger struct {
	*slog.Logger

	files  []io.Closer
	tracer *Tracer
}

func level(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Console is where console-level records are written. The CLI replaces this so
// log lines are printed above the prompt instead of through it (FR-9.4).
type Console interface {
	io.Writer
}

// Options configures New.
type Options struct {
	File         string
	Level        string
	ConsoleLevel string
	SIPTrace     bool
	SIPTraceFile string
	Console      io.Writer
}

// New opens the log files and builds the logger. Records at or above
// ConsoleLevel also go to Console.
func New(opt Options) (*Logger, error) {
	l := &Logger{}

	fh, err := os.OpenFile(opt.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", opt.File, err)
	}
	l.files = append(l.files, fh)

	handlers := []slog.Handler{
		slog.NewTextHandler(fh, &slog.HandlerOptions{Level: level(opt.Level)}),
	}
	if opt.Console != nil {
		handlers = append(handlers, &consoleHandler{
			w:     opt.Console,
			level: level(opt.ConsoleLevel),
		})
	}
	l.Logger = slog.New(&multiHandler{handlers: handlers})

	// The tracer always exists, so `debug on` can echo SIP packets to the
	// terminal even when file tracing is switched off in config.
	l.tracer = &Tracer{}
	if opt.SIPTrace {
		tf, err := os.OpenFile(opt.SIPTraceFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("open sip trace file %s: %w", opt.SIPTraceFile, err)
		}
		l.files = append(l.files, tf)
		l.tracer.file = tf
		l.tracer.fileOn.Store(true)
	}
	return l, nil
}

// Tracer returns the SIP tracer. It is never nil.
func (l *Logger) Tracer() *Tracer { return l.tracer }

func (l *Logger) Close() error {
	var first error
	for _, c := range l.files {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// multiHandler fans a record out to several handlers.
type multiHandler struct{ handlers []slog.Handler }

func (m *multiHandler) Enabled(ctx contextT, lvl slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx contextT, r slog.Record) error {
	var first error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: out}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: out}
}

// consoleHandler renders a short human line for the terminal.
type consoleHandler struct {
	w     io.Writer
	level slog.Level
	attrs []slog.Attr
	mu    sync.Mutex
}

func (c *consoleHandler) Enabled(_ contextT, lvl slog.Level) bool { return lvl >= c.level }

func (c *consoleHandler) Handle(_ contextT, r slog.Record) error {
	var b strings.Builder
	switch {
	case r.Level >= slog.LevelError:
		b.WriteString("error: ")
	case r.Level >= slog.LevelWarn:
		b.WriteString("warn: ")
	}
	b.WriteString(r.Message)
	write := func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	}
	for _, a := range c.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteString("\n")

	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := io.WriteString(c.w, b.String())
	return err
}

func (c *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &consoleHandler{w: c.w, level: c.level, attrs: append(slices(c.attrs), attrs...)}
}

func (c *consoleHandler) WithGroup(string) slog.Handler { return c }

func slices(in []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(in))
	copy(out, in)
	return out
}

// Tracer implements sip.SIPTracer, writing every SIP message verbatim with
// direction, transport, peer and timestamp (NFR-7).
//
// It has two independent sinks: the trace file, fixed at startup by
// logging.sip_trace, and the console, toggled at runtime by the `debug`
// command. Both are guarded by atomics rather than by flipping sipgo's global
// sip.SIPDebug, which the transport goroutines read without synchronisation.
type Tracer struct {
	mu   sync.Mutex
	file io.Writer

	fileOn    atomic.Bool
	consoleOn atomic.Bool

	consoleMu sync.Mutex
	console   io.Writer
}

// SetConsole installs the writer used when console tracing is on. The CLI
// passes a writer that prints above the prompt.
func (t *Tracer) SetConsole(w io.Writer) {
	t.consoleMu.Lock()
	t.console = w
	t.consoleMu.Unlock()
}

// EnableConsole turns terminal echo of SIP packets on or off.
func (t *Tracer) EnableConsole(on bool) { t.consoleOn.Store(on) }

// ConsoleEnabled reports whether SIP packets are echoed to the terminal.
func (t *Tracer) ConsoleEnabled() bool { return t.consoleOn.Load() }

// FileEnabled reports whether the trace file is being written.
func (t *Tracer) FileEnabled() bool { return t.fileOn.Load() }

// credRe blanks the digest response and nonces so the trace never carries a
// usable credential (NFR-7).
var credRe = regexp.MustCompile(`(?i)(response|nonce|cnonce)="[^"]*"`)

func (t *Tracer) write(dir, transport, laddr, raddr string, msg []byte) {
	fileOn, consoleOn := t.fileOn.Load(), t.consoleOn.Load()
	if !fileOn && !consoleOn {
		return // nothing to do: skip the redaction pass entirely
	}

	header := fmt.Sprintf("=== %s %s %s %s %s ===",
		time.Now().Format("15:04:05.000"), dir, transport, laddr, raddr)
	body := credRe.ReplaceAllString(string(msg), `$1="REDACTED"`)
	record := header + "\n" + body + "\n"

	if fileOn && t.file != nil {
		t.mu.Lock()
		fmt.Fprint(t.file, record)
		t.mu.Unlock()
	}

	if consoleOn {
		t.consoleMu.Lock()
		w := t.console
		t.consoleMu.Unlock()
		if w != nil {
			_, _ = io.WriteString(w, record)
		}
	}
}

func (t *Tracer) SIPTraceRead(transport, laddr, raddr string, msg []byte) {
	t.write("RECV", transport, laddr, "<- "+raddr, msg)
}

func (t *Tracer) SIPTraceWrite(transport, laddr, raddr string, msg []byte) {
	t.write("SEND", transport, laddr, "-> "+raddr, msg)
}

type contextT = context.Context
