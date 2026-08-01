package diag

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Format selects how a [List] is rendered.
type Format string

const (
	// FormatText is the human-readable form, grouped by file.
	FormatText Format = "text"
	// FormatJSON is one object per diagnostic, for tooling.
	FormatJSON Format = "json"
	// FormatGitHub emits workflow commands so diagnostics appear as
	// annotations on the changed lines of a pull request.
	FormatGitHub Format = "github"
)

// ParseFormat converts a flag value into a [Format].
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatGitHub:
		return FormatGitHub, nil
	default:
		return "", fmt.Errorf("unknown diagnostic format %q, want text, json or github", s)
	}
}

// RenderOptions tune the text renderer.
type RenderOptions struct {
	// Color emits ANSI severity colors.
	Color bool
	// Hints includes each diagnostic's hint line. Off in compact output.
	Hints bool
}

// Render writes the list in the requested format. Nothing is written when the
// list is empty, so a clean run stays silent.
func Render(w io.Writer, l *List, f Format, opts RenderOptions) error {
	if l.Len() == 0 {
		return nil
	}
	switch f {
	case FormatJSON:
		return renderJSON(w, l)
	case FormatGitHub:
		return renderGitHub(w, l)
	default:
		return renderText(w, l, opts)
	}
}

// String renders the list as text without color, for error messages and tests.
func (l *List) String() string {
	var b strings.Builder
	_ = renderText(&b, l, RenderOptions{Hints: true})
	return b.String()
}

const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiDim    = "\033[2m"
)

func colorFor(sev Severity) string {
	switch sev {
	case SeverityError:
		return ansiRed
	case SeverityWarning:
		return ansiYellow
	default:
		return ansiBlue
	}
}

func renderText(w io.Writer, l *List, opts RenderOptions) error {
	items := l.All()

	// Group by file so a reader fixing one configuration file sees everything
	// wrong with it in one block.
	lastFile := "\x00"
	for _, d := range items {
		if d.Anchor.File != lastFile {
			lastFile = d.Anchor.File
			if d.Anchor.File != "" {
				if _, err := fmt.Fprintf(w, "\n%s\n", d.Anchor.File); err != nil {
					return err
				}
			}
		}

		sev := string(d.Severity)
		if opts.Color {
			sev = colorFor(d.Severity) + sev + ansiReset
		}

		var where string
		switch {
		case d.Anchor.Line > 0 && d.Anchor.Column > 0:
			where = fmt.Sprintf("%d:%d: ", d.Anchor.Line, d.Anchor.Column)
		case d.Anchor.Line > 0:
			where = fmt.Sprintf("%d: ", d.Anchor.Line)
		case d.Anchor.File == "" && d.Anchor.Path != "":
			where = d.Anchor.Path + ": "
		}

		indent := "  "
		if d.Anchor.File == "" {
			indent = ""
		}
		if _, err := fmt.Fprintf(w, "%s%s%s[%s]: %s\n", indent, where, sev, d.Code.ID, d.Message); err != nil {
			return err
		}

		if opts.Hints && d.Hint != "" {
			hint := d.Hint
			if opts.Color {
				hint = ansiDim + hint + ansiReset
			}
			if _, err := fmt.Fprintf(w, "%s  %s\n", indent, hint); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprintf(w, "\n%s\n", summarize(l))
	return err
}

func summarize(l *List) string {
	var parts []string
	if n := l.Count(SeverityError); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "error", "errors")))
	}
	if n := l.Count(SeverityWarning); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "warning", "warnings")))
	}
	if n := l.Count(SeverityInfo); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "note", "notes")))
	}
	return strings.Join(parts, ", ")
}

func renderJSON(w io.Writer, l *List) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(struct {
		Diagnostics []Diagnostic `json:"diagnostics"`
		Errors      int          `json:"errors"`
		Warnings    int          `json:"warnings"`
	}{
		Diagnostics: l.All(),
		Errors:      l.Count(SeverityError),
		Warnings:    l.Count(SeverityWarning),
	})
}

func renderGitHub(w io.Writer, l *List) error {
	for _, d := range l.All() {
		level := "notice"
		switch d.Severity {
		case SeverityError:
			level = "error"
		case SeverityWarning:
			level = "warning"
		}

		var props []string
		if d.Anchor.File != "" {
			props = append(props, "file="+d.Anchor.File)
		}
		if d.Anchor.Line > 0 {
			props = append(props, fmt.Sprintf("line=%d", d.Anchor.Line))
		}
		if d.Anchor.Column > 0 {
			props = append(props, fmt.Sprintf("col=%d", d.Anchor.Column))
		}
		props = append(props, "title="+d.Code.ID)

		// Workflow commands are newline-delimited, so any newline in the
		// message has to be escaped or it truncates the annotation.
		msg := strings.NewReplacer("\n", "%0A", "\r", "%0D").Replace(d.Message)
		if _, err := fmt.Fprintf(w, "::%s %s::%s\n", level, strings.Join(props, ","), msg); err != nil {
			return err
		}
	}
	return nil
}
