package rigclient

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/simonjanss/rig/runtime/tenancy"
)

// CallOption adjusts one call.
//
// Every generated method takes these, which is what keeps a new header from
// being a regeneration: the document says what an endpoint takes, and a
// deployment's own headers are not part of that.
type CallOption func(*call)

// call is one request's own settings, laid over the client's.
type call struct {
	header http.Header
	// query is what an option added to the URL, laid over whatever the
	// operation's own typed arguments rendered.
	query     url.Values
	anonymous bool
	retried   bool
	timeout   time.Duration
	// conditional records that the caller asked a question a 304 answers, which
	// is what makes that status a result rather than a failure.
	conditional bool
}

// client is the HTTP client this call goes out on: the runtime's, unless
// [WithTimeout] asked for a different deadline. The copy is shallow, so the
// transport and its connection pool are shared.
func (c *call) client(base *http.Client) *http.Client {
	if c.timeout == 0 {
		return base
	}
	copied := *base
	copied.Timeout = c.timeout
	return &copied
}

func newCall(opts []CallOption) *call {
	c := &call{header: http.Header{}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithHeader sets a header on this call, replacing any the client sends.
func WithHeader(key, value string) CallOption {
	return func(c *call) { c.header.Set(key, value) }
}

// WithQuery sets a query parameter on this call, replacing one the method built.
func WithQuery(key, value string) CallOption {
	return func(c *call) {
		if c.query == nil {
			c.query = url.Values{}
		}
		c.query.Set(key, value)
	}
}

// Wide asks an endpoint for everything in the tenant rather than the caller's
// own rows.
//
// It has to be asked for and it has to be held: the permission is per endpoint —
// `session.read.all`, `authlog.read.all` — and a caller who asks for more than
// they hold is refused rather than quietly given less. That refusal is the point.
// A narrower answer would leave a client unable to tell "you may not see that"
// from "there is nothing else", and drawing the wrong conclusion from a
// correct-looking response is worse than an error.
//
// An option rather than a parameter, because it applies to a handful of the
// hand-written authentication endpoints and to every generated read, so a method
// signature per endpoint would be the same word spelled thirty times. A
// generated read also carries it as a typed field on its query, which is the
// same request written the other way.
func Wide() CallOption { return WithQuery(tenancy.ScopeParam, string(tenancy.ScopeAll)) }

// WithRequestID names this call in the server's logs, overriding
// [Config.RequestID].
func WithRequestID(id string) CallOption {
	return func(c *call) { c.header.Set(DefaultRequestIDHeader, id) }
}

// WithIdempotencyKey marks a retry of a write as the same write.
func WithIdempotencyKey(key string) CallOption {
	return func(c *call) { c.header.Set("Idempotency-Key", key) }
}

// WithRange asks for part of a file, from start to end inclusive. A negative
// end means the rest of it.
//
// It is only meaningful on a download, and it is an option rather than a
// generated parameter because the document says nothing about ranges: what an
// endpoint takes is what the document describes, and this is a question about
// one call. A server that honours it answers 206, which arrives as
// [Content.Status].
func WithRange(start, end int64) CallOption {
	return func(c *call) {
		spec := fmt.Sprintf("bytes=%d-", start)
		if end >= 0 {
			spec = fmt.Sprintf("bytes=%d-%d", start, end)
		}
		c.header.Set("Range", spec)
	}
}

// WithIfNoneMatch asks for a file only if it has changed, quoting the tag from
// a previous [Content].
//
// The unchanged answer is 304 with no body, and it arrives as a [Content] whose
// Status says so rather than as an error — a caller who asked the question is
// owed the answer.
func WithIfNoneMatch(etag string) CallOption {
	return func(c *call) {
		c.header.Set("If-None-Match", etag)
		c.conditional = true
	}
}

// WithTimeout bounds this call, replacing the client's own.
//
// [DefaultTimeout] caps a whole exchange at thirty seconds, which is right for
// a JSON call and wrong for a hundred-megabyte upload. A context deadline
// cannot help, because [http.Client.Timeout] is a ceiling and not a default —
// so this takes a shallow copy of the client with a different one, sharing the
// transport. Raising the default instead would weaken every other route to suit
// the one that transfers files.
func WithTimeout(d time.Duration) CallOption {
	return func(c *call) { c.timeout = d }
}

// Anonymous sends the call with no credential.
//
// It is what a sign-in does — presenting an expired token to the endpoint that
// would have replaced it is how a client gets stuck — and what a public endpoint
// wants when the caller would rather not be identified by a stale session.
func Anonymous() CallOption {
	return func(c *call) { c.anonymous = true }
}
