// Package electric proxies live-sync shape requests.
//
// A shape is a filtered view of one table that a client subscribes to and keeps
// up to date. The sync service serves it; this package stands in front, because
// the filter is not the client's to choose.
//
// That is the whole design. Everything about which rows exist — the tenant, the
// soft-delete predicate, the snapshot predicate, and whatever the application
// adds — is decided here and sent as a parameterized filter. Everything about
// where the client is in the stream — its offset, its handle, its cursor — is
// forwarded untouched, because that genuinely is the client's business. A
// parameter that is neither is dropped rather than passed along, since a
// request that could set `table` could read any table there is.
//
// The rest of the package is what happens when the sync service is not there: a
// [Shape.Fallback] answers from the application's own read in the protocol's own
// format, and a circuit stops asking a service that has stopped answering so
// that the requests behind an outage are not each paying to discover it.
package electric

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProtocolParams are the sync protocol's own query parameters, forwarded as the
// client sent them. They say where in the stream a request resumes; none of
// them can widen what the stream contains.
//
// Exported because the proxy is not the only thing that has to name them: a
// specification describing a shape route documents the same five, and a second
// copy of the list is a second thing to forget when the protocol grows a sixth.
var ProtocolParams = []string{"offset", "handle", "live", "cursor", "replica"}

// Shape is what to subscribe to.
type Shape struct {
	// Table is the table to stream, optionally schema-qualified.
	Table string
	// Where is the filter, with $1-style placeholders bound by Params.
	Where string
	// Params are the placeholder values.
	Params []string
	// Columns narrows the projection. Empty streams every column.
	//
	// It is worth setting: a shape carries every column it names to every
	// subscriber, forever, and a password hash in a live-sync stream is a
	// password hash on somebody's laptop.
	Columns []string

	// Key are the table's primary key columns, in the order the table declares
	// them.
	//
	// They identify a row within the shape — see [RowKey] — and are what
	// [Config.DB] needs to answer this shape without being told anything else
	// about it. A key column outside Columns is still read, and still left out
	// of the row: the projection is the promise on that path too.
	//
	// Required for that read, and refused rather than defaulted where it is
	// missing: a snapshot whose rows are all named after the table is one row as
	// far as a subscriber is concerned. A generated shape always carries it.
	Key []string

	// Schema is the electric-schema document for Columns: a JSON object mapping
	// each column to its Postgres type. See [Snapshot.Schema], which is where it
	// ends up.
	//
	// It describes Columns, so a shape that narrows the projection and does not
	// narrow this describes a different set than it sends.
	Schema string

	// NoFallback answers a sync outage with a 502 even where this proxy could
	// have answered from the database.
	//
	// It is for a shape whose value is its freshness rather than its rows. A
	// snapshot of who is looking at this page, that then stops updating, is
	// worth less than an empty list — so presence sets this, and it is a
	// decision rather than an omission.
	NoFallback bool

	// Fallback answers this shape from somewhere other than the database this
	// package would otherwise read.
	//
	// It is the escape hatch, and it wins: a default that overrode it would not
	// be one. Setting it is for a shape that is not a single table's rows — one
	// answered from a materialised view, a cache, or a second database.
	//
	// Whatever narrows Where has to narrow this too. Where is a filter this
	// package sends and can therefore promise; a Fallback is a read it cannot
	// see inside, so a shape scoped to less than its table with a fallback that
	// is not is a subscriber shown rows the subscription would have withheld.
	// The read built from Where has no such gap, which is why it is the default.
	Fallback Fallback

	// probe answers whether this shape has a snapshot to start again to, without
	// building one. Set for the read this package builds, where the question is
	// one row rather than every row; nil for a [Fallback], which can only be
	// asked by calling it. See [Proxy.answer].
	probe func(context.Context) error
}

