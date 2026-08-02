package server

import (
	"net"
	"sync"
)

// limitListener bounds how many connections are alive at once.
//
// The SSH library accepts in a bare loop and starts a goroutine per connection,
// and the session limit is only reached after a client has authenticated and
// asked for a terminal. Between those two points an unauthenticated flood is
// unbounded, so the cap goes on the listener where it applies from the first
// packet. Accept waits while the cap is reached, which leaves further
// connections in the kernel backlog rather than in this process.
type limitListener struct {
	net.Listener
	slots chan struct{}

	// closed releases an Accept that is waiting for a slot. Closing the inner
	// listener does not, so without this a saturated listener would never return
	// from Accept and shutdown would hang.
	closeOnce sync.Once
	closed    chan struct{}
}

func newLimitListener(inner net.Listener, max int) net.Listener {
	if max <= 0 {
		return inner
	}
	return &limitListener{
		Listener: inner,
		slots:    make(chan struct{}, max),
		closed:   make(chan struct{}),
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}

	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *limitListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// limitConn returns its slot when closed.
type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

// Close is idempotent: the library closes a connection from more than one place,
// and releasing a slot twice would let the cap drift upwards.
func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
