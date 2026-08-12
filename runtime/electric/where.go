package electric

import (
	"strconv"
	"strings"
)

// Where builds a shape's filter.
//
// Conditions are added through methods rather than written as text, and every
// value becomes a numbered parameter the sync service binds. That is not
// politeness: a shape's filter is assembled from a tenant identifier and
// whatever an application's scoping function adds, and the moment either is
// interpolated into SQL, a live-sync endpoint becomes an injection point with a
// streaming response attached.
type Where struct {
	parts  []string
	params []string
}

// Eq adds `column = $n`.
func (w *Where) Eq(column string, value string) *Where {
	return w.binary(column, "=", value)
}

// NotEq adds `column <> $n`.
func (w *Where) NotEq(column string, value string) *Where {
	return w.binary(column, "<>", value)
}

// Gt, Gte, Lt, and Lte compare.
func (w *Where) Gt(column, value string) *Where  { return w.binary(column, ">", value) }
func (w *Where) Gte(column, value string) *Where { return w.binary(column, ">=", value) }
func (w *Where) Lt(column, value string) *Where  { return w.binary(column, "<", value) }
func (w *Where) Lte(column, value string) *Where { return w.binary(column, "<=", value) }

// In adds `column IN ($a, $b, ...)`.
//
// An empty set adds `false`, because "in nothing" matches nothing — and
// omitting the condition instead would widen the shape to everything, which is
// the opposite of what the caller asked for.
func (w *Where) In(column string, values ...string) *Where {
	if len(values) == 0 {
		return w.Raw("false")
	}

	placeholders := make([]string, 0, len(values))
	for _, v := range values {
		placeholders = append(placeholders, w.bind(v))
	}
	w.parts = append(w.parts, quote(column)+" IN ("+strings.Join(placeholders, ", ")+")")
	return w
}

// IsNull adds `column IS NULL`.
func (w *Where) IsNull(column string) *Where {
	w.parts = append(w.parts, quote(column)+" IS NULL")
	return w
}

// NotNull adds `column IS NOT NULL`.
func (w *Where) NotNull(column string) *Where {
	w.parts = append(w.parts, quote(column)+" IS NOT NULL")
	return w
}

// Raw adds a condition verbatim.
//
// It is the escape hatch for something the methods cannot express, and the one
// place in this package where the caller is responsible for what reaches the
// database. Never build the string from a request; use the other methods, which
// bind their values.
func (w *Where) Raw(condition string) *Where {
	if condition = strings.TrimSpace(condition); condition != "" {
		w.parts = append(w.parts, "("+condition+")")
	}
	return w
}

// Bind adds a parameter without a condition and returns its placeholder, for a
// [Where.Raw] that needs one.
func (w *Where) Bind(value string) string { return w.bind(value) }

// Empty reports whether nothing has been added.
func (w *Where) Empty() bool { return len(w.parts) == 0 }

// SQL renders the conditions, joined with AND.
func (w *Where) SQL() string { return strings.Join(w.parts, " AND ") }

// Params are the bound values, in placeholder order.
func (w *Where) Params() []string { return w.params }

func (w *Where) binary(column, op, value string) *Where {
	w.parts = append(w.parts, quote(column)+" "+op+" "+w.bind(value))
	return w
}

func (w *Where) bind(value string) string {
	w.params = append(w.params, value)
	return "$" + strconv.Itoa(len(w.params))
}

// quote wraps an identifier.
//
// Column names here come from the compiled document rather than from a request,
// so this is about correctness for a column called "order" rather than about
// safety. An embedded quote is doubled anyway: a column name that could end the
// identifier would be a way out of it.
func quote(column string) string {
	return `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
}
