package ir

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Duration is a length of time, written the way a person writes one.
//
// It exists because the document is read by more than Go. A field holding
// 43200000000000 is a field every consumer has to divide by something, and one
// holding "12h" reads correctly in the IR JSON, in an OpenAPI description, and
// in a TypeScript comment. [Duration.Duration] hands the Go value back to a
// generator that wants to emit a literal.
type Duration time.Duration

// Duration returns the value as a [time.Duration].
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the canonical form: the largest units first, and nothing that
// is zero. A whole number of days is written in days, which is what makes a
// thirty-day refresh token read as 30d rather than as 720h0m0s.
func (d Duration) String() string { return FormatDuration(time.Duration(d)) }

// MarshalJSON writes the canonical string form.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts the string form, and a bare 0 for "none".
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		var n int64
		if err2 := json.Unmarshal(data, &n); err2 != nil || n != 0 {
			return err
		}
		*d = 0
		return nil
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ParseDuration reads a duration in Go's syntax, extended with d for days.
//
// The extension is the whole reason this is not [time.ParseDuration]. A refresh
// token that lasts a month is configured as 30d by anybody writing it down, and
// 720h is the same number with the arithmetic already done — which is a thing to
// get wrong rather than a thing to read.
//
// A day is exactly 24 hours here. Nothing in this package is doing calendar
// arithmetic, so there is no daylight saving to be wrong about.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if s == "0" {
		return 0, nil
	}

	var (
		total time.Duration
		rest  = s
		neg   bool
	)
	if rest[0] == '-' || rest[0] == '+' {
		neg = rest[0] == '-'
		rest = rest[1:]
	}

	for rest != "" {
		// The number, then the unit. Splitting them by hand rather than handing
		// the whole string to time.ParseDuration is what lets a d in the middle
		// of 1d12h be understood.
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration %q: expected a number at %q", s, rest)
		}
		number, unitAndRest := rest[:i], rest[i:]

		j := 0
		for j < len(unitAndRest) && !(unitAndRest[j] >= '0' && unitAndRest[j] <= '9') {
			j++
		}
		unit := unitAndRest[:j]
		rest = unitAndRest[j:]

		if unit == "d" {
			// Handed to time.ParseDuration as hours, so the number keeps
			// whatever precision it was written with: 0.5d is twelve hours.
			part, err := time.ParseDuration(number + "h")
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q: %w", s, err)
			}
			total += part * 24
			continue
		}

		part, err := time.ParseDuration(number + unit)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		total += part
	}

	if neg {
		total = -total
	}
	return total, nil
}

// FormatDuration renders a duration the way [Duration.String] does.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}

	var b strings.Builder
	if d < 0 {
		b.WriteByte('-')
		d = -d
	}

	for _, unit := range []struct {
		suffix string
		size   time.Duration
	}{
		{"d", 24 * time.Hour},
		{"h", time.Hour},
		{"m", time.Minute},
		{"s", time.Second},
		{"ms", time.Millisecond},
		{"us", time.Microsecond},
	} {
		if n := d / unit.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, unit.suffix)
			d -= n * unit.size
		}
	}
	if d > 0 {
		fmt.Fprintf(&b, "%dns", d)
	}
	return b.String()
}