// Config builds a proxy.
type Config struct {
	// URL is the sync service, for example http://electric:3000.
	URL string

	// Client makes the upstream request.
	//
	// It must not have a Timeout: a live request is a long poll that
	// deliberately hangs until something changes, and a client timeout would
	// cut every subscription at the same interval. Cancellation comes from the
	// request's context instead, so a client that goes away takes its upstream
	// request with it.
	//
	// This is not the only clock a poll runs under, and it is the only one this
	// field decides. The server's own WriteTimeout would cut the answer on the
	// way out however patient this client was; [Proxy.Serve] replaces that one
	// per request with [PollDeadline].
	Client *http.Client

	// Extra are query parameters added to every upstream request, for the
	// credentials a hosted sync service needs.
	Extra url.Values

	// Headers are added to every upstream request, for the same reason.
	Headers http.Header

	// DB answers a shape when the sync service cannot be reached, by running the
	// shape's own filter against the database the sync service reads.
	//
	// Setting it gives every shape a fallback. Nil is the older behaviour: a
	// sync outage is a 502 and a subscriber with no rows.
	//
	// A [Shape] is a SELECT — a table, a projection, and a parameterized filter
	// — so there is nothing to write per shape and nothing to keep in step. That
	// is the point. A hand-written fallback is a read this package cannot see
	// inside, so a shape scoped to less than its table with a fallback that is
	// not shows a subscriber rows the subscription would have withheld; the read
	// built from [Shape.Where] is the same predicate the sync service was sent,
	// so it cannot diverge from it.
	//
	// Three things it is worth knowing:
	//
	// The cost lands all at once. Every subscriber falls back at the same
	// moment, because what they have in common is the sync service being gone —
	// so an outage is a read per subscriber per shape against the database the
	// sync service was shielding. [Config.MaxSnapshotRows] bounds each one and
	// [Config.SnapshotTimeout] bounds how long it may take.
	//
	// [Where.Raw] now reaches this database. It was always the one place in this
	// package where the caller is responsible for what it writes; until now what
	// it wrote was parsed by the sync service in a grammar of its own, and from
	// here it is concatenated into a statement sent to Postgres.
	//
	// Postgres accepts filters the sync service refuses, which inverts the usual
	// reassurance that a fallback is the narrower of the two. [Where.In] on a
	// column whose type is a Postgres enum is a 400 from the sync service — see
	// [Where.EqText], which exists for that reason — and runs here without
	// complaint. So a scope with that mistake in it fails loudly while the sync
	// service is up and answers with rows while it is down.
	DB DB

	// InitialTimeout bounds the wait on the one request that is not a long poll:
	// a subscriber reading a shape from the beginning.
	//
	// It exists so that a sync service which is running and not answering is
	// treated as unreachable rather than waited on, which is the outage a
	// [Shape.Fallback] is most useful for and the one a transport error does not
	// catch. A live poll's wait is deliberately left unbounded — see Client;
	// what bounds its answer on the way out is [PollDeadline].
	//
	// The wait only: once the answer has started arriving it is copied out
	// whole, up to [PollDeadline]. A large shape read over a slow connection is
	// a transfer rather than an outage, and half of one is worse than either.
	//
	// Zero is [DefaultInitialTimeout]. A negative value is no timeout, which is
	// what a project whose sync service legitimately takes a while to begin
	// answering should set.
	InitialTimeout time.Duration

	// MaxSnapshotRows bounds a fallback snapshot, and refuses past the bound
	// rather than sending part of one.
	//
	// A snapshot is built whole, in memory, per subscriber — and every subscriber
	// falls back at once, because what they have in common is the sync service
	// being gone. Truncating instead would be worse than refusing: a subscriber
	// cannot tell a short answer from a complete one, so a collection quietly
	// missing half its rows would look like a table that had lost them.
	//
	// Zero is [DefaultMaxSnapshotRows]. A negative value is no bound.
	MaxSnapshotRows int

	// SnapshotTimeout bounds one read of [Config.DB].
	//
	// It exists for the reason [Config.InitialTimeout] does, one layer down.
	// Every subscriber falls back at the same moment, so an outage is every one
	// of them queueing on the same pool — and without this, a database that has
	// become slow because of that queue is a request that waits rather than one
	// that gives up and lets the next through.
	//
	// Zero is [DefaultSnapshotTimeout]. A negative value is no timeout, and the
	// request's own context is then the only bound.
	SnapshotTimeout time.Duration

	// BreakerThreshold is how many failures in a row stop this proxy asking.
	//
	// An outage is one process being unreachable, and every request that goes on
	// asking it pays [InitialTimeout] to learn what the request before it already
	// learned — a held goroutine, a held connection, and a subscriber watching a
	// spinner for ten seconds before it is given the snapshot it could have had
	// at once. Past this many consecutive failures the proxy stops asking and
	// answers from here, until [BreakerCooldown] has passed and one request is
	// let through to find out whether the service is back.
	//
	// The trade is a shape whose sync service is fine and whose network is
	// briefly not: it is answered from a fallback, or refused, for as long as one
	// cooldown — where before it would have waited and then usually succeeded. In
	// a row rather than a rate, so a single failure among successes never counts.
	//
	// Zero is [DefaultBreakerThreshold]. A negative value never opens the
	// circuit, which is every request asking however long the outage lasts.
	BreakerThreshold int

	// BreakerCooldown is how long the proxy goes on not asking before it lets one
	// request through to test the sync service.
	//
	// One request, not all of them: the rest are answered from here while it
	// finds out. Nothing polls in the background, so a service that comes back is
	// noticed by the next subscriber through the door rather than a moment
	// earlier by a goroutine of this package's own.
	//
	// Zero is [DefaultBreakerCooldown].
	BreakerCooldown time.Duration

	// OnError is told about a failure that was answered rather than returned:
	// the sync service being unreachable, and a Fallback that refused. And one
	// that could not be answered at all, because the answer had already begun —
	// a response cut short on its way out.
	//
	// It exists because those are the failures a shape endpoint hides. The
	// proxy writes a status and a short line, and without this the cause reaches
	// nobody — there is no logger in this package, for the reason there is none
	// anywhere else in runtime.
	//
	// A request the circuit refused to make is not one of them: it attempted
	// nothing, so it learned nothing worth a second line. What is worth a line is
	// the circuit opening, and that is [OnSyncState].
	//
	// Every error names the [Shape.Table] it came from. This takes a context and
	// an error and nothing else, so a handler cannot add the route afterwards —
	// and without it an application serving four shapes gets four identical lines
	// during one outage.
	OnError func(context.Context, error)

	// OnSyncState is told when the answer to whether the sync service is there
	// changes: false when the circuit opens, true when a request through it
	// succeeds.
	//
	// Twice per outage rather than once per request, which is what makes it the
	// thing to alert on. [Proxy.SyncReachable] is the same answer for a health
	// endpoint or a banner, asked rather than pushed.
	OnSyncState func(ctx context.Context, reachable bool)
}

