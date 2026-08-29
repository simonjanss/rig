package tablesync

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/ir"
)

// todoComment is the placeholder written for anything rig cannot describe.
//
// It is deliberately not a real sentence. The missing-comment rule rejects a
// TODO, so a file full of placeholders fails validation until someone says what
// the columns mean — which is the whole point of the rule.
const todoComment = "TODO: describe this."

// render writes a complete configuration file for a table.
func render(t *ir.Table, enums map[string]*ir.PgEnum, n *naming.Namer) string {
	var b strings.Builder

	b.WriteString("# yaml-language-server: $schema=" + schemaRelPath() + "\n")
	fmt.Fprintf(&b, "table: %s\n", t.Name)
	fmt.Fprintf(&b, "comment: %s\n", quote(tableComment(t)))

	if t.Column(compile.ColDeletedAt) != nil {
		b.WriteString("\n# Days a soft-deleted row stays restorable.\n")
		b.WriteString("restore_window_days: 30\n")
	}

	cols := configurableColumns(t)
	if len(cols) > 0 {
		b.WriteString("\ncolumns:\n")
		for _, c := range cols {
			b.WriteString(renderColumn(c, "  "))
		}
	}

	used := enumsUsedBy(t, enums)
	if len(used) > 0 {
		b.WriteString("\nenums:\n")
		for _, e := range used {
			b.WriteString(renderEnum(e, n, "  "))
		}
	}

	return b.String()
}

// schemaRelPath is the editor directive's target. Table files sit two levels
// down under the default layout, which is what the relative path assumes; a
// different layout only costs a wrong hint, not a wrong file.
func schemaRelPath() string { return "../../.rig/table.schema.json" }

func tableComment(t *ir.Table) string {
	if c := strings.TrimSpace(t.Comment); c != "" {
		return c
	}
	return "TODO: what is this table for?"
}

// renderColumn writes one column entry at the given indentation.
func renderColumn(c *ir.Column, indent string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s%s:\n", indent, c.Name)
	fmt.Fprintf(&b, "%s  comment: %s\n", indent, quote(columnComment(c)))

	// A generated or identity column can never be written, so saying so in the
	// file would be restating a fact the database already enforces.
	return b.String()
}

func columnComment(c *ir.Column) string {
	if c.CommentSource == ir.CommentSourceDatabase && strings.TrimSpace(c.Comment) != "" {
		return c.Comment
	}
	return todoComment
}

func renderEnum(e *ir.PgEnum, n *naming.Namer, indent string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s%s:\n", indent, e.Name)
	fmt.Fprintf(&b, "%s  name: %s\n", indent, n.Go(e.Name))
	fmt.Fprintf(&b, "%s  description: %s\n", indent, quote("TODO: what does this enumeration represent?"))
	if len(e.Values) == 0 {
		return b.String()
	}

	fmt.Fprintf(&b, "%s  values:\n", indent)
	for _, v := range e.Values {
		fmt.Fprintf(&b, "%s    %s:\n", indent, v.Value)
		fmt.Fprintf(&b, "%s      name: %s\n", indent, n.Go(v.Value))
		fmt.Fprintf(&b, "%s      description: %s\n", indent, quote(todoComment))
	}
	return b.String()
}

// quote wraps a scalar when YAML would otherwise misread it. A comment starting
// with "TODO:" is the common case: the colon turns it into a mapping.
func quote(s string) string {
	// A newline cannot survive a plain or single-quoted scalar — YAML folds it
	// into a space — and a Postgres comment is free text that regularly has
	// one. Double quotes are the only form that can spell it.
	if strings.ContainsAny(s, "\n\r\t") {
		return strconv.Quote(s)
	}

	needsQuote := strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`") ||
		strings.HasPrefix(s, " ") ||
		strings.HasSuffix(s, " ") ||
		s == ""

	if !needsQuote {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
