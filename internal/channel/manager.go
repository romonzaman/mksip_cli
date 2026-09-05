package channel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/emiago/sipgo/sip"

	"sipclient/internal/audio"
	"sipclient/internal/config"
	"sipclient/internal/media"
	"sipclient/internal/sipua"
)

// ChannelCount is fixed at two, which is the minimum for an attended transfer
// (§1.2: the model must not preclude more, but only two are required).
const ChannelCount = 2

// Manager owns both channels, tracks which one has the audio device and routes
// inbound SIP requests to the right one (requirements §5).
type Manager struct {
	cfg config.Config
	ua  *sipua.UA
	log *slog.Logger

	router   *audio.Router
	pool     *media.PortPool
	channels []*Channel

	mu       sync.RWMutex
	activeID int
	autoHold bool

	// subs are the event subscribers. There is more than one surface driving
	// this client -- the REPL and the web UI -- and a single shared channel
	// would let them steal events from each other, so each gets its own.
	subMu sync.Mutex
	subs  map[*subscriber]struct{}

	// changeListeners are notified after any state change, for surfaces that
	// re-render rather than react to individual events.
	changeMu        sync.Mutex
	changeListeners map[int]func()
	nextListenerID  int

	// notify routes REFER NOTIFYs to the transfer waiting on that dialog.
	notifyMu sync.Mutex
	notifies map[string]func(*sip.Request)
}

// subscriber is one consumer of the event stream.
type subscriber struct {
	ch chan Event
	// dropped counts events discarded because this consumer fell behind. A
	// slow consumer must never block the manager or the other consumers.
	dropped uint64
}

// NewManager builds the channel set. The UA is attached later with SetUA so
// the manager's handlers can be given to sipua.New.
func NewManager(cfg config.Config, logger *slog.Logger, router *audio.Router) *Manager {
	m := &Manager{
		cfg:             cfg,
		log:             logger,
		router:          router,
		pool:            media.NewPortPool(cfg.Media.RTPPortStart, cfg.Media.RTPPortEnd),
		activeID:        1,
		autoHold:        true,
		subs:            make(map[*subscriber]struct{}),
		changeListeners: make(map[int]func()),
		notifies:        make(map[string]func(*sip.Request)),
	}
	return m
}

// SetUA wires the SIP stack and creates the channels.
func (m *Manager) SetUA(ua *sipua.UA) {
	m.ua = ua
	for id := 1; id <= ChannelCount; id++ {
		leg := m.router.AddLeg(id)
		m.channels = append(m.channels, New(id, m.cfg, ua, m.pool, leg, m.log,
			m.emit, m.stateChanged))
	}
	m.applyActive()
}

// eventBuffer is how many events a subscriber may fall behind by. Beyond it,
// events are dropped for that subscriber only.
const eventBuffer = 64

// Subscribe returns an event stream and a function that closes it. Every
// subscriber receives every event (FR-9.4); one falling behind loses its own
// events and affects no one else.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, eventBuffer)}

	m.subMu.Lock()
	m.subs[sub] = struct{}{}
	m.subMu.Unlock()

	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			m.subMu.Lock()
			delete(m.subs, sub)
			m.subMu.Unlock()
			close(sub.ch)
		})
	}
}

// AddChangeListener registers a callback fired after any state change, and
// returns a function that removes it.
func (m *Manager) AddChangeListener(f func()) func() {
	m.changeMu.Lock()
	id := m.nextListenerID
	m.nextListenerID++
	m.changeListeners[id] = f
	m.changeMu.Unlock()

	return func() {
		m.changeMu.Lock()
		delete(m.changeListeners, id)
		m.changeMu.Unlock()
	}
}

// emit delivers an event to every subscriber without blocking.
func (m *Manager) emit(e Event) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	for sub := range m.subs {
		select {
		case sub.ch <- e:
		default:
			sub.dropped++
			// Log the first drop per subscriber; after that it would be noise.
			if sub.dropped == 1 {
				m.log.Warn("event consumer is behind, dropping notifications",
					"text", e.Text)
			}
		}
	}
}

// SubscriberCount reports how many event consumers are attached.
func (m *Manager) SubscriberCount() int {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	return len(m.subs)
}

func (m *Manager) stateChanged() {
	m.applyActive()

	m.changeMu.Lock()
	listeners := make([]func(), 0, len(m.changeListeners))
	for _, f := range m.changeListeners {
		listeners = append(listeners, f)
	}
	m.changeMu.Unlock()

	for _, f := range listeners {
		f()
	}
}