// DefaultInitialTimeout is how long a shape read from the beginning waits for
// the sync service when [Config.InitialTimeout] says nothing.
const DefaultInitialTimeout = 10 * time.Second

// PollDeadline is how long [Proxy.Serve] gives itself to write a response,
// replacing the one deadline the server set for every route at once.
//
// The other end of [DefaultInitialTimeout]: that one bounds the wait for an
// answer, this one bounds the sending of it. A shape route needs both because
// it is the one route in an application that is slow on purpose.
//
// Five minutes is past every legitimate case rather than tuned to one, which is
// why it is a constant and not a [Config] field. Note what it does not bound: a
// sync service that hangs forever leaves this handler blocked in the upstream
// call, and no write deadline unblocks that — what ends such a poll is the
// client hanging up or [Proxy.Drain]. So being generous here costs one stalled
// reader's socket for as long as this, per request, never accumulating.
//
// Not "no deadline at all", though the wait above is deliberately unbounded. A
// write blocked on a reader that stopped reading is unblocked by nothing else:
// cancelling the request's context does not abort a write already in flight.
// Finite and generous is the honest form of unbounded.
const PollDeadline = 5 * time.Minute

// DefaultMaxSnapshotRows is how large a fallback snapshot may be when
// [Config.MaxSnapshotRows] says nothing.
const DefaultMaxSnapshotRows = 20_000

// DefaultSnapshotTimeout is how long one read of [Config.DB] may take when
// [Config.SnapshotTimeout] says nothing.
//
// Shorter than [DefaultInitialTimeout], because by the time it applies the
// subscriber has already spent that waiting for the sync service: the two are
// consecutive on the same request, not alternatives.
const DefaultSnapshotTimeout = 5 * time.Second

// DefaultBreakerThreshold is how many failures in a row stop a proxy asking
// when [Config.BreakerThreshold] says nothing.
//
// Five, because a shape route is not the only thing that fails: one refused
// connection is a restart or a redeploy, and answering the next subscriber from
// a fallback because of it would be trading live sync for nothing.
const DefaultBreakerThreshold = 5

// DefaultBreakerCooldown is how long a proxy goes on not asking when
// [Config.BreakerCooldown] says nothing.
//
// The same five seconds a subscriber holding a snapshot is asked to wait, so a
// service that comes back is found on the poll that was coming anyway.
const DefaultBreakerCooldown = retryAfterSeconds * time.Second

// retryAfterSeconds is what a subscriber holding a fallback snapshot is asked to
// wait before polling again. Short, because what it is waiting for is a service
// coming back rather than a quota refilling, and the cost of asking early is one
// refused connection.
const retryAfterSeconds = 5

// Proxy forwards shape requests.
type Proxy struct {
	base    *url.URL
	client  *http.Client
	extra   url.Values
	headers http.Header
	initial time.Duration
	maxRows int
	db      DB
	snapTTL time.Duration
	onError func(context.Context, error)
	onState func(context.Context, bool)
	// One breaker for the proxy, because what it watches for is the one sync
	// service being unreachable rather than anything about a shape.
	breaker *breaker

	// life is this proxy's own, cancelled by Drain. Every upstream call is
	// derived from it as well as from its request, which is what makes a poll
	// end when the process is stopping and not only when its subscriber gives
	// up.
	life   context.Context
	retire context.CancelFunc

	// mu guards draining against polling: a request may only join the group
	// while the answer to "is this proxy still serving" is still no.
	mu       sync.RWMutex
	draining bool
	polling  sync.WaitGroup
}

