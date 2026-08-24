package throttle

import (
	"context"
	"sync"
	"time"
)

// Local is a process-local tally in front of a [Recorder].
//
// It exists because the obvious implementation is a trap. A limiter that writes
// to Postgres on every API call adds a round trip to the hot path and — worse —
// puts every request from one busy caller on one row, so the counter becomes the
// bottleneck exactly when traffic is highest. That is a self-inflicted outage
// under the load the limiter was bought to survive.
//
// So each process counts locally and reconciles periodically. Two facts make
// this sound rather than merely cheap:
//
// Within a bucket the count only rises. Nothing decrements, so a replica's own
// tally plus the last global total it saw is a *lower bound* on the truth.
// Refusing on that lower bound can never refuse somebody who is actually under
// their limit — so a caller who is over it is refused from memory, and however
// fast the requests arrive they cost at most one write per Interval. The
// requests an attacker sends are precisely the ones this must not spend a
// round trip on.
//
// The error is one-sided and bounded. What a replica cannot see is what the
// other replicas have counted since it last looked, so the limiter allows too
// much rather than too little, by at most one Interval of traffic per replica.
// For fair-use limiting that is the right trade. It is not the right trade for
// the auth limits, which is why those still count exact rows through [Postgres].
type Local struct {
	next Recorder
	cfg  LocalConfig

	mu    sync.Mutex
	state map[localKey]*localState
	// pruned is when dead buckets were last dropped from state. Without it the
	// map grows one entry per address that ever called, which is a memory leak
	// an attacker chooses the size of.
	pruned time.Time
}

// LocalConfig tunes how often a process tells the others what it has counted.
type LocalConfig struct {
	// Interval is how long a replica may hold increments before publishing
	// them. It is the overshoot, stated as time: the limiter can miss up to one
	// Interval of traffic per replica.
	//
	// Publishing on a schedule is not an optimisation, it is what makes the
	// limit a limit. Ten replicas each quietly counting a fifth of the budget
	// are each under it and together at twice it.
	//
	// Zero means one second.
	Interval time.Duration

	// Threshold is the fraction of a limit above which a caller is reconciled
	// on every request regardless of Interval, so enforcement tightens as
	// somebody approaches their limit rather than after they pass it.
	//
	// Zero means 0.5.
	Threshold float64

	// MaxKeys bounds the map. Past it, new keys go straight to the recorder
	// rather than being remembered — slower, and the alternative is a process
	// whose memory is chosen by whoever is calling.
	//
	// Zero means 100_000.
	MaxKeys int
}

type localKey struct {
	limit string
	kind  string
	value string
}

type localState struct {
	// bucket is which window the numbers below belong to. A different one means
	// they are stale rather than wrong.
	bucket time.Time
	// window is the limit's window, kept so that prune can tell how long this
	// bucket can still be counted for. Without it the sweep has to assume the
	// longest window any limit might have, and a minute's bucket is held for as
	// long as an hour's.
	window time.Duration
	// pending is what this replica has counted and not yet published.
	pending int
	// global is the last total the recorder reported, including other replicas.
	global int
	// flushedAt is when global was learned.
	flushedAt time.Time
	// flushing keeps one publisher per key: without it a busy key sends a
	// round trip per concurrent request at exactly the moment it can least
	// afford to.
	flushing bool
}

// NewLocal wraps a recorder with a process-local tally.
func NewLocal(next Recorder, cfg LocalConfig) *Local {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.5
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = 100_000
	}
	return &Local{next: next, cfg: cfg, state: make(map[localKey]*localState)}
}

