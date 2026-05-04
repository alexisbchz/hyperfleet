package vmmgr

import (
	"errors"
	"io"
	"sync"
)

const (
	consoleHistoryBytes = 64 * 1024
	consoleSubBuffer    = 64
)

// Console multiplexes a microVM's serial console between many subscribers.
// Stdin from any attached subscriber is forwarded to the VM (last-writer-wins).
// Stdout from the VM is broadcast to every subscriber, with a small ring of
// recent history so newly-attached subscribers see what they just missed.
type Console struct {
	stdin io.WriteCloser

	mu      sync.Mutex
	history []byte
	subs    map[chan []byte]struct{}
	closed  bool
}

func newConsole(stdin io.WriteCloser, stdout io.Reader) *Console {
	c := &Console{
		stdin: stdin,
		subs:  make(map[chan []byte]struct{}),
	}
	go c.pump(stdout)
	return c
}

func (c *Console) pump(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			c.mu.Lock()
			c.history = appendRing(c.history, chunk, consoleHistoryBytes)
			for ch := range c.subs {
				select {
				case ch <- chunk:
				default:
					// slow subscriber drops data
				}
			}
			c.mu.Unlock()
		}
		if err != nil {
			c.mu.Lock()
			c.closed = true
			for ch := range c.subs {
				close(ch)
			}
			c.subs = nil
			c.mu.Unlock()
			return
		}
	}
}

func (c *Console) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for ch := range c.subs {
		close(ch)
	}
	c.subs = nil
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
}

// Attach returns a ReadWriteCloser whose Read drains recent history then
// streams new console output, and whose Write sends to the VM stdin.
// The caller must Close the attachment when done.
func (c *Console) Attach() (io.ReadWriteCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("console closed")
	}
	a := &attachment{
		console: c,
		ch:      make(chan []byte, consoleSubBuffer),
		pending: append([]byte(nil), c.history...),
	}
	c.subs[a.ch] = struct{}{}
	return a, nil
}

type attachment struct {
	console *Console
	ch      chan []byte

	readMu  sync.Mutex
	pending []byte
	dead    bool
}

func (a *attachment) Read(p []byte) (int, error) {
	a.readMu.Lock()
	defer a.readMu.Unlock()

	for len(a.pending) == 0 {
		if a.dead {
			return 0, io.EOF
		}
		chunk, ok := <-a.ch
		if !ok {
			a.dead = true
			return 0, io.EOF
		}
		a.pending = chunk
	}
	n := copy(p, a.pending)
	a.pending = a.pending[n:]
	return n, nil
}

func (a *attachment) Write(p []byte) (int, error) {
	if a.console.stdin == nil {
		return 0, errors.New("console has no stdin")
	}
	return a.console.stdin.Write(p)
}

func (a *attachment) Close() error {
	a.console.mu.Lock()
	defer a.console.mu.Unlock()
	if _, ok := a.console.subs[a.ch]; ok {
		delete(a.console.subs, a.ch)
		close(a.ch)
	}
	return nil
}

func appendRing(history, chunk []byte, max int) []byte {
	if len(chunk) >= max {
		return append(history[:0], chunk[len(chunk)-max:]...)
	}
	combined := append(history, chunk...)
	if len(combined) > max {
		combined = combined[len(combined)-max:]
	}
	return combined
}