// New builds a proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.URL == "" {
		return nil, errors.New("electric: a URL is required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("electric: %w", err)
	}

	client := cfg.Client
	if client == nil {
		// No Timeout, on purpose. See Config.Client.
		client = &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 64,
				// A live poll spends most of its life idle and open.
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 0,
			},
		}
	}
	// Zero is "say nothing" and takes the default. A negative value is a
	// deliberate answer and is carried through untouched — see
	// [Config.InitialTimeout], [Config.MaxSnapshotRows] and
	// [Config.BreakerThreshold], each of which documents what its negative
	// means, and the guards that read them back.
	initial := cmp.Or(cfg.InitialTimeout, DefaultInitialTimeout)
	maxRows := cmp.Or(cfg.MaxSnapshotRows, DefaultMaxSnapshotRows)
	snapTTL := cmp.Or(cfg.SnapshotTimeout, DefaultSnapshotTimeout)
	threshold := cmp.Or(cfg.BreakerThreshold, DefaultBreakerThreshold)

	// Not cmp.Or, and this is the one place the difference shows: a cooldown
	// below zero has no meaning to carry through, so it is a mistake to correct
	// rather than a setting to honour.
	cooldown := cfg.BreakerCooldown
	if cooldown <= 0 {
		cooldown = DefaultBreakerCooldown
	}

	life, retire := context.WithCancel(context.Background())

	return &Proxy{
		base:    base,
		client:  client,
		life:    life,
		retire:  retire,
		extra:   cfg.Extra,
		headers: cfg.Headers,
		initial: initial,
		maxRows: maxRows,
		db:      cfg.DB,
		snapTTL: snapTTL,
		onError: cfg.OnError,
		onState: cfg.OnSyncState,
		breaker: &breaker{threshold: threshold, cooldown: cooldown},
	}, nil
}

// SyncReachable reports whether the sync service is answering.
//
// It is the circuit's state and not a probe: false once enough requests in a row
// have failed that this proxy stopped asking, true again as soon as one has
// succeeded. So it costs nothing to ask, and a health endpoint or a banner can
// ask it per request.
//
// True is also what it says when the circuit has been turned off with a negative
// [Config.BreakerThreshold], because then nothing is counting.
func (p *Proxy) SyncReachable() bool { return p.breaker.reachable() }

// Drain ends the polls this proxy is holding open and stops it starting more.
//
// Register it as the first step of a shutdown, which is what the step is for:
//
//	app.DrainWithin("shapes", 5*time.Second, proxy.Drain)
//
// A live subscription is a request that is deliberately not answering yet. It is
// still an in-flight request, so [http.Server.Shutdown] waits for it — and
// waits, because nothing in the poll is late and Shutdown does not cancel a
// request's context. One open browser tab is then a shutdown that spends its
// budget waiting for the sync service to have news, and a sync service that
// hangs rather than refuses is a shutdown that spends all of it. What goes
// without in either case is everything registered after: the flush, the sweep,
// and the pass of the notification engine whose claims are only handed back if
// its close step runs.
//
// So they are ended from this side instead, at the point in the sequence where
// readiness is already false and there is nothing to be gained from holding a
// subscription open. A subscriber gets a 503 with a Retry-After and resumes from
// the same offset against whichever replica is still serving; nothing is lost,
// because a poll that had not answered had nothing in it yet.
//
// It returns when every poll has gone, or when ctx says to stop waiting. Calling
// it twice is safe, and a proxy that never served anything returns at once.
func (p *Proxy) Drain(ctx context.Context) error {
	p.mu.Lock()
	first := !p.draining
	p.draining = true
	p.mu.Unlock()

	if first {
		p.retire()
	}

	// The wait is for the handlers, not for the connections: a poll that has
	// been cancelled is a handler about to return, and what Shutdown counts is
	// the request rather than the socket under it.
	gone := make(chan struct{})
	go func() {
		p.polling.Wait()
		close(gone)
	}()

	select {
	case <-gone:
	case <-ctx.Done():
		return fmt.Errorf("electric: polls were still open: %w", ctx.Err())
	}

	// Nothing is using them, and they are keep-alives to a service this process
	// is finished with.
	p.client.CloseIdleConnections()
	return nil
}

// stopping is what a subscriber is told by a proxy that is going away.
//
// Deliberately not [Proxy.answer]: that offers a snapshot to a request reading
// from the beginning, and a snapshot is a read and a large body from a process
// whose budget is nearly spent. Come back is the honest answer, and the client
// asks again in five seconds — by which time this replica is gone and another
// one takes it.
func (p *Proxy) stopping(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	http.Error(w, "the sync service is unavailable", http.StatusServiceUnavailable)
}

