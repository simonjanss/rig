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
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return rigerr.BadRequest("the request body is empty")
		}
		return rigerr.BadRequest("the request body is not the shape this route takes: %s", err)
	}
	return nil
}
