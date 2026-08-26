package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// The parameter readers, and the refusals they answer with.
//
// Two generators emit routes that read parameters off a request — `server-go`
// for the API, `electric-go` for the streaming routes — and both of them used to
// emit these bodies, down to the wording of the 400. Every project therefore
// carried two copies of one contract, and the contract is a wire contract: the
// sentence a client reads when it sends `limit=soon` is part of what rig
// promises, and two copies of a promise is one promise that can come to be made
// two ways.
//
// **The split between the two halves below is the load-bearing part.** The
// parsers take `(name, raw)` rather than a request, because the two generators
// disagree about where `raw` comes from — a path segment, a query value, a
// declared shape parameter — and agree entirely about what to do with it once
// they have it. The readers on top of them are the small number of shapes those
// two generators actually need. A reader that only one of them needs is still
// here rather than emitted, because the refusal it writes is the same refusal.
//
// **An absent parameter and an empty one are the same thing**, on purpose. A
// query string carries no way to tell `?since=` from no `since` at all that
// every client agrees on, so rig picks the reading that cannot surprise anybody:
// empty is unset. The consequence is that a parameter whose empty string is
// meaningful cannot be expressed, and no route rig generates has one.

// ParseInt reads a whole number.
func ParseInt(name, raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, rigerr.BadRequest("%s must be a whole number", name)
	}
	return v, nil
}

// ParseInt64 reads a whole number that may not fit in an int.
func ParseInt64(name, raw string) (int64, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, rigerr.BadRequest("%s must be a whole number", name)
	}
	return v, nil
}

// ParseFloat reads a number.
func ParseFloat(name, raw string) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, rigerr.BadRequest("%s must be a number", name)
	}
	return v, nil
}

// ParseBool reads a boolean.
//
// [strconv.ParseBool]'s spelling, so `1`, `t` and `TRUE` are all true. Wider
// than the `true`/`false` the message names, and deliberately so: the message
// tells a client what to send, and the parser accepts what a client is likely to
// have sent anyway.
func ParseBool(name, raw string) (bool, error) {
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, rigerr.BadRequest("%s must be true or false", name)
	}
	return v, nil
}

// ParseUUID reads an identifier.
//
// The message says "identifier" rather than "UUID" because that is what it is to
// whoever is reading it: a client that got one out of a previous response and
// sent it back does not need to be told what shape rig keeps them in.
func ParseUUID(name, raw string) (uuid.UUID, error) {
	v, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, rigerr.BadRequest("%s is not a valid identifier", name)
	}
	return v, nil
}

// ParseTime reads an RFC 3339 timestamp.
func ParseTime(name, raw string) (time.Time, error) {
	v, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, rigerr.BadRequest("%s must be an RFC 3339 timestamp", name)
	}
	return v, nil
}

// PathString reads a path segment that must be there.
//
// It can be missing in exactly one way — a handler asking for a segment the
// pattern it was registered under does not have — so the refusal is a 400 rather
// than a 500 on the theory that a router rig did not write may have got there
// first.
func PathString(r *http.Request, name string) (string, error) {
	raw := r.PathValue(name)
	if raw == "" {
		return "", rigerr.BadRequest("%s is required", name)
	}
	return raw, nil
}

// PathUUID reads an identifier out of the path.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	return ParseUUID(name, r.PathValue(name))
}

// PathInt reads a whole number out of the path.
func PathInt(r *http.Request, name string) (int, error) {
	return ParseInt(name, r.PathValue(name))
}

// QueryRequired reads a query parameter that must be present.
//
// A shape whose scoping depends on a value is a shape that must not stream
// without it, which is the one place rig insists on a query parameter.
func QueryRequired(r *http.Request, name string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", rigerr.BadRequest("%s is required", name)
	}
	return v, nil
}

// QueryOptional reads a query parameter and says whether it was there.
func QueryOptional(r *http.Request, name string) (string, bool) {
	v := r.URL.Query().Get(name)
	return v, v != ""
}

// QueryString reads a query parameter, falling back when absent.
func QueryString(r *http.Request, name, fallback string) string {
	if raw := r.URL.Query().Get(name); raw != "" {
		return raw
	}
	return fallback
}

// QueryInt reads a whole-number query parameter, falling back when absent.
func QueryInt(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return ParseInt(name, raw)
}

// QueryBool reads a boolean query parameter, falling back when absent.
func QueryBool(r *http.Request, name string, fallback bool) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return ParseBool(name, raw)
}

// QueryUUID reads an identifier out of the query string.
//
// Absent is [uuid.Nil] and no error, because every route that reads one treats
// it as a filter that was not asked for.
func QueryUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return uuid.Nil, nil
	}
	return ParseUUID(name, raw)
}

// QueryTime reads a timestamp out of the query string.
//
// Absent is the zero time and no error, for the reason [QueryUUID] gives.
func QueryTime(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, nil
	}
	return ParseTime(name, raw)
}
