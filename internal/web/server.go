package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"sipclient/internal/channel"
	"sipclient/internal/config"
	"sipclient/internal/control"
)

//go:embed static
var staticFiles embed.FS

// Server is the browser control surface.
type Server struct {
	cfg config.Web
	ctl *control.Controller
	log *slog.Logger

	ln   net.Listener
	http *http.Server

	clientsMu sync.Mutex
	clients   map[*wsClient]struct{}

	unsubscribeEvents func()
	unsubscribeChange func()

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// New builds the server. It does not listen until Start.
func New(cfg config.Web, ctl *control.Controller, logger *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		ctl:     ctl,
		log:     logger.With("component", "web"),
		clients: make(map[*wsClient]struct{}),
		done:    make(chan struct{}),
	}
}

// Start binds the listener and serves until ctx is done. It returns the URL
// the UI is reachable at, which the caller shows the operator -- with port 0
// there is no way to guess it.
func (s *Server) Start(ctx context.Context, addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("web: listen on %s: %w", addr, err)
	}
	s.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.Handle("GET /static/", http.FileServerFS(mustSubFS()))
	mux.HandleFunc("GET /{$}", s.handleIndex)

	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Subscribe before serving, so no event is missed by an early client.
	events, unsubEvents := s.ctl.Manager().Subscribe()
	s.unsubscribeEvents = unsubEvents
	s.unsubscribeChange = s.ctl.Manager().AddChangeListener(s.broadcastState)

	s.wg.Add(2)
	go s.pumpEvents(events)
	go func() {
		defer s.wg.Done()
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("web server stopped", "error", err)
		}
	}()

	url := "http://" + ln.Addr().String()
	return url, nil
}

// Close shuts the server down and disconnects every browser.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.unsubscribeChange != nil {
			s.unsubscribeChange()
		}
		if s.unsubscribeEvents != nil {
			s.unsubscribeEvents()
		}

		s.clientsMu.Lock()
		for c := range s.clients {
			c.close()
		}
		s.clients = make(map[*wsClient]struct{})
		s.clientsMu.Unlock()

		if s.http != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.http.Shutdown(shutdownCtx)
		}
		s.wg.Wait()
	})
}

func mustSubFS() fs.FS {
	sub, err := fs.Sub(staticFiles, ".")
	if err != nil {
		panic(err) // the embed is compiled in; this cannot fail at runtime
	}
	return sub
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

// handleState serves one-shot state, for a page load before the socket is up.
func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.ctl.Snapshot())
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "UI not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The UI is embedded and changes only with the binary, but a stale cached
	// page against a new protocol is a confusing failure.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}

// pumpEvents relays manager events to every connected browser.
func (s *Server) pumpEvents(events <-chan channel.Event) {
	defer s.wg.Done()

	for {
		select {
		case <-s.done:
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			s.broadcast(EventMessage{
				Type:    TypeEvent,
				Kind:    e.Kind.String(),
				Channel: e.Channel,
				Text:    e.Text,
			})
			// An event nearly always accompanies a state change; push both so
			// the UI never shows a notice that disagrees with the channels.
			s.broadcastState()
		}
	}
}

func (s *Server) broadcastState() {
	s.broadcast(StateMessage{Type: TypeState, State: s.ctl.Snapshot()})
}

// broadcast sends a message to every connected browser.
func (s *Server) broadcast(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.log.Error("cannot encode message for the browser", "error", err)
		return
	}

	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for c := range s.clients {
		c.enqueue(data)
	}
}

// ClientCount reports how many browsers are connected.
func (s *Server) ClientCount() int {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	return len(s.clients)
}

// handleWS upgrades a connection and serves it until it closes.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Accept's default rejects an Origin that does not match Host, so a page
	// the operator visits elsewhere cannot drive their phone.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		s.log.Warn("websocket upgrade refused", "error", err, "origin", r.Header.Get("Origin"))
		return
	}

	c := newWSClient(conn)
	s.clientsMu.Lock()
	s.clients[c] = struct{}{}
	s.clientsMu.Unlock()

	s.log.Info("browser connected", "clients", s.ClientCount())
	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, c)
		s.clientsMu.Unlock()
		c.close()
		s.log.Info("browser disconnected", "clients", s.ClientCount())
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The first frame is the full state, so the page renders immediately.
	if data, err := json.Marshal(StateMessage{Type: TypeState, State: s.ctl.Snapshot()}); err == nil {
		c.enqueue(data)
	}

	go c.writeLoop(ctx)
	s.readLoop(ctx, c)
}

// readLoop dispatches commands until the socket closes.
func (s *Server) readLoop(ctx context.Context, c *wsClient) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return // closed, or the context ended
		}
		if typ != websocket.MessageText {
			continue
		}

		var cmd Command
		if err := json.Unmarshal(data, &cmd); err != nil {
			s.replyErr(c, "", fmt.Errorf("malformed message: %w", err))
			continue
		}
		if cmd.Type != TypeCommand {
			s.replyErr(c, cmd.ID, fmt.Errorf("unknown message type %q", cmd.Type))
			continue
		}

		// Commands run on their own goroutine: dialling and transferring block
		// on the network, and a stalled command must not stop the browser
		// sending a hangup.
		go s.runCommand(ctx, c, cmd)
	}
}

func (s *Server) replyErr(c *wsClient, id string, err error) {
	s.reply(c, ResultMessage{Type: TypeResult, ID: id, OK: false, Error: err.Error()})
}

func (s *Server) reply(c *wsClient, res ResultMessage) {
	if data, err := json.Marshal(res); err == nil {
		c.enqueue(data)
	}
}