// hopHeaders are per-connection and must not be forwarded in either direction.
//
// A set rather than a list, keyed the way net/http canonicalizes rather than the
// way the RFC spells them, because that is how they arrive in a header map:
// forwarding a response is then one lookup per header instead of a nine-way
// case-insensitive scan, and the response being forwarded here is every
// response. Note "Te" — [http.CanonicalHeaderKey] lowercases everything after
// the first letter, so the RFC's "TE" would never match.
var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true,
	"Transfer-Encoding": true, "Te": true, "Trailer": true, "Upgrade": true,
	"Proxy-Authenticate": true, "Proxy-Authorization": true,
}

// Serve forwards one shape request and streams the answer back.
//
// It writes the response, including on failure. A caller that wants its own
// error rendering should check the shape before calling.
//
// When the sync service cannot be reached and the shape has a [Fallback], a
// subscriber reading from the beginning is answered from there instead. Nothing
// about the request says which of the two it got, and nothing has to: the answer
// is in the same format either way.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, s Shape) {
	// First, so that everything below reads one field rather than deciding for
	// itself where a fallback comes from. s is a value, so this is local to the
	// request and the caller's shape is untouched.
	s = p.resolve(s)

	// Second, and before any of the paths below can write: the server's
	// WriteTimeout is not this route's to obey. See [extendWrite].
	extendWrite(w)

	// Before anything is built, and before the circuit below: whether this proxy
	// is still in the business of serving this shape at all. Joining the group
	// under the same lock that reads the flag is what makes Drain's wait mean
	// something: a request that got past here is one Drain waits for, and a
	// request that did not is one it never has to know about.
	//
	// First of the two, because a drained proxy has nothing to offer an open
	// circuit either. Answering from the fallback there would be a table read
	// and a large body out of a process whose pool is about to close, and the
	// subscriber would rather be told to come back — see [Proxy.stopping].
	p.mu.RLock()
	if p.draining {
		p.mu.RUnlock()
		p.stopping(w)
		return
	}
	p.polling.Add(1)
	p.mu.RUnlock()
	defer p.polling.Done()

	// Then the circuit: while it is open there is nothing to learn from asking,
	// and the point of not asking is that this request does not wait for the
	// answer the last one already got.
	if !p.breaker.allow() {
		p.answer(w, r, s)
		return
	}

	// The request's context, so a client that hangs up cancels the poll it left
	// running upstream — bounded first for a read from the beginning, which is
	// the one request that is not supposed to hang.
	//
	// A timer rather than a deadline on the context, because what is being
	// bounded is the wait for an answer and not the sending of one. A deadline
	// would still be running while the body copied, and a body cut in half goes
	// out under a 200 already written: a subscriber cannot tell it from a whole
	// one, and no fallback can rescue it because the status has been sent.
	//
	// Which is exactly the failure [extendWrite] above prevents from the other
	// direction. The two are not in tension: that one is a deadline on the
	// connection, and it is set generously enough that only a stalled reader
	// reaches it, where this one is a bound on a wait that is supposed to end.
	ctx := r.Context()
	answered := func() bool { return true }
	if p.initial > 0 && initial(r) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		answered = time.AfterFunc(p.initial, cancel).Stop
	}

	// And this proxy's own lifetime on top of whichever of the two that was.
	//
	// A live poll is a request that is deliberately not answering yet: nothing
	// in it is late, so no timeout above applies to it, and http.Server.Shutdown
	// does not cancel a request context. Without this the only thing that ends
	// such a poll is the sync service finally having something to say — which
	// is the whole of a shutdown budget spent waiting for news, and the rest of
	// the teardown going without.
	ctx, untie := context.WithCancel(ctx)
	defer untie()
	defer context.AfterFunc(p.life, untie)()

	upstream, err := p.request(ctx, r, s)
	if err != nil {
		http.Error(w, "cannot reach the sync service", http.StatusBadGateway)
		return
	}

	res, err := p.client.Do(upstream)
	// The wait is over, however it ended: what is left is a transfer. Stopping
	// the timer here is what leaves the body unbounded, and failing to stop it
	// is the deadline having fired — either that is why this call failed, or it
	// landed on an answer now reading from a cancelled context. Both are the
	// outage the deadline is there to catch.
	if !answered() {
		if res != nil {
			res.Body.Close()
		}
		p.note(r, false)
		p.unavailable(w, r, s, fmt.Errorf("electric: the sync service did not answer within %s", p.initial))
		return
	}
	if err != nil {
		// Whatever failed here, the deadline did not: that is the branch above.
		// So a cancelled request is the client hanging up mid-poll, which is the
		// ordinary end of a subscription rather than a failure to report — and
		// not a failure of the sync service either, so the circuit is not told
		// about it. A page being closed is not an outage.
		if r.Context().Err() != nil {
			return
		}
		// Neither is this process stopping. The poll was ended from this side,
		// so the subscriber is told to come back rather than handed a snapshot:
		// there is no server left to read one out of, and the next attempt
		// belongs to whichever replica is still serving.
		if p.life.Err() != nil {
			p.stopping(w)
			return
		}
		p.note(r, false)
		p.unavailable(w, r, s, err)
		return
	}

	// A sync service answering with its own failure is as unreachable as one
	// that does not answer, and the circuit counts it either way: whether this
	// shape has something to answer with instead is a fact about the shape, and
	// what the circuit tracks is the service.
	p.note(r, res.StatusCode < 500)

	// But only where there is something to answer with instead is it answered
	// from here. Otherwise it is forwarded untouched, because the status and the
	// Retry-After the sync service chose say more than a 502 substituted for
	// them, and a shape with no fallback has nothing to gain from the swap.
	//
	// A 4xx is never either: that is a decision about this shape, and answering
	// it from somewhere else would hide a filter being refused.
	if res.StatusCode >= 500 && s.Fallback != nil && initial(r) {
		res.Body.Close()
		p.unavailable(w, r, s, fmt.Errorf("electric: the sync service answered %d", res.StatusCode))
		return
	}
	defer res.Body.Close()

	// The electric-* headers carry the cursor the client needs to resume, so
	// dropping them would end the subscription after one response.
	out := w.Header()
	for name, values := range res.Header {
		if isHopHeader(name) {
			continue
		}
		for _, v := range values {
			out.Add(name, v)
		}
	}
	w.WriteHeader(res.StatusCode)

	// Flush after the copy rather than during it: a long poll answers all at
	// once, and the flush is what stops a buffering intermediary from sitting
	// on the response until the next one arrives.
	//
	// Through a ResponseController rather than a type assertion for w: a
	// middleware that wraps the writer — the request log's does — is not an
	// http.Flusher itself, and an assertion would quietly find nothing and skip
	// the flush. The controller follows Unwrap to the writer that can.
	if _, err := io.Copy(w, res.Body); err != nil {
		p.cutShort(r, s, err)
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		p.cutShort(r, s, err)
	}
}

