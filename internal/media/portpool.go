package media

import (
	"fmt"
	"sync"
	"time"
)

// quarantine is how long a released RTP port is withheld so stray packets from
// the old call cannot land in a new session (FR-3.6).
const quarantine = time.Second

// PortPool hands out even-numbered RTP ports; RTCP uses port+1.
//
// A configured range routinely has holes: other software on the host may hold
// ports inside it (a container runtime forwarding a PBX's own RTP range is the
// common case). Ports that fail to bind are marked unusable and skipped for
// the life of the process, so a hole costs one failed bind rather than every
// subsequent call.
type PortPool struct {
	mu sync.Mutex
	// ephemeral means no range was configured: Acquire yields 0 and the OS
	// assigns a free port at bind time, which cannot collide.
	ephemeral bool
	start     int
	end       int
	inUse     map[int]bool
	freed     map[int]time.Time
	unusable  map[int]bool
}

// NewPortPool builds a pool over the configured inclusive range.
func NewPortPool(start, end int) *PortPool {
	return &PortPool{
		ephemeral: start == 0 && end == 0,
		start:     start, end: end,
		inUse:    make(map[int]bool),
		freed:    make(map[int]time.Time),
		unusable: make(map[int]bool),
	}
}

// Acquire reserves an even port with a free successor for RTCP. In ephemeral
// mode it returns 0, meaning "bind to any free port".
func (p *PortPool) Acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ephemeral {
		return 0, nil
	}

	first := p.start
	if first%2 != 0 {
		first++
	}
	now := time.Now()
	for port := first; port+1 <= p.end; port += 2 {
		if p.inUse[port] || p.unusable[port] {
			continue
		}
		if until, held := p.freed[port]; held {
			if now.Sub(until) < quarantine {
				continue
			}
			delete(p.freed, port)
		}
		p.inUse[port] = true
		return port, nil
	}
	if len(p.unusable) > 0 {
		return 0, fmt.Errorf(
			"no free RTP port in range %d-%d (%d port(s) in the range are held by "+
				"another process; widen media.rtp_port_start/end or move the range)",
			p.start, p.end, len(p.unusable)*2)
	}
	return 0, fmt.Errorf("no free RTP port in range %d-%d", p.start, p.end)
}

// MarkUnusable excludes a port pair that could not be bound, so it is not
// offered again.
func (p *PortPool) MarkUnusable(port int) {
	if port == 0 {
		return // nothing to exclude: the OS picks the port
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inUse, port)
	p.unusable[port] = true
}

// Ephemeral reports whether the OS assigns ports.
func (p *PortPool) Ephemeral() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ephemeral
}

// Unusable reports how many port pairs have been excluded.
func (p *PortPool) Unusable() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.unusable)
}

// Release returns a port to the pool after its quarantine.
func (p *PortPool) Release(port int) {
	if port == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ephemeral {
		// The OS owns the port; tracking it would grow the map for nothing.
		return
	}
	delete(p.inUse, port)
	p.freed[port] = time.Now()
}
