// Package sipua owns the SIP stack: transport, registration and the helpers for
// sending requests inside an established dialog (requirements §4, §7).
package sipua

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"sipclient/internal/config"
)

// Handlers receives inbound requests. The channel manager supplies them.
type Handlers struct {
	OnInvite func(req *sip.Request, tx sip.ServerTransaction)
	OnAck    func(req *sip.Request, tx sip.ServerTransaction)
	OnBye    func(req *sip.Request, tx sip.ServerTransaction)
	OnCancel func(req *sip.Request, tx sip.ServerTransaction)
	OnNotify func(req *sip.Request, tx sip.ServerTransaction)
	OnInfo   func(req *sip.Request, tx sip.ServerTransaction)
}

// UA is the SIP user agent.
type UA struct {
	cfg config.Config
	log *slog.Logger

	ua     *sipgo.UserAgent
	client *sipgo.Client
	server *sipgo.Server
	dialog *sipgo.DialogUA

	// signalHost is what we put in Via, Contact and SDP: the public address
	// when NAT is configured, otherwise the local interface address.
	signalHost string
	localHost  string
	// listenHost is what the sockets bind to, which is deliberately not the
	// same as signalHost: we advertise a routable address but accept traffic
	// arriving by any path.
	listenHost string
	localPort  int

	contact sip.ContactHeader

	reg      registrar
	handlers Handlers
}

// New builds the user agent. It does not open any socket yet.
func New(cfg config.Config, logger *slog.Logger, tracer sip.SIPTracer, handlers Handlers) (*UA, error) {
	u := &UA{cfg: cfg, log: logger, handlers: handlers}

	if tracer != nil {
		sip.SIPDebugTracer(tracer)
		sip.SIPDebug = true
	}
	sip.SetDefaultLogger(logger)

	local, err := resolveLocalHost(cfg)
	if err != nil {
		return nil, err
	}
	u.localHost = local

	// Bind to all interfaces unless told otherwise. A PBX or proxy may reach
	// this host by a different route than the one we used to reach it -- on a
	// machine with several bridges that is the normal case -- and a socket
	// bound to one address refuses everything else, so an incoming call simply
	// never appears.
	u.listenHost = cfg.Network.ListenAddress
	if u.listenHost == "" {
		u.listenHost = "0.0.0.0"
	}

	// Resolve port 0 to a concrete free port before building the Contact.
	// Advertising an unresolved 0 would omit the port entirely, and peers would
	// then send in-dialog requests (ACK, BYE, NOTIFY) to the default 5060,
	// where nothing is listening.
	u.localPort = cfg.Network.LocalSIPPort
	if u.localPort == 0 {
		u.localPort, err = freePort(cfg.SIP.Server.Transport, u.listenHost)
		if err != nil {
			return nil, err
		}
		logger.Info("selected local SIP port", "port", u.localPort)
	}

	u.signalHost = local
	switch {
	case cfg.Network.PublicAddress != "":
		u.signalHost = cfg.Network.PublicAddress
		logger.Info("using configured public address", "address", u.signalHost)
	case cfg.Network.STUNServer != "":
		mapped, err := discoverPublicAddress(cfg.Network.STUNServer, 3*time.Second)
		if err != nil {
			// STUN is a convenience; a LAN PBX does not need it (open question Q2).
			logger.Warn("STUN discovery failed, using local address",
				"server", cfg.Network.STUNServer, "error", err)
		} else {
			u.signalHost = mapped
			logger.Info("STUN discovered public address", "address", mapped)
		}
	}

	uaOpts := []sipgo.UserAgentOption{
		sipgo.WithUserAgent(cfg.SIP.UserAgent),
		sipgo.WithUserAgentHostname(cfg.SIP.Domain),
	}
	u.ua, err = sipgo.NewUA(uaOpts...)
	if err != nil {
		return nil, fmt.Errorf("sip: create user agent: %w", err)
	}

	clientOpts := []sipgo.ClientOption{
		sipgo.WithClientLogger(logger),
		// Hostname and port set what goes in Via.
		sipgo.WithClientHostname(u.signalHost),
		sipgo.WithClientPort(u.localPort),
	}

	// Note on sockets: sipgo sends requests from their own socket rather than
	// from the listener. Via therefore advertises the listener port while the
	// packet source is ephemeral, which is exactly what ;rport exists for --
	// responses come back to the source, and any peer that ignores rport and
	// replies to the Via port reaches the listener instead. Both work.
	//
	// Binding requests to the listener (WithClientConnectionAddr) was tried and
	// removed: it cannot be combined with a wildcard listener, because sipgo
	// keys its connection pool by the literal bind address and so tries to bind
	// the port a second time. Accepting inbound calls on every interface is
	// worth more than making the source port match.
	if cfg.Network.Rport {
		clientOpts = append(clientOpts, sipgo.WithClientNAT())
	}
	u.client, err = sipgo.NewClient(u.ua, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("sip: create client: %w", err)
	}

	u.server, err = sipgo.NewServer(u.ua, sipgo.WithServerLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("sip: create server: %w", err)
	}

	u.contact = sip.ContactHeader{
		DisplayName: cfg.SIP.DisplayName,
		Address: sip.Uri{
			Scheme: "sip",
			User:   cfg.SIP.Username,
			Host:   u.signalHost,
			Port:   u.localPort,
		},
	}
	if cfg.SIP.Server.Transport == "tcp" {
		u.contact.Address.UriParams = sip.NewParams()
		u.contact.Address.UriParams.Add("transport", "tcp")
	}

	u.dialog = &sipgo.DialogUA{
		Client:         u.client,
		ContactHDR:     u.contact,
		RewriteContact: cfg.Network.Rport || cfg.Network.PublicAddress != "",
	}

	u.reg.init(u)
	u.registerHandlers()
	return u, nil
}