// cutShort reports a response that began and did not finish.
//
// The one failure on this path that is otherwise invisible. A 200 and the
// electric-* headers have been written, [Proxy.note] has already recorded the
// attempt as a success, and what fails after that reaches nobody — the
// subscriber sees a torn connection and retries from the same offset, which
// during a truncation that repeats is every subscriber stalling with nothing in
// the log to say so.
//
// Which of the two calls above notices is not fixed: a long poll answers all at
// once and its body is small enough to fit the server's buffer, so the copy
// succeeds and the flush is where the write actually happens. That is why both
// report rather than only the copy.
//
// Not the circuit. The sync service answered; what failed was the sending, and
// counting it against the service would open the circuit because a reader went
// away.
//
// Not a client that hung up either, which is the same rule [Proxy.trySnapshot]
// follows and for the same reason: during an outage that is one line per closed
// tab, burying the error that caused it.
func (p *Proxy) cutShort(r *http.Request, s Shape, cause error) {
	if p.onError == nil || r.Context().Err() != nil {
		return
	}
	p.onError(r.Context(), fmt.Errorf("electric: the response was cut short: %w, for shape %s", cause, s.Table))
}

// extendWrite replaces the server's write deadline for this one request.
//
// [github.com/simonjanss/rig/runtime/serve.Config.WriteTimeout] is set once on
// the one http.Server — thirty seconds — and its clock starts when the request's
// headers were read, not when the body starts going out. A poll the sync service
// holds for longer than that has its answer killed on the way out: a 200 the
// subscriber never receives, and a connection torn down under it. Raising the
// server's field instead would weaken every other route in the application,
// which is why there is no PollTimeout on serve.Config and should not be one.
// The file transfers do the same thing for the same reason.
//
// For every request rather than only a live poll. The long poll is the loud
// case, but [Config.InitialTimeout] promises that an answer which has started
// arriving is copied out whole however long that takes, and a large snapshot
// over a slow connection breaks that promise under a thirty-second deadline
// just as quietly.
//
// The write deadline only. A shape request is a GET with no body, and net/http
// has already cleared the read deadline for the length of the handler because
// that pending read is how it notices a client going away. A read deadline set
// here lands on that watch instead: when it expires the request's context is
// cancelled, and the branch below reads a cancelled context as the client
// hanging up — so a poll that outlived it would be abandoned silently and
// counted as a closed tab. Mirroring the file transfers, which set both, is the
// obvious improvement here and it is a bug.
//
// A writer that does not support the control keeps the server's deadlines,
// which is the right failure: the response stays bounded by something rather
// than by nothing.
func extendWrite(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(PollDeadline))
}

