package project

import (
	"time"

	"github.com/invopop/jsonschema"

	"github.com/simonjanss/rig/pkg/ir"
)

// Duration is a length of time in a configuration file: 10m, 12h, 30d, 500ms.
//
// Go's own syntax, extended with d for days, because a refresh token that lasts
// a month is written 30d by everybody who writes it down. See
// [github.com/simonjanss/rig/pkg/ir.ParseDuration].
type Duration time.Duration

// DurationPattern is the JSON Schema pattern a duration must match. It is what
// an editor validates against, and it is deliberately the same expression the
// emitted schema publishes.
const DurationPattern = `^(0|(\d+(\.\d+)?(ns|us|µs|ms|s|m|h|d))+)$`

// Duration returns the value as a [time.Duration].
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// IR converts to the document's own duration type.
func (d Duration) IR() ir.Duration { return ir.Duration(d) }

// String renders the canonical form, largest unit first.
func (d Duration) String() string { return ir.FormatDuration(time.Duration(d)) }

// UnmarshalYAML reads the string form.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	v, err := ir.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the string form, so a configuration file rig rewrites keeps
// reading the way somebody would have typed it.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// JSONSchema describes a duration as the string it is.
//
// Without this the reflector would see the underlying int64 and publish a schema
// that rejects "10m" — which is the only form anybody writes.
func (Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Pattern:     DurationPattern,
		Description: "A length of time: 250ms, 30s, 15m, 12h, 30d. Go's duration syntax, with d for days.",
		Examples:    []any{"10m", "12h", "30d"},
	}
}