// resolveLocalHost picks the address we bind and advertise. When not configured
// it is discovered by looking up the route toward the SIP server.
func resolveLocalHost(cfg config.Config) (string, error) {
	if cfg.Network.LocalAddress != "" {
		return cfg.Network.LocalAddress, nil
	}
	conn, err := net.Dial("udp", cfg.ServerAddr())
	if err != nil {
		return "", fmt.Errorf("network.local_address: cannot determine route to %s: %w",
			cfg.ServerAddr(), err)
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "", fmt.Errorf("network.local_address: %w", err)
	}
	return host, nil
}

// freePort asks the OS for an unused port so it can be advertised in Contact.
func freePort(transport, host string) (int, error) {
	if transport == "tcp" {
		l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return 0, fmt.Errorf("network.local_sip_port: cannot find a free TCP port: %w", err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port, nil
	}

	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host)})
	if err != nil {
		return 0, fmt.Errorf("network.local_sip_port: cannot find a free UDP port: %w", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port, nil
}

func (u *UA) registerHandlers() {
	bind := func(set func(sipgo.RequestHandler), h func(*sip.Request, sip.ServerTransaction)) {
		if h == nil {
			return
		}
		set(func(req *sip.Request, tx sip.ServerTransaction) {
			// A panic in one handler must not take down the other channel (NFR-9).
			defer u.recoverHandler(req)
			h(req, tx)
		})
	}
	bind(u.server.OnInvite, u.handlers.OnInvite)
	bind(u.server.OnAck, u.handlers.OnAck)
	bind(u.server.OnBye, u.handlers.OnBye)
	bind(u.server.OnCancel, u.handlers.OnCancel)
	bind(u.server.OnNotify, u.handlers.OnNotify)
	bind(u.server.OnInfo, u.handlers.OnInfo)

	// OPTIONS keepalives are answered so the PBX sees us as alive.
	u.server.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})

	// Inbound REFER is out of scope for v1: decline explicitly rather than
	// letting it time out (§1.2).
	u.server.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotImplemented,
			"Not Implemented", nil))
	})
}

func (u *UA) recoverHandler(req *sip.Request) {
	if r := recover(); r != nil {
		u.log.Error("panic in SIP handler",
			"method", string(req.Method), "panic", fmt.Sprint(r), "stack", stack())
	}
}

