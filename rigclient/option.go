package rigclient

import "net/http"

// CallOption adjusts one call.
//
// Every generated method takes these, which is what keeps a new header from
// being a regeneration: the document says what an endpoint takes, and a
// deployment's own headers are not part of that.
type CallOption func(*call)

// call is one request's own settings, laid over the client's.
type call struct {
	header    http.Header
	anonymous bool
	retried   bool
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

// WithRequestID names this call in the server's logs, overriding
// [Config.RequestID].
func WithRequestID(id string) CallOption {
	return func(c *call) { c.header.Set(DefaultRequestIDHeader, id) }
}

// WithIdempotencyKey marks a retry of a write as the same write.
func WithIdempotencyKey(key string) CallOption {
	return func(c *call) { c.header.Set("Idempotency-Key", key) }
}

// Anonymous sends the call with no credential.
//
// It is what a sign-in does — presenting an expired token to the endpoint that
// would have replaced it is how a client gets stuck — and what a public endpoint
// wants when the caller would rather not be identified by a stale session.
func Anonymous() CallOption {
	return func(c *call) { c.anonymous = true }
}
