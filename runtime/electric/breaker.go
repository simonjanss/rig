package electric

import (
	"sync"
	"time"
)

// breaker stops asking a sync service that has stopped answering.
//
// Without it every shape request during an outage pays the full
// [Config.InitialTimeout] before it can be answered from anywhere else: ten
// seconds of a held goroutine, a held connection and a subscriber watching a
// spinner, to learn what the request before it already learned. With it the
// first few requests pay that and the rest are answered immediately — from a
// [Shape.Fallback] where there is one, and with the status there always was
// where there is not.
//
// It is deliberately about the sync service and not about a shape. The failure
// it watches for is one process being unreachable, which every shape shares, so
// counting per shape would mean each of them discovering the same outage
// separately and none of them being able to say the service is down.
//
// A failure is what the proxy can see: a refused connection, a read from the
// beginning hitting [Config.InitialTimeout], and a 5xx. Not a 4xx, which is a
// decision about one shape, and not a client hanging up, which says nothing
// about the service. A live poll is unbounded on purpose — a poll with nothing
// to report is supposed to hang — so a service that accepts connections and
// never answers is found by the reads from the beginning, which is the same
// reason that deadline exists.
//
// Three states, and the middle one is the whole trick: closed, asking normally;
// open, asking nothing; and open with the cooldown elapsed, where exactly one
// request is let through to find out. That one request is how it closes again —
// nothing polls, and a service that comes back is noticed by the next
// subscriber rather than by a goroutine of this package's own.
type breaker struct {
	// threshold is how many failures in a row open the circuit. Zero or less is
	// a circuit that never opens.
	threshold int
	// cooldown is how long it stays open before one request may test the
	// service.
	cooldown time.Duration

	mu       sync.Mutex
	failures int
	open     bool
	// retryAt is when the next request may be let through. It moves forward when
	// one is, so that a second does not follow it while the first is still
	// waiting to find out.
	retryAt time.Time
}

// allow reports whether to ask the sync service at all.
func (b *breaker) allow() bool {
	if b.threshold <= 0 {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.open {
		return true
	}
	now := time.Now()
	if now.Before(b.retryAt) {
		return false
	}
	b.retryAt = now.Add(b.cooldown)
	return true
}

// record says how an attempt went, and reports whether that changed the answer
// to whether the sync service is there.
//
// Only an attempt: a request the circuit refused to make says nothing about the
// service, and neither does a client that hung up. The caller decides which of
// those it has.
func (b *breaker) record(reachable bool) (changed bool) {
	if b.threshold <= 0 {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if reachable {
		b.failures = 0
		if b.open {
			b.open = false
			return true
		}
		return false
	}

	b.failures++
	// A failure while open is the test request having failed, which is not news:
	// the circuit stays open and the cooldown that allow set is already running.
	if !b.open && b.failures >= b.threshold {
		b.open = true
		b.retryAt = time.Now().Add(b.cooldown)
		return true
	}
	return false
}

// reachable reports the last thing the breaker learned: false once the circuit
// has opened, and true again as soon as one request has succeeded.
func (b *breaker) reachable() bool {
	if b.threshold <= 0 {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.open
}
