package telemetry

import (
	"net"
	"sync"
)

// connectionLimit rejects transport connections beyond the finite per-agent
// cap. A rejected connection closes promptly with a transport error; no OTLP
// request has been admitted, so no gRPC status can be attached to it.
type connectionLimit struct {
	net.Listener
	slots chan struct{}
}

func newConnectionLimit(inner net.Listener, limit int) *connectionLimit {
	return &connectionLimit{Listener: inner, slots: make(chan struct{}, limit)}
}

func (l *connectionLimit) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &connectionLimitConn{Conn: conn, release: func() { <-l.slots }}, nil
		default:
			_ = conn.Close()
		}
	}
}

type connectionLimitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *connectionLimitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
