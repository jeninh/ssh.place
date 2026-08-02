package tui

import "sync"

// Exit carries the reason a session ended from the model out to the SSH
// middleware, which prints it after bubbletea has left the alternate screen.
// A plain field would be written by the program's goroutine and read by the
// middleware's, so it is guarded.
type Exit struct {
	mu     sync.Mutex
	reason string
}

// Set records the reason the session is ending.
func (e *Exit) Set(reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.reason = reason
	e.mu.Unlock()
}

// Reason returns the recorded reason, or "" if the session ended normally.
func (e *Exit) Reason() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reason
}