// Incr implements [Recorder].
func (l *Local) Incr(ctx context.Context, limit Limit, key Key, now time.Time, delta int) (int, time.Time, error) {
	if limit.Window <= 0 {
		// Let the recorder underneath own the message, so there is one wording
		// for a limit nobody finished configuring.
		return l.next.Incr(ctx, limit, key, now, delta)
	}

	now = now.UTC()
	bucket := now.Truncate(limit.Window)
	resetAt := bucket.Add(limit.Window)

	k := localKey{limit.Name, key.Kind, key.Value}

	l.mu.Lock()
	l.prune(now)

	st, held := l.state[k]
	if !held {
		if len(l.state) >= l.cfg.MaxKeys {
			// Degrade to the recorder rather than growing. Correct and slow
			// beats fast and out of memory.
			l.mu.Unlock()
			return l.next.Incr(ctx, limit, key, now, delta)
		}
		st = &localState{bucket: bucket, window: limit.Window}
		l.state[k] = st
	}

	// A rollover makes the old numbers meaningless, but not the unpublished
	// ones: those were real requests, and the bucket they belong to is still
	// counted — as the weighted previous bucket — for one more window. Publish
	// them at an instant inside the bucket they happened in.
	var (
		carried    int
		carriedAt  time.Time
		rolledOver = st.bucket != bucket
	)
	if rolledOver {
		carried, carriedAt = st.pending, st.bucket
		*st = localState{bucket: bucket, window: limit.Window}
	}

	st.pending += delta
	estimate := st.global + st.pending

	// Already refusing. The estimate is a lower bound on the truth, so this
	// decision cannot become wrong by learning more, and reconciling would buy
	// nothing — while costing a write per request at the one moment a write per
	// request is least affordable, because the requests arriving are an attack.
	over := estimate > limit.Max

	due := st.pending > 0 && !st.flushing &&
		// Close to the limit: reconcile now, so the tightening happens before
		// the limit is passed rather than after.
		((!over && float64(estimate) >= l.cfg.Threshold*float64(limit.Max)) ||
			// Otherwise on the clock, so the other replicas learn about us. This
			// is the only branch a refused caller can reach, which bounds an
			// attack to one write per Interval however fast it arrives.
			now.Sub(st.flushedAt) >= l.cfg.Interval)

	if !due {
		l.mu.Unlock()
		if carried > 0 {
			l.publish(ctx, limit, key, carriedAt, carried)
		}
		return estimate, resetAt, nil
	}

	sending := st.pending
	st.pending = 0
	st.flushing = true
	l.mu.Unlock()

	if carried > 0 {
		l.publish(ctx, limit, key, carriedAt, carried)
	}

	total, recordedReset, err := l.next.Incr(ctx, limit, key, now, sending)

	l.mu.Lock()
	st.flushing = false
	if err != nil {
		// Put them back rather than dropping them. A database that is refusing
		// writes is exactly when the local count is the only count there is.
		//
		// After a rollover that puts them in the wrong bucket, which counts
		// them a window late rather than not at all. That is the direction to
		// be wrong in: the alternative is a replica that quietly forgets
		// requests whenever an outage straddles a boundary.
		st.pending += sending
		estimate = st.global + st.pending
		l.mu.Unlock()
		return estimate, resetAt, err
	}

	if st.bucket != bucket {
		// The bucket rolled over while this flush was out, so total is the
		// count of a window nobody is deciding from any more. Writing it to
		// global would hand the new bucket the old one's whole count — and
		// because global only ever rises, the key would stay there for the rest
		// of the window. The answer is still returned: it is the truth about
		// the bucket this request happened in.
		l.mu.Unlock()
		return total, recordedReset, nil
	}

	// max, not assignment: a concurrent publisher may have learned a later
	// total, and a counter that can go backwards is a limit that can be reset
	// by racing it.
	st.global = max(st.global, total)
	st.flushedAt = now
	estimate = st.global + st.pending
	l.mu.Unlock()

	return estimate, recordedReset, nil
}

// publish sends increments belonging to a window that has already ended.
//
// Its answer is about a bucket nobody is deciding from any more, so there is
// nothing to return — but the count still matters, because that bucket is the
// weighted half of the current one for another window.
func (l *Local) publish(ctx context.Context, limit Limit, key Key, at time.Time, n int) {
	// Errors are dropped deliberately: this is bookkeeping for a window that
	// has closed, and failing the caller's request over it would turn a
	// database blip into an outage on a path that had already succeeded.
	_, _, _ = l.next.Incr(ctx, limit, key, at, n)
}

// prune drops states whose bucket can no longer be counted. Caller holds mu.
func (l *Local) prune(now time.Time) {
	if now.Sub(l.pruned) < l.cfg.Interval {
		return
	}
	l.pruned = now

	for k, st := range l.state {
		// Two windows: one for the bucket itself and one for the span in which
		// it is still the weighted previous bucket. Past that the state cannot
		// be counted by anything, so keeping it is not caution.
		//
		// The limit's own window rather than a ceiling over every limit, and
		// the difference is not memory. Past MaxKeys a new key skips the local
		// tally and takes a write per request — the amplification this type
		// exists to avoid — so what fills the map decides when that starts, and
		// holding a minute's buckets for an hour fills it with keys that cannot
		// affect an answer.
		if st.pending == 0 && !st.flushing && now.Sub(st.bucket) > 2*st.window {
			delete(l.state, k)
		}
	}
}
