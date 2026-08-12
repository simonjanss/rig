// Package patch provides the two wrapper types an update input is made of.
//
// Everything else rig generates is a plain Go type — string, time.Time, *string
// for a nullable column. Update input is the exception, because a PATCH has to
// distinguish things a plain value cannot: a field the caller left out from one
// they sent, and, where the column allows it, a field they explicitly cleared.
//
// There are two types rather than one because a column either can be null or it
// cannot, and the type should say which:
//
//   - [Optional] is absent or set. It is what a NOT NULL column gets, and there
//     is no way to write null into one.
//   - [Nullable] is absent, null, or set. It is what a nullable column gets.
//
// The alternative — one three-state type everywhere — makes clearing a NOT NULL
// column something you can write down and have rejected at runtime. This way
// the compiler rejects it, and the only null that has to be caught is one that
// arrives over the wire.
//
// The zero value of each is absent, which is what makes them work with
// encoding/json without any cooperation from the surrounding struct:
// UnmarshalJSON is only called for keys that are present, so a field the client
// omitted is never touched.
package patch

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Optional is a value that may be left out of an update.
//
// Two states: absent, or set to a value. A column that is NOT NULL cannot be
// cleared, so there is nothing here that could express clearing it.
type Optional[T any] struct {
	present bool
	value   T
}

// NewOptional builds an Optional carrying a value.
func NewOptional[T any](v T) Optional[T] { return Optional[T]{present: true, value: v} }

// Absent builds an Optional that leaves the field alone. It is the zero value,
// and exists so the intent can be written down.
func Absent[T any]() Optional[T] { return Optional[T]{} }

// Get returns the value, and whether one was given.
func (o Optional[T]) Get() (T, bool) {
	if !o.present {
		var zero T
		return zero, false
	}
	return o.value, true
}

// Or returns the value, or the fallback when absent.
func (o Optional[T]) Or(fallback T) T {
	if v, ok := o.Get(); ok {
		return v
	}
	return fallback
}

// IsAbsent reports whether the field was left out.
func (o Optional[T]) IsAbsent() bool { return !o.present }

// IsSet reports whether the field carries a value.
func (o Optional[T]) IsSet() bool { return o.present }

// ErrNullNotAllowed is returned when a client sends null for a column that
// cannot hold one.
//
// It is a distinct type so the decoding layer can turn it into a 400 that names
// the field, rather than the generic complaint encoding/json would produce.
type ErrNullNotAllowed struct{}

// Error implements error.
func (ErrNullNotAllowed) Error() string { return "this field cannot be null" }

// UnmarshalJSON implements [json.Unmarshaler].
//
// An explicit null is an error rather than a silent absence. The two mean
// different things — "leave this alone" and "clear this" — and a column that
// cannot be cleared should say so rather than quietly ignoring the request.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return ErrNullNotAllowed{}
	}

	if err := json.Unmarshal(data, &o.value); err != nil {
		return err
	}
	o.present = true
	return nil
}

// MarshalJSON implements [json.Marshaler].
//
// An absent value still encodes as null, because a struct field cannot be
// omitted from the outside. Generated responses use plain values, so this only
// matters when a request is echoed back.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.present {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}

// String renders the value for logs and test failures.
func (o Optional[T]) String() string {
	if !o.present {
		return "absent"
	}
	return fmt.Sprintf("%v", o.value)
}

// Nullable is a value that may be left out, cleared, or set.
//
// Three states, matching a nullable column exactly.
type Nullable[T any] struct {
	present bool
	null    bool
	value   T
}

// NewNullable builds a Nullable carrying a value.
func NewNullable[T any](v T) Nullable[T] { return Nullable[T]{present: true, value: v} }

// Null builds a Nullable that clears the field.
func Null[T any]() Nullable[T] { return Nullable[T]{present: true, null: true} }

// Unspecified builds a Nullable that leaves the field alone. It is the zero
// value, and exists so the intent can be written down.
func Unspecified[T any]() Nullable[T] { return Nullable[T]{} }

// FromPtr builds a Nullable from a pointer: nil clears, anything else sets.
//
// It is how a previous row's value — which the model holds as *T — becomes an
// input, which is what merging a partial update against the current state
// needs.
func FromPtr[T any](v *T) Nullable[T] {
	if v == nil {
		return Null[T]()
	}
	return NewNullable(*v)
}

// FromSlice builds a Nullable from a slice: nil clears, anything else sets.
//
// The array counterpart of [FromPtr]. A nullable array column is held as a
// plain slice rather than a pointer to one — nil already means "no value" —
// so the pointer form has nothing to take the address of.
//
// An empty non-nil slice is a value: a column set to {} is not a column set to
// NULL, and a replay that confused the two would quietly change the row.
func FromSlice[T any](v []T) Nullable[[]T] {
	if v == nil {
		return Null[[]T]()
	}
	return NewNullable(v)
}

// Get returns the value, and whether there is one. It reports false for both
// absent and null; use [Nullable.IsNull] when the difference matters.
func (n Nullable[T]) Get() (T, bool) {
	if !n.present || n.null {
		var zero T
		return zero, false
	}
	return n.value, true
}

// Or returns the value, or the fallback when absent or null.
func (n Nullable[T]) Or(fallback T) T {
	if v, ok := n.Get(); ok {
		return v
	}
	return fallback
}

// Ptr returns a pointer to the value, or nil when absent or null.
//
// It is what a repository passes to the driver: a nil pointer is exactly how a
// NULL is written, so no branch is needed at the call site.
func (n Nullable[T]) Ptr() *T {
	if v, ok := n.Get(); ok {
		return &v
	}
	return nil
}

// IsAbsent reports whether the field was left out entirely.
func (n Nullable[T]) IsAbsent() bool { return !n.present }

// IsNull reports whether the field was explicitly cleared.
func (n Nullable[T]) IsNull() bool { return n.present && n.null }

// IsSet reports whether the field carries a value.
func (n Nullable[T]) IsSet() bool { return n.present && !n.null }

// Touched reports whether the field was mentioned at all, whether it was set to
// a value or cleared. It is the question a repository asks when deciding which
// columns belong in an UPDATE.
func (n Nullable[T]) Touched() bool { return n.present }

// UnmarshalJSON implements [json.Unmarshaler].
func (n *Nullable[T]) UnmarshalJSON(data []byte) error {
	n.present = true

	if isJSONNull(data) {
		n.null = true
		var zero T
		n.value = zero
		return nil
	}

	n.null = false
	return json.Unmarshal(data, &n.value)
}

// MarshalJSON implements [json.Marshaler].
func (n Nullable[T]) MarshalJSON() ([]byte, error) {
	if !n.present || n.null {
		return []byte("null"), nil
	}
	return json.Marshal(n.value)
}

// String renders the value for logs and test failures.
func (n Nullable[T]) String() string {
	switch {
	case !n.present:
		return "absent"
	case n.null:
		return "null"
	default:
		return fmt.Sprintf("%v", n.value)
	}
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
