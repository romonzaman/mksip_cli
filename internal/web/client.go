package web

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// sendBuffer is how far behind a browser may fall before it is disconnected.
// A browser that cannot keep up with state pushes is not usable anyway, and
// dropping it is better than growing a queue forever.
const sendBuffer = 32

// writeTimeout bounds a single frame write, so one stalled browser cannot hold
// a goroutine open indefinitely.
const writeTimeout = 5 * time.Second

// wsClient is one connected browser.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte

	closeOnce sync.Once
	closed    chan struct{}
}

func newWSClient(conn *websocket.Conn) *wsClient {
	return &wsClient{
		conn:   conn,
		send:   make(chan []byte, sendBuffer),
		closed: make(chan struct{}),
	}
}

// enqueue queues a frame. It never blocks: a browser that has fallen behind is
// closed rather than allowed to stall the manager's event pump.
func (c *wsClient) enqueue(data []byte) {
	select {
	case <-c.closed:
	case c.send <- data:
	default:
		c.close()
	}
}

// writeLoop drains the queue onto the socket.
func (c *wsClient) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case data := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(writeCtx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *wsClient) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.CloseNow()
	})
}
