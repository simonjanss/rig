// Package patch provides the one wrapper type generated code uses.
//
// Everything else rig generates is a plain Go type — string, time.Time,
// *uuid.UUID for a nullable column. Update input is the exception, because it
// has to distinguish three things a pointer cannot: a field the caller left
// out, a field explicitly set to null, and a field set to a value.
//
// Without that distinction a PATCH cannot both "leave this alone" and "clear
// this", and every API that tries ends up with a magic sentinel value or a
// second list of field names alongside the body.
package patch

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Patch is a value that may be absent, null, or set.
//
// The zero value is absent, which is what makes it work with encoding/json:
// UnmarshalJSON is only called for keys that are present, so a field the client
// omitted simply never gets touched.
type Patch[T any] struct {
	present bool
	null    bool
	value   T
}

// Set builds a patch carrying a value.
func Set[T any](v T) Patch[T] { return Patch[T]{present: true, value: v} }

// Null builds a patch that clears the field.
func Null[T any]() Patch[T] { return Patch[T]{present: true, null: true} }

// Absent builds a patch that leaves the field alone. It is the zero value, and
// exists so the intent can be written down.
func Absent[T any]() Patch[T] { return Patch[T]{} }

// Get returns the value, and whether there is one. It reports false for both
// absent and null, which is what most callers want; use [Patch.IsNull] when the
// difference matters.
func (p Patch[T]) Get() (T, bool) {
	if !p.present || p.null {
		var zero T
		return zero, false
	}
	return p.value, true
}

// Or returns the value, or the fallback when absent or null.
func (p Patch[T]) Or(fallback T) T {
	if v, ok := p.Get(); ok {
		return v
	}
	return fallback
}

// IsAbsent reports whether the field was left out entirely.
func (p Patch[T]) IsAbsent() bool { return !p.present }

// IsNull reports whether the field was explicitly set to null.
func (p Patch[T]) IsNull() bool { return p.present && p.null }

// IsSet reports whether the field carries a value.
func (p Patch[T]) IsSet() bool { return p.present && !p.null }

// Touched reports whether the field was mentioned at all, whether it was set to
// a value or to null. This is the question a repository asks when deciding
// which columns to include in an UPDATE.
func (p Patch[T]) Touched() bool { return p.present }

// UnmarshalJSON implements [json.Unmarshaler].
//
// It is only called when the key is present, which is exactly what makes the
// absent state work without any cooperation from the surrounding struct.
func (p *Patch[T]) UnmarshalJSON(data []byte) error {
	p.present = true

	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		p.null = true
		var zero T
		p.value = zero
		return nil
	}

	p.null = false
	if err := json.Unmarshal(data, &p.value); err != nil {
		return err
	}
	return nil
}

// MarshalJSON implements [json.Marshaler].
//
// An absent patch still encodes as null, because a struct field cannot be
// omitted from the outside. Generated response types use plain values, so this
// only matters when a request is echoed back.
func (p Patch[T]) MarshalJSON() ([]byte, error) {
	if !p.present || p.null {
		return []byte("null"), nil
	}
	return json.Marshal(p.value)
}

// String renders the patch for logs and test failures.
func (p Patch[T]) String() string {
	switch {
	case !p.present:
		return "absent"
	case p.null:
		return "null"
	default:
		return fmt.Sprintf("%v", p.value)
	}
}