// Listen opens the signalling transports.
//
// Both UDP and TCP are served, whatever sip.server.transport says. That
// setting chooses how we send; it must not decide what we can receive, because
// a proxy may route an inbound call over either transport regardless of how we
// registered. Failing to listen on the configured transport is fatal; failing
// on the other one is only a warning.
func (u *UA) Listen(ctx context.Context) error {
	primary := u.cfg.SIP.Server.Transport
	secondary := "tcp"
	if primary == "tcp" {
		secondary = "udp"
	}

	if err := u.listenOn(ctx, primary); err != nil {
		return err
	}
	if err := u.listenOn(ctx, secondary); err != nil {
		u.log.Warn("could not also listen on the other transport; "+
			"inbound calls routed over it will not arrive",
			"network", secondary, "error", err)
	}
	return nil
}

// CheckAdvertisedReachable tests whether the address we put in Via, Contact
// and SDP can actually be connected to, and warns if it cannot.
//
// This catches a whole class of silent failure. An address discovered from the
// route to the SIP server is valid as a packet *source* but is not necessarily
// usable as a *destination*: the network address of a subnet (a .0 host in a
// /24) is the common case, and the kernel refuses connections to it. Outbound
// calls still work, and RTP still works because peers latch onto our source
// address, so everything looks healthy -- but no inbound INVITE and no refer
// NOTIFY can ever reach us, because both are sent to our Contact.
//
// The check is advisory: a failure here does not stop the client.
func (u *UA) CheckAdvertisedReachable() {
	// The structural check is always valid: a network or broadcast address
	// cannot accept connections no matter who is asking.
	if hint := unusableAddressReason(u.signalHost); hint != "" {
		u.log.Warn("the address we advertise cannot accept connections, "+
			"so inbound calls and transfer NOTIFYs will not arrive",
			"advertised", u.signalHost, "reason", hint,
			"fix", "set network.public_address to an address your PBX can reach")
		return
	}

	// Probing by dialling is only meaningful for an address we discovered
	// ourselves. A configured public_address is frequently a NAT or container
	// alias that is reachable from the PBX but not from this host, so testing
	// it here would report a failure that is not one.
	if u.cfg.Network.PublicAddress != "" {
		u.log.Debug("advertised address is configured; skipping the reachability probe",
			"advertised", u.signalHost)
		return
	}

	addr := net.JoinHostPort(u.signalHost, fmt.Sprint(u.localPort))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		_ = conn.Close()
		u.log.Debug("advertised address is reachable", "addr", addr)
		return
	}

	u.log.Warn("could not connect to the address we advertise; if your PBX "+
		"cannot reach it either, inbound calls and transfer NOTIFYs will not arrive",
		"advertised", addr,
		"error", err.Error(),
		"fix", "set network.public_address to an address your PBX can reach")
}

// unusableAddressReason names why an address cannot be a destination, when it
// is recognisably one of the standard cases.
func unusableAddressReason(host string) string {
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}

	// Find the interface this address belongs to and compare against its mask.
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || !n.IP.Equal(ip) {
				continue
			}
			network := v4.Mask(n.Mask)
			if network.Equal(v4) {
				return fmt.Sprintf(
					"it is the network address of %s on interface %s, "+
						"which cannot accept connections", n.String(), iface.Name)
			}
			// Broadcast address: every host bit set.
			bcast := make(net.IP, len(network))
			for i := range network {
				bcast[i] = network[i] | ^n.Mask[i]
			}
			if bcast.Equal(v4) {
				return fmt.Sprintf("it is the broadcast address of %s on interface %s",
					n.String(), iface.Name)
			}
		}
	}
	return ""
}

