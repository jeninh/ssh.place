package server

import (
	"net"
	"sync"
	"testing"
	"time"
)

func TestLimitListenerCapsConcurrentConnections(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()

	const max = 3
	ln := newLimitListener(inner, max)

	// Accept in the background, holding every connection open.
	var (
		mu       sync.Mutex
		accepted []net.Conn
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
		}
	}()

	// Dial more than the cap. The extras sit in the backlog rather than being
	// handed to the server.
	var dialed []net.Conn
	for i := 0; i < max+4; i++ {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		dialed = append(dialed, c)
	}
	defer func() {
		for _, c := range dialed {
			c.Close()
		}
	}()

	// Give the accept loop time to take everything it is allowed to.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(accepted)
		mu.Unlock()
		if n >= max {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	n := len(accepted)
	mu.Unlock()
	if n != max {
		t.Fatalf("accepted %d connections, want the cap of %d", n, max)
	}

	// Closing one hands its slot back, so the next connection gets through.
	mu.Lock()
	first := accepted[0]
	mu.Unlock()
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n = len(accepted)
		mu.Unlock()
		if n > max {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n <= max {
		t.Errorf("accepted %d after freeing a slot, want more than %d", n, max)
	}

	ln.Close()
	<-done
}

// Closing twice must not release the slot twice, or the cap drifts upwards.
func TestLimitConnCloseIsIdempotent(t *testing.T) {
	released := 0
	c := &limitConn{Conn: nopConn{}, release: func() { released++ }}
	_ = c.Close()
	_ = c.Close()
	_ = c.Close()
	if released != 1 {
		t.Errorf("release called %d times, want 1", released)
	}
}

func TestLimitListenerZeroMaxIsUnlimited(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	if got := newLimitListener(inner, 0); got != inner {
		t.Error("a zero cap should hand back the listener untouched")
	}
}

type nopConn struct{ net.Conn }

func (nopConn) Close() error { return nil }