// Channels returns both channels in order.
func (m *Manager) Channels() []*Channel { return m.channels }

// Get resolves a channel number.
func (m *Manager) Get(id int) (*Channel, error) {
	if id < 1 || id > len(m.channels) {
		return nil, fmt.Errorf("no channel %d (valid: 1-%d)", id, len(m.channels))
	}
	return m.channels[id-1], nil
}

// ActiveID is the channel currently holding the audio device.
func (m *Manager) ActiveID() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeID
}

// Active returns the active channel.
func (m *Manager) Active() *Channel {
	ch, _ := m.Get(m.ActiveID())
	return ch
}

// Resolve returns the named channel, or the active one when id is 0 (FR-3.5).
func (m *Manager) Resolve(id int) (*Channel, error) {
	if id == 0 {
		return m.Active(), nil
	}
	return m.Get(id)
}

// SetActive makes a channel the audio-connected one, holding the previous
// active channel when auto-hold is on (FR-3.4).
func (m *Manager) SetActive(ctx context.Context, id int) error {
	target, err := m.Get(id)
	if err != nil {
		return err
	}

	m.mu.Lock()
	previous := m.activeID
	m.activeID = id
	autoHold := m.autoHold
	m.mu.Unlock()

	if autoHold && previous != id {
		if prev, err := m.Get(previous); err == nil {
			s := prev.Snapshot()
			if s.State == Connected && !s.LocalHeld {
				if err := prev.Hold(ctx); err != nil {
					m.log.Warn("auto-hold failed", "channel", previous, "error", err)
				}
			}
		}
	}

	m.applyActive()
	_ = target
	return nil
}

// applyActive reconciles every channel's audio attachment.
func (m *Manager) applyActive() {
	active := m.ActiveID()
	for _, ch := range m.channels {
		ch.SetActive(ch.ID == active)
	}
}

// Swap holds the active channel and retrieves the other, the core gesture
// during a warm transfer (FR-4.9).
func (m *Manager) Swap(ctx context.Context) error {
	if len(m.channels) != 2 {
		return fmt.Errorf("swap needs exactly two channels")
	}

	from := m.Active()
	var to *Channel
	for _, ch := range m.channels {
		if ch.ID != from.ID {
			to = ch
		}
	}

	if !to.State().InCall() {
		return fmt.Errorf("channel %d has no call to swap to (%s)", to.ID, to.State())
	}

	if s := from.Snapshot(); s.State == Connected && !s.LocalHeld {
		if err := from.Hold(ctx); err != nil {
			return fmt.Errorf("hold channel %d: %w", from.ID, err)
		}
	}

	m.mu.Lock()
	m.activeID = to.ID
	m.mu.Unlock()

	if s := to.Snapshot(); s.LocalHeld {
		if err := to.Unhold(ctx); err != nil {
			return fmt.Errorf("retrieve channel %d: %w", to.ID, err)
		}
	}
	m.applyActive()
	m.emit(Event{Text: fmt.Sprintf("audio now on channel %d", to.ID)})
	return nil
}

// FirstIdle returns the lowest-numbered idle channel, or nil when both are busy.
func (m *Manager) FirstIdle() *Channel {
	for _, ch := range m.channels {
		if ch.State() == Idle {
			return ch
		}
	}
	return nil
}

// HangupAll clears every channel (FR-4.6).
func (m *Manager) HangupAll(ctx context.Context) {
	for _, ch := range m.channels {
		if ch.State().Busy() {
			if err := ch.Hangup(ctx); err != nil {
				m.log.Debug("hangup during clear-all", "channel", ch.ID, "error", err)
			}
		}
	}
}

// SetMuted mutes the microphone globally.
func (m *Manager) SetMuted(muted bool) {
	m.router.SetMuted(muted)
	for _, ch := range m.channels {
		_ = ch.SetMuted(muted)
	}
}

// Muted reports the microphone state.
func (m *Manager) Muted() bool { return m.router.Muted() }

// RegisterNotify routes NOTIFYs on a dialog to a transfer watcher (FR-5.4).
func (m *Manager) RegisterNotify(callID string, handler func(*sip.Request)) func() {
	m.notifyMu.Lock()
	m.notifies[callID] = handler
	m.notifyMu.Unlock()

	return func() {
		m.notifyMu.Lock()
		delete(m.notifies, callID)
		m.notifyMu.Unlock()
	}
}