// initial reports whether this request is a subscriber reading the shape from
// the beginning, rather than resuming one it already has.
//
// It is the only request a snapshot can answer. A resumed one asks for what
// changed since an offset, and a snapshot in reply to that is not a smaller
// answer than the client wanted — it is a different question answered, with no
// way for the client to tell.
//
// An absent offset counts, because the protocol's own default is the beginning.
func initial(r *http.Request) bool {
	q := r.URL.Query()
	if q.Get("live") != "" && q.Get("live") != "false" {
		return false
	}
	switch off := q.Get("offset"); off {
	case "", "-1":
		return true
	default:
		return false
	}
}

// note tells the circuit how an attempt on the sync service went, and says so
// through [Config.OnSyncState] on the one attempt that changes the answer.
//
// Only an attempt: a request the circuit refused to make learned nothing, and a
// client that hung up says nothing about the service either.
func (p *Proxy) note(r *http.Request, reachable bool) {
	if p.breaker.record(reachable) && p.onState != nil {
		p.onState(r.Context(), reachable)
	}
}

// unavailable answers a request the sync service could not, and reports why.
func (p *Proxy) unavailable(w http.ResponseWriter, r *http.Request, s Shape, cause error) {
	if p.onError != nil {
		// Named with its table, because [Config.OnError] takes a context and an
		// error and nothing else: a request that has already been routed to a
		// shape is past the point where a handler could add the route, so if the
		// table is not in the error it is nowhere. An application serving four
		// shapes otherwise gets four identical lines and no way to tell which
		// subscriber waited — which matters most on the timeout, where the one
		// that paid it is exactly the question.
		p.onError(r.Context(), fmt.Errorf("%w, for shape %s", cause, s.Table))
	}
	p.answer(w, r, s)
}

// answer is what goes back when the sync service is not the one answering.
//
// Separate from [Proxy.unavailable] because of the request that was never made:
// with the circuit open there is no attempt and no error, and a line per skipped
// request would bury the one that said the circuit had opened. What goes back is
// the same either way — a subscriber cannot tell the two apart and has no reason
// to.
func (p *Proxy) answer(w http.ResponseWriter, r *http.Request, s Shape) {
	if s.Fallback != nil && initial(r) {
		snap, ok := p.trySnapshot(w, r, s)
		if !ok {
			return
		}
		writeSnapshot(w, s, snap)
		return
	}

	// A subscriber resuming from a handle this proxy invented already holds the
	// rows a snapshot would send it, so what it needs is not another one: it is
	// to be told to keep them and come back. 503 with a Retry-After says that,
	// where a 502 reads as a failure of the request rather than of the service.
	if isFallbackHandle(r.URL.Query().Get("handle")) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		http.Error(w, "the sync service is unavailable", http.StatusServiceUnavailable)
		return
	}

	// A subscriber resuming from a handle the *sync service* issued — the tab
	// that was already streaming when the outage began. It is asked to start
	// again, which is the only thing that reaches the snapshot it could
	// otherwise only get by being reloaded. [writeMustRefetch] has the whole
	// reasoning, including why this is checked after the branch above rather
	// than before it.
	//
	// What is being decided here is whether there is a snapshot at all, and not
	// what is in it. A shape having a fallback is not the same as that fallback
	// answering: it can refuse, and [Config.MaxSnapshotRows] can refuse for it.
	// Sending a subscriber to start again and then meeting it with that refusal
	// would cost it the rows it was holding and hand it the 502 it would have
	// had anyway — the one outcome this branch exists to avoid. So a refusal
	// keeps the rows where they are.
	//
	// [Shape.probe] is why that costs a row rather than a table for the read
	// this package builds. A [Fallback] has no such shortcut: the only way to
	// ask it anything is to call it, so its answer is read and thrown away, and
	// a tab that was already streaming costs one read more than the tab beside
	// it that arrived during the outage.
	if s.Fallback != nil {
		if !p.tryProbe(w, r, s) {
			return
		}
		writeMustRefetch(w)
		return
	}

	http.Error(w, "the sync service is unavailable", http.StatusBadGateway)
}

// trySnapshot reads the fallback, and answers the request itself when it cannot
// be sent.
//
// False means this request is finished and there is nothing left for the caller
// to do. Its two arms mean different things, which is why the caller is told
// only that much: either the fallback refused and the subscriber has been sent a
// 502, or the subscriber hung up while the fallback was being read.
//
// The second of those is not a refusal. Nothing was decided about the shape —
// somebody closed a tab — so it gets no status and no line in [Config.OnError],
// which during an outage would otherwise fill with one line per closed tab and
// bury the error that caused the outage.
func (p *Proxy) trySnapshot(w http.ResponseWriter, r *http.Request, s Shape) (Snapshot, bool) {
	snap, err := p.snapshot(r.Context(), s)
	return snap, p.sendable(w, r, err)
}

