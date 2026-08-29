// Package notify is the example's one dependency that is not the database.
//
// It exists to show the shape of one: something built while the routes are
// wired, fed by a hook after a write commits, running work of its own in the
// background, and needing to stop in a particular order when the process does.
// A real application has several — a queue consumer, a metrics exporter, a
// search indexer — and they all look like this.
//
// What it does is deliberately dull: it collects a line per created todo and
// writes them out in batches, the way anything that talks to a slow service
// would rather than making a call per request.
package notify

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Notifier batches messages and flushes them on a timer.
type Notifier struct {
	out   io.Writer
	every time.Duration

	mu      sync.Mutex
	pending []string
	// closed stops Record from accepting more. It is separate from the loop
	// being stopped: the point of draining is that the two happen at different
	// moments.
	closed bool

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

// New builds a notifier. Nothing happens until Start.
func New(out io.Writer, every time.Duration) *Notifier {
	return &Notifier{
		out:     out,
		every:   every,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Start runs the flush loop until Close.
func (n *Notifier) Start() {
	go func() {
		defer close(n.stopped)

		ticker := time.NewTicker(n.every)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				n.flush()
			case <-n.stop:
				return
			}
		}
	}()
}

// Record adds a message to the next batch.
//
// It is called from a hook that runs after the write has committed, so it must
// not fail the request it belongs to and does not report anything. Once the
// notifier is draining it silently drops what it is given, which is the right
// answer for a process on its way out: the alternative is refusing a write that
// has already happened.
func (n *Notifier) Record(message string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed {
		return
	}
	n.pending = append(n.pending, message)
}

// StopRecording stops accepting messages, without flushing.
//
// This is the drain step: it runs while the server is still answering, so that
// the requests still in flight are the last things recorded. Anything already
// collected is written by Close.
func (n *Notifier) StopRecording(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.closed = true
	return nil
}

// Close writes what is left and stops the loop.
//
// It runs after the server has stopped answering, which is what makes the last
// batch complete: a flush before that would miss whatever the requests in
// flight went on to record.
func (n *Notifier) Close(ctx context.Context) error {
	n.stopOnce.Do(func() { close(n.stop) })

	select {
	case <-n.stopped:
	case <-ctx.Done():
		return fmt.Errorf("the flush loop did not stop: %w", ctx.Err())
	}

	return n.flush()
}

// Pending is how many messages are waiting, for tests and for a metric.
func (n *Notifier) Pending() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return len(n.pending)
}

func (n *Notifier) flush() error {
	n.mu.Lock()
	batch := n.pending
	n.pending = nil
	n.mu.Unlock()

	for _, message := range batch {
		if _, err := fmt.Fprintln(n.out, message); err != nil {
			return fmt.Errorf("write a notification: %w", err)
		}
	}
	return nil
}
