// Package httpx is the wire shape of the routes rig owns.
//
// Four small things, each of which three packages had a copy of: how JSON goes
// out, how an error goes out, how a body comes in, and how the caller is
// established before any of it. The value is not the lines saved — it is that a
// client parsing a failure from /auth/login and a failure from /notifications is
// parsing one shape, which had stopped being true. `notifyhttp` and
// `presencehttp` answered a nested `{"error":{code,message}}` that neither of
// rig's own client libraries can read, and `authhttp` answered a flat envelope
// missing the per-field detail a validation failure is worth.
//
// # Why a package of its own
//
// The error writer needs [throttle.RefusalOf] to put a Retry-After on a 429, and
// `runtime/throttle` already imports `runtime/rigerr`. So these cannot live in
// `rigerr`, which is the other obvious home: it would be a cycle. They are not in
// `runtime/serve` either, which is the process's lifecycle rather than the wire.
//
// # The keys are camelCase, always
//
// A project's `api.json_case` renames the fields of its *own* resources, and the
// generated server's error envelope goes through it. This one does not. These
// routes are identical in every project and the browser packages are compiled
// against them once, which is the same argument presence's `Beat` already makes
// for its own fields. So the generated `DefaultErrorMapper` and [WriteError]
// write the same shape under the same names for a camelCase project and
// deliberately diverge for any other — and the generated one stays generated,
// because one struct cannot be both.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// MaxBodyBytes bounds a request body when a route does not say otherwise.
//
// A limit at all, because without one a single client can exhaust the server's
// memory by streaming forever into a handler that is waiting for a small JSON
// object. A mebibyte, because it is far past any body these routes take and far
// short of anything that matters.
const MaxBodyBytes = 1 << 20

// WriteJSON writes a JSON response. A nil body is a status and no bytes.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// Decode reads a JSON request body, refusing a field the route does not declare.
//
// DisallowUnknownFields for the reason the generated decoder uses it: a client
// that sent `accountId` believing it meant something should be told so, not
// silently ignored — the field it was reaching for is often the one the route
// exists to make unreachable.
//
// A limit of zero or less is [MaxBodyBytes]. An unauthenticated route should pass
// something smaller, because there the limit is the only thing between a stranger
// and the server's memory.
//
// An empty body is its own message. It is the most common client mistake by a
// wide margin, and "unexpected end of JSON input" is not what the person reading
// it needs to know.
func Decode(r *http.Request, limit int64, into any) error {
	if limit <= 0 {
		limit = MaxBodyBytes
	}
	dec := json.NewDecoder(Bounded(r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, ErrTooLarge) {
			return rigerr.TooLarge("the request body is larger than the %d bytes this endpoint accepts", limit)
		}
		if errors.Is(err, io.EOF) {
			return rigerr.BadRequest("the request body is empty")
		}
		return rigerr.BadRequest("the request body is not the shape this route takes: %s", err)
	}
	return nil
}

// ErrTooLarge is what a [Bounded] reader fails with past its limit. It is
// matched with [errors.Is] rather than returned to a caller, which reads it back
// off the decoder that was reading through one.
var ErrTooLarge = errors.New("the request body is over the limit")

// Bounded is [io.LimitReader] that refuses rather than truncates.
//
// The difference is the whole point. A LimitReader stops early and says nothing,
// so a body one byte over the limit reaches the JSON decoder as a document that
// ends in the middle — and the caller is told its request was malformed, which
// is both untrue and unactionable. This one fails with [ErrTooLarge] instead, so
// the answer is a 413 naming the limit.
//
// Not [net/http.MaxBytesReader], which does the same job and better — it also
// stops the connection being held open by a client still sending. That one needs
// the [net/http.ResponseWriter], and neither [Decode] nor the decoders in
// runtime/apibase has one to give it. Changing their signatures would reach every
// generated server.
func Bounded(r io.Reader, limit int64) io.Reader { return &bounded{r: r, left: limit} }

type bounded struct {
	r    io.Reader
	left int64
}

func (b *bounded) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, ErrTooLarge
	}
	// One past the limit, so that a body of exactly the limit is read whole and
	// the next read is what fails. Reading only up to the limit would leave a
	// decoder that had consumed a complete document unable to tell the two apart.
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}