// tryProbe establishes that this shape has a snapshot to start again to, without
// building one where it does not have to.
//
// False means the same as it does for [Proxy.trySnapshot], for the same two
// reasons: the shape refused and a 502 has been sent, or the subscriber hung up
// while it was being asked.
func (p *Proxy) tryProbe(w http.ResponseWriter, r *http.Request, s Shape) bool {
	if s.probe == nil {
		_, ok := p.trySnapshot(w, r, s)
		return ok
	}
	err := s.probe(r.Context())
	if err != nil {
		err = fmt.Errorf("electric: the fallback for %s refused: %w", s.Table, err)
	}
	return p.sendable(w, r, err)
}

// sendable reports whether a fallback answered, and answers the request itself
// where it did not.
//
// The hangup arm is not a refusal. Nothing was decided about the shape —
// somebody closed a tab — so it gets no status and no line in [Config.OnError],
// which during an outage would otherwise fill with one line per closed tab and
// bury the error that caused the outage.
func (p *Proxy) sendable(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return true
	}
	if r.Context().Err() != nil {
		return false
	}
	p.refuse(r, w, err)
	return false
}

// snapshot reads a shape's fallback and holds it to [Config.MaxSnapshotRows].
//
// The question it answers is whether there is a snapshot and whether it may be
// sent, which is what both of [Proxy.trySnapshot]'s callers need — one of them
// then throws the rows away. The error is the sentence [Proxy.refuse] reports,
// so what a log says about a refusal does not depend on which of them asked.
//
// The bound is checked after the fact here, which is the only way to check a
// [Fallback]: it returns the rows it read, so they have been built by the time
// there is a count to compare. The read this package builds applies it as a
// LIMIT instead and never materializes the row past the bound — same refusal,
// paid for differently.
func (p *Proxy) snapshot(ctx context.Context, s Shape) (Snapshot, error) {
	snap, err := s.Fallback(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("electric: the fallback for %s refused: %w", s.Table, err)
	}
	if p.maxRows > 0 && len(snap.Rows) > p.maxRows {
		return Snapshot{}, fmt.Errorf("electric: the fallback for %s answered with %d rows, past the %d it may send",
			s.Table, len(snap.Rows), p.maxRows)
	}
	return snap, nil
}

// refuse answers a fallback that could not be sent, and says why to whoever is
// listening. The subscriber is told the sync service is unavailable, which is
// true and is all it can act on.
func (p *Proxy) refuse(r *http.Request, w http.ResponseWriter, cause error) {
	if p.onError != nil {
		p.onError(r.Context(), cause)
	}
	http.Error(w, "the sync service is unavailable", http.StatusBadGateway)
}

// request builds the upstream call.
func (p *Proxy) request(ctx context.Context, r *http.Request, s Shape) (*http.Request, error) {
	if s.Table == "" {
		return nil, errors.New("electric: the shape names no table")
	}

	target := *p.base
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/shape"

	q := url.Values{}
	for name, values := range p.extra {
		for _, v := range values {
			q.Add(name, v)
		}
	}

	// Ours, always. A request that could set these could read any table there
	// is, under any filter it liked.
	q.Set("table", s.Table)
	if s.Where != "" {
		q.Set("where", s.Where)
		for i, v := range s.Params {
			q.Set("params["+strconv.Itoa(i+1)+"]", v)
		}
	}
	if len(s.Columns) > 0 {
		q.Set("columns", strings.Join(s.Columns, ","))
	}

	// Theirs, forwarded. Anything else the client sent is dropped.
	from := r.URL.Query()
	for _, name := range ProtocolParams {
		if v := from.Get(name); v != "" {
			q.Set(name, v)
		}
	}
	target.RawQuery = q.Encode()

	upstream, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}

	// Conditional-request headers pass through so the sync service can answer
	// 304, which is most of what keeps a subscription cheap.
	for _, name := range []string{"If-None-Match", "If-Modified-Since", "Accept-Encoding"} {
		if v := r.Header.Get(name); v != "" {
			upstream.Header.Set(name, v)
		}
	}
	// Deliberately not the client's Authorization header: this server has
	// already decided who the caller is, and the sync service's credentials are
	// this server's, not theirs.
	for name, values := range p.headers {
		for _, v := range values {
			upstream.Header.Add(name, v)
		}
	}
	return upstream, nil
}

func isHopHeader(name string) bool { return hopHeaders[http.CanonicalHeaderKey(name)] }
