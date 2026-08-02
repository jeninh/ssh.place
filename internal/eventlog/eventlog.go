// Package eventlog appends every accepted placement to a JSON Lines file so a
// timelapse can be rendered from it later.
package eventlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one line of the log.
type Event struct {
	At       time.Time `json:"t"`
	Identity string    `json:"id"`
	X        int       `json:"x"`
	Y        int       `json:"y"`
	Rune     string    `json:"r"`
	Color    uint8     `json:"c"`
	// Block is set when the placement was a solid block of Color rather than a
	// character, which a timelapse needs in order to replay it.
	Block bool `json:"block,omitempty"`
}

// Log is an append-only event log. A nil *Log is valid and discards
// everything, which lets the server run with logging switched off without the
// call sites growing a branch.
type Log struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
	// pending counts writes buffered since the last flush.
	pending int
}

// flushEvery bounds how many events can be lost to a hard kill.
const flushEvery = 32

// Open opens path for appending, creating it and its directory if needed.
func Open(path string) (*Log, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create event log dir: %w", err)
		}
	}
	// 0600: the log pairs key fingerprints with addresses, so it is nobody
	// else's business on a shared host.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	return &Log{f: f, w: bufio.NewWriter(f)}, nil
}

// Append records one placement.
func (l *Log) Append(e Event) error {
	if l == nil {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(line); err != nil {
		return err
	}
	if err := l.w.WriteByte('\n'); err != nil {
		return err
	}
	l.pending++
	if l.pending >= flushEvery {
		return l.flushLocked()
	}
	return nil
}

// Flush pushes buffered events to the file.
func (l *Log) Flush() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushLocked()
}

func (l *Log) flushLocked() error {
	if l.pending == 0 {
		return nil
	}
	l.pending = 0
	return l.w.Flush()
}

// Close flushes and closes the log.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.flushLocked()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}