// Handlers returns the inbound SIP request handlers for the UA.
func (m *Manager) Handlers() sipua.Handlers {
	return sipua.Handlers{
		OnInvite: m.onInvite,
		OnAck:    m.onAck,
		OnBye:    m.onBye,
		OnCancel: m.onCancel,
		OnNotify: m.onNotify,
		OnInfo:   m.onInfo,
	}
}

// findByDialog matches an in-dialog request to a channel. The tag orientation
// works for both call directions: a peer's in-dialog request always carries our
// tag in To and theirs in From.
func (m *Manager) findByDialog(req *sip.Request) *Channel {
	callID := req.CallID()
	to, from := req.To(), req.From()
	if callID == nil || to == nil || from == nil {
		return nil
	}
	localTag, hasLocal := to.Params.Get("tag")
	remoteTag, hasRemote := from.Params.Get("tag")
	if !hasLocal || !hasRemote {
		return nil
	}

	for _, ch := range m.channels {
		info, _, ok := ch.DialogInfo()
		if !ok {
			continue
		}
		if info.CallID == callID.Value() &&
			info.LocalTag == localTag && info.RemoteTag == remoteTag {
			return ch
		}
	}
	return nil
}

func (m *Manager) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	// An INVITE inside an existing dialog is a re-INVITE: hold, retrieve or a
	// session refresh (FR-4.8, FR-4.11).
	if ch := m.findByDialog(req); ch != nil {
		ch.HandleReInvite(req, tx)
		return
	}

	// Only accept calls while registered (FR-2.7).
	if !m.ua.Registered() {
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil))
		return
	}

	ch := m.FirstIdle()
	if ch == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBusyHere, "Busy Here", nil))
		m.emit(Event{Kind: EventInfo,
			Text: "rejected inbound call with 486 Busy Here: both channels in use"})
		return
	}

	dlg, err := m.ua.DialogUA().ReadInvite(req, tx)
	if err != nil {
		m.log.Warn("cannot create inbound dialog", "error", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest,
			"Bad Request", nil))
		return
	}

	if err := ch.Incoming(context.Background(), dlg); err != nil {
		_ = dlg.Respond(sip.StatusBusyHere, "Busy Here", nil)
		_ = dlg.Close()
		return
	}

	s := ch.Snapshot()
	m.emit(Event{Kind: EventIncoming, Channel: ch.ID,
		Text: fmt.Sprintf("from %s -- `answer %d` or `reject %d`", s.Remote, ch.ID, ch.ID)})

	// Hold the INVITE server transaction open until the operator answers or
	// declines. sipgo terminates it as soon as this handler returns, and each
	// request is dispatched on its own goroutine, so blocking here is both
	// required and safe.
	ch.WaitInboundDecision(context.Background())
}

func (m *Manager) onAck(req *sip.Request, tx sip.ServerTransaction) {
	if ch := m.findByDialog(req); ch != nil {
		ch.HandleAck(req, tx)
	}
}

func (m *Manager) onBye(req *sip.Request, tx sip.ServerTransaction) {
	ch := m.findByDialog(req)
	if ch == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req,
			sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
		return
	}
	ch.HandleBye(req, tx)
}

func (m *Manager) onCancel(req *sip.Request, tx sip.ServerTransaction) {
	// CANCEL matches the INVITE transaction, which has no To tag yet, so match
	// on Call-ID alone across ringing channels.
	callID := req.CallID()
	if callID == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}
	for _, ch := range m.channels {
		if ch.MatchesCallID(callID.Value()) {
			ch.HandleCancel(req, tx)
			return
		}
	}
	_ = tx.Respond(sip.NewResponseFromRequest(req,
		sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil))
}

func (m *Manager) onNotify(req *sip.Request, tx sip.ServerTransaction) {
	sipua.RespondNotify(req, tx)

	callID := req.CallID()
	if callID == nil {
		return
	}
	m.notifyMu.Lock()
	handler := m.notifies[callID.Value()]
	m.notifyMu.Unlock()

	if handler != nil {
		handler(req)
	}
}

func (m *Manager) onInfo(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
}

// UA exposes the SIP stack for the transfer package.
func (m *Manager) UA() *sipua.UA { return m.ua }
