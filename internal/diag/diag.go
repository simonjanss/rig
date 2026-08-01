// Package diag collects the problems rig finds and reports them together.
//
// Every stage of the compiler appends to one [List] rather than returning on the
// first failure. A developer who has made five mistakes in a table configuration
// should learn about all five from one run, not discover them one at a time
// across five runs.
//
// Diagnostics carry an [Anchor] pointing at the exact file, line, and column
// that caused them, so an editor can jump straight there.
package diag

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Severity is how much a diagnostic matters.
type Severity string

const (
	// SeverityError stops generation. The document is still written so the
	// project can be inspected and repaired.
	SeverityError Severity = "error"
	// SeverityWarning is reported but does not stop generation. Several
	// warnings can be promoted to errors from the project configuration.
	SeverityWarning Severity = "warning"
	// SeverityInfo notes something worth knowing, such as a generated endpoint
	// being shadowed by a hand-written one.
	SeverityInfo Severity = "info"
)

// ParseSeverity converts a configuration string into a severity. The empty
// string and "off" both mean "not reported"; ok is false in that case.
func ParseSeverity(s string) (sev Severity, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return SeverityError, true
	case "warn", "warning":
		return SeverityWarning, true
	case "info":
		return SeverityInfo, true
	default:
		return "", false
	}
}

// Anchor points at the source of a diagnostic.
//
// File, Line, and Column locate it in a configuration file. Path is the
// dotted route to the offending key, for example
// "tables.lesson.columns.title.comment", and is reported when there is no file
// to point at — a problem found in the database rather than in YAML.
type Anchor struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	Path   string `json:"path,omitempty"`
}

// String renders the anchor as "file:line:col", "file:line", "file", or the
// path, whichever is the most precise available.
func (a Anchor) String() string {
	switch {
	case a.File != "" && a.Line > 0 && a.Column > 0:
		return fmt.Sprintf("%s:%d:%d", a.File, a.Line, a.Column)
	case a.File != "" && a.Line > 0:
		return fmt.Sprintf("%s:%d", a.File, a.Line)
	case a.File != "":
		return a.File
	default:
		return a.Path
	}
}

// At builds an anchor for a dotted configuration path with no file position.
func At(path string) Anchor { return Anchor{Path: path} }

// Diagnostic is one problem.
type Diagnostic struct {
	Code     Code     `json:"code"`
	Severity Severity `json:"severity"`
	Anchor   Anchor   `json:"anchor,omitzero"`
	Message  string   `json:"message"`
	// Hint is an optional second line telling the reader what to do about it.
	Hint string `json:"hint,omitempty"`
}

// String renders a single diagnostic on one line, without its hint.
func (d Diagnostic) String() string {
	if loc := d.Anchor.String(); loc != "" {
		return fmt.Sprintf("%s: %s[%s]: %s", loc, d.Severity, d.Code.ID, d.Message)
	}
	return fmt.Sprintf("%s[%s]: %s", d.Severity, d.Code.ID, d.Message)
}

// List accumulates diagnostics. The zero value is ready to use, and a nil
// *List silently discards everything appended to it, so a stage can be called
// without a collector when the caller does not care.
type List struct {
	items []Diagnostic
}

// Add appends a diagnostic with the severity its code declares.
func (l *List) Add(code Code, anchor Anchor, format string, args ...any) {
	l.AddSeverity(code, code.Severity, anchor, format, args...)
}

// AddSeverity appends a diagnostic at an explicit severity, for rules whose
// severity is configurable. A severity of "" is dropped, which is how a rule
// turned off in the project configuration reports nothing.
func (l *List) AddSeverity(code Code, sev Severity, anchor Anchor, format string, args ...any) {
	if l == nil || sev == "" {
		return
	}
	l.items = append(l.items, Diagnostic{
		Code:     code,
		Severity: sev,
		Anchor:   anchor,
		Message:  fmt.Sprintf(format, args...),
		Hint:     code.Hint,
	})
}

// Append merges another list into this one.
func (l *List) Append(other List) {
	if l == nil || len(other.items) == 0 {
		return
	}
	l.items = append(l.items, other.items...)
}

// All returns the diagnostics in report order: sorted by file, then position,
// then code. Diagnostics with no file sort first, because they describe the
// project as a whole rather than one line of it.
func (l *List) All() []Diagnostic {
	if l == nil || len(l.items) == 0 {
		return nil
	}
	out := slices.Clone(l.items)
	slices.SortStableFunc(out, func(a, b Diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Anchor.File, b.Anchor.File),
			cmp.Compare(a.Anchor.Line, b.Anchor.Line),
			cmp.Compare(a.Anchor.Column, b.Anchor.Column),
			cmp.Compare(a.Code.ID, b.Code.ID),
			cmp.Compare(a.Message, b.Message),
		)
	})
	return out
}

// Len is the number of diagnostics collected.
func (l *List) Len() int {
	if l == nil {
		return 0
	}
	return len(l.items)
}

// Count returns how many diagnostics have the given severity.
func (l *List) Count(sev Severity) int {
	if l == nil {
		return 0
	}
	n := 0
	for _, d := range l.items {
		if d.Severity == sev {
			n++
		}
	}
	return n
}

// HasErrors reports whether anything blocks generation.
func (l *List) HasErrors() bool { return l.Count(SeverityError) > 0 }

// Err returns an error describing every collected error-severity diagnostic, or
// nil when there are none. Warnings and info are not part of the error: they are
// reported, not fatal.
func (l *List) Err() error {
	if !l.HasErrors() {
		return nil
	}
	var errs []Diagnostic
	for _, d := range l.All() {
		if d.Severity == SeverityError {
			errs = append(errs, d)
		}
	}
	return &Error{Diagnostics: errs}
}

// Error is the error returned by [List.Err]. It renders every failing
// diagnostic, so a caller that only prints the error still shows the whole set.
type Error struct {
	Diagnostics []Diagnostic
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation %s:", len(e.Diagnostics), plural(len(e.Diagnostics), "error", "errors"))
	for _, d := range e.Diagnostics {
		b.WriteString("\n  ")
		b.WriteString(d.String())
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
