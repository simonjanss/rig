package rigclient

import (
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// The query-string writers the generated methods call.
//
// Every one of them takes a pointer and writes nothing when it is nil, which is
// the point: the generated server applies a parameter's default only when the
// parameter is absent, so a client that helpfully sent limit=0 would get an
// empty page instead of the default one. Absent has to stay absent, and a
// pointer is how a Go struct says so.
//
// The formats match what the server parses: RFC 3339 for a time, the canonical
// hyphenated form for a UUID, "true"/"false" for a boolean.

// P takes the address of a value, so a query can be filled in one expression:
//
//	c.Todos.List(ctx, client.TodoListQuery{Limit: rigclient.P(10)})
func P[T any](v T) *T { return &v }

// SetInt writes an integer parameter.
func SetInt(q url.Values, key string, v *int) {
	if v != nil {
		q.Set(key, strconv.Itoa(*v))
	}
}

// SetInt64 writes a 64-bit integer parameter.
func SetInt64(q url.Values, key string, v *int64) {
	if v != nil {
		q.Set(key, strconv.FormatInt(*v, 10))
	}
}

// SetFloat64 writes a floating-point parameter.
func SetFloat64(q url.Values, key string, v *float64) {
	if v != nil {
		q.Set(key, strconv.FormatFloat(*v, 'f', -1, 64))
	}
}

// SetBool writes a boolean parameter.
func SetBool(q url.Values, key string, v *bool) {
	if v != nil {
		q.Set(key, strconv.FormatBool(*v))
	}
}

// SetString writes a string parameter.
//
// The type parameter covers every named string type as well: an enum, and the
// scope parameter, are strings the compiler keeps apart from each other and this
// does not have to.
func SetString[T ~string](q url.Values, key string, v *T) {
	if v != nil {
		q.Set(key, string(*v))
	}
}

// SetTime writes a timestamp parameter, in RFC 3339.
func SetTime(q url.Values, key string, v *time.Time) {
	if v != nil {
		q.Set(key, v.Format(time.RFC3339Nano))
	}
}

// SetUUID writes an identifier parameter.
func SetUUID(q url.Values, key string, v *uuid.UUID) {
	if v != nil {
		q.Set(key, v.String())
	}
}

// SetStrings writes a repeated string parameter, one key per value.
func SetStrings[T ~string](q url.Values, key string, vs []T) {
	for _, v := range vs {
		q.Add(key, string(v))
	}
}

// PathValue escapes a value for a path segment.
//
// An identifier that arrived from somewhere else can be anything at all, and a
// slash in one would otherwise silently address a different route.
func PathValue(v string) string { return url.PathEscape(v) }