// listenOn starts one transport and reports a bind failure promptly.
func (u *UA) listenOn(ctx context.Context, network string) error {
	addr := net.JoinHostPort(u.listenHost, fmt.Sprint(u.localPort))

	ready := make(chan error, 1)
	go func() {
		err := u.server.ListenAndServe(ctx, network, addr)
		if err != nil && ctx.Err() == nil {
			u.log.Error("SIP transport stopped", "network", network, "error", err)
		}
		ready <- err
	}()

	// Give the listener a moment to fail fast on a port clash.
	select {
	case err := <-ready:
		if err != nil {
			return fmt.Errorf("sip: listen %s on %s: %w", network, addr, err)
		}
	case <-time.After(250 * time.Millisecond):
	}
	u.log.Info("SIP transport listening", "network", network, "addr", addr,
		"advertised", net.JoinHostPort(u.signalHost, fmt.Sprint(u.localPort)))
	return nil
}

// Close shuts the stack down.
func (u *UA) Close() {
	if u.client != nil {
		_ = u.client.Close()
	}
	if u.server != nil {
		_ = u.server.Close()
	}
	if u.ua != nil {
		_ = u.ua.Close()
	}
}

// Client exposes the sipgo client for in-dialog work.
func (u *UA) Client() *sipgo.Client { return u.client }

// DialogUA exposes the dialog factory.
func (u *UA) DialogUA() *sipgo.DialogUA { return u.dialog }

// Contact is the contact header we advertise.
func (u *UA) Contact() sip.ContactHeader { return u.contact }

// ContactURI renders the Contact address we advertise, for logging.
func (u *UA) ContactURI() string {
	addr := u.contact.Address
	return (&addr).String()
}

// MediaHost is the address to put in the SDP connection line. It defaults to
// the signalling address but can be overridden when RTP takes a different
// path to this host than SIP does.
func (u *UA) MediaHost() string {
	if a := u.cfg.Media.AdvertiseAddress; a != "" {
		return a
	}
	return u.signalHost
}

// SignalHost is the address we put in Via and Contact.
func (u *UA) SignalHost() string { return u.signalHost }

// LocalHost is the address we advertise.
func (u *UA) LocalHost() string { return u.localHost }

// ListenHost is the address the sockets bind to.
func (u *UA) ListenHost() string { return u.listenHost }

// ListenAddr renders the bound address, including the transports served.
func (u *UA) ListenAddr() string {
	return fmt.Sprintf("%s (udp+tcp)", net.JoinHostPort(u.listenHost, fmt.Sprint(u.localPort)))
}

// AdvertisedAddr renders what peers are told to contact.
func (u *UA) AdvertisedAddr() string {
	return net.JoinHostPort(u.signalHost, fmt.Sprint(u.localPort))
}

// AORUri is our address of record as a URI.
func (u *UA) AORUri() sip.Uri {
	return sip.Uri{Scheme: "sip", User: u.cfg.SIP.Username, Host: u.cfg.SIP.Domain}
}

// ResolveTarget expands a dial string into a URI. A bare extension gets our
// configured domain; a full URI is used as given (FR-4.1).
func (u *UA) ResolveTarget(target string) (sip.Uri, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return sip.Uri{}, fmt.Errorf("no dial target given")
	}
	if !strings.Contains(target, "@") && !strings.HasPrefix(strings.ToLower(target), "sip:") {
		target = fmt.Sprintf("sip:%s@%s", target, u.cfg.SIP.Domain)
	} else if !strings.HasPrefix(strings.ToLower(target), "sip:") {
		target = "sip:" + target
	}

	var uri sip.Uri
	if err := sip.ParseUri(target, &uri); err != nil {
		return sip.Uri{}, fmt.Errorf("invalid dial target %q: %w", target, err)
	}
	if uri.Host == "" {
		return sip.Uri{}, fmt.Errorf("invalid dial target %q: no host", target)
	}
	return uri, nil
}

// RouteDestination is where requests physically go: the outbound proxy when
// configured, otherwise the registrar (FR §3.3 outbound_proxy).
func (u *UA) RouteDestination() string {
	if p := u.cfg.SIP.OutboundProxy; p != "" {
		if _, _, err := net.SplitHostPort(p); err != nil {
			return net.JoinHostPort(p, fmt.Sprint(u.cfg.SIP.Server.Port))
		}
		return p
	}
	return u.cfg.ServerAddr()
}
