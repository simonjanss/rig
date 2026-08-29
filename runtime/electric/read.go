package electric

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/simonjanss/rig/runtime/dbx"
)

// DB is the database a shape is read from when the sync service cannot be
// reached: the same one the sync service reads.
//
// [dbx.Conn] and not [dbx.Pool], because nothing here opens a transaction. One
// statement answers a shape, which is the whole reason this works — a [Shape] is
// a SELECT.
type DB = dbx.Conn

// resolve settles where this shape's fallback comes from.
//
// Explicit wins. A [Shape.Fallback] the default overrode would not be an escape
// hatch, and there is no ordering in which both make sense. Below that,
// [Config.DB] answers every shape that has not opted out, and a proxy without
// one answers a sync outage the way this package always did.
func (p *Proxy) resolve(s Shape) Shape {
	if s.Fallback != nil || p.db == nil || s.NoFallback {
		return s
	}

	// A copy with no fallback on it, captured by the closures below. Reading s
	// back after the assignment would be reading the closure it now holds, and a
	// fallback that calls itself is not a fallback.
	shape := s

	s.Fallback = func(ctx context.Context) (Snapshot, error) { return p.read(ctx, shape) }
	s.probe = func(ctx context.Context) error { return p.exists(ctx, shape) }
	return s
}

// binaryFormats are the columns read in Postgres's binary format. Every other
// column is read as the text Postgres prints, which is the form a shape response
// carries — see [Value].
//
// Three of the four are here because their text form is not a fact about the
// row. A date and a timestamp are printed according to the session's DateStyle,
// and an instant according to its TimeZone as well, so the same row would come
// out one way on a laptop in Stockholm and another in a container with no TZ
// set. That is the argument [dbx.UTC] already makes, and this is the one read in
// rig that would otherwise be subject to it. Decoding them hands the rendering
// to [Value] and [DateOnly], which answer in one form everywhere.
//
// It is also where the difference between a date and an instant is still known.
// Both are a time.Time by the time [Value] sees one, which is why [DateOnly]
// exists and why the generated encoder used to choose between them per column. A
// column's own type says which it is, so nothing has to be told.
//
// Boolean is here for neither reason: Postgres prints t and f, and the sync
// service normalizes those to true and false before a subscriber sees them.
// [Value] already writes the normalized form.
//
// Notably absent: time and time with time zone. Postgres prints both in one
// form whatever DateStyle says, and that form — 10:06:07.5, the fraction
// trimmed — is what [TimeOnly] was written to produce. pgx has no codec for
// timetz either, so text is the only way to read one at all.
//
// An OID this map does not name is read as text, because a missing key is zero
// and zero is [pgx.TextFormatCode].
var binaryFormats = pgx.QueryResultFormatsByOID{
	pgtype.BoolOID:        pgx.BinaryFormatCode,
	pgtype.DateOID:        pgx.BinaryFormatCode,
	pgtype.TimestampOID:   pgx.BinaryFormatCode,
	pgtype.TimestamptzOID: pgx.BinaryFormatCode,
}

// read answers a shape from [Config.DB], using the shape's own filter.
//
// The filter is the point. [Shape.Where] and [Shape.Params] are what was sent to
// the sync service, so this is not a second description of which rows the shape
// holds that has to be kept in step with the first — it is the first one, run
// somewhere else. Whatever a scope narrowed, this narrows.
func (p *Proxy) read(ctx context.Context, s Shape) (Snapshot, error) {
	ctx, cancel := p.readContext(ctx)
	defer cancel()

	rows, err := p.db.Query(ctx, selectRows(s, p.readLimit()), p.args(s)...)
	if err != nil {
		return Snapshot{}, err
	}
	defer rows.Close()

	var (
		out    []Row
		fields []pgconn.FieldDescription
		key    []int
		send   []int
	)
	for rows.Next() {
		if fields == nil {
			// Once, on the first row: the descriptions are the same for every row
			// of one result, and resolving the projection against them is the only
			// way to answer a shape whose Columns is empty.
			fields = rows.FieldDescriptions()
			if key, send, err = layout(s, fields); err != nil {
				return Snapshot{}, err
			}
		}
		raw := rows.RawValues()

		value := make(map[string]any, len(send))
		for _, i := range send {
			v, err := column(fields[i], raw[i])
			if err != nil {
				return Snapshot{}, fmt.Errorf("%s.%s: %w", s.Table, fields[i].Name, err)
			}
			value[fields[i].Name] = v
		}

		parts := make([]string, 0, len(key))
		for _, i := range key {
			// A null in the key is not a key, and a row this cannot name is a row
			// a subscriber cannot address.
			if raw[i] == nil {
				return Snapshot{}, fmt.Errorf("%s.%s is null in a row's key", s.Table, fields[i].Name)
			}
			part, err := keyPart(fields[i], raw[i])
			if err != nil {
				return Snapshot{}, fmt.Errorf("%s.%s: %w", s.Table, fields[i].Name, err)
			}
			parts = append(parts, part)
		}
		out = append(out, Row{Key: RowKey(s.Table, parts...), Value: value})
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, err
	}

	// One past the bound, which is what the LIMIT asked for. Refused rather than
	// truncated, for the reason [Config.MaxSnapshotRows] gives: a subscriber
	// cannot tell a short answer from a complete one.
	if p.maxRows > 0 && len(out) > p.maxRows {
		return Snapshot{}, fmt.Errorf("it holds more than the %d rows a snapshot may send", p.maxRows)
	}
	return Snapshot{Rows: out, Schema: s.Schema}, nil
}

// exists reports whether [Proxy.read] would answer, and costs a row rather than
// a table.
//
// It is for [Proxy.answer]'s must-refetch branch, which needs to know that a
// resuming subscriber has somewhere to start again to and nothing at all about
// what is there. Asking that by building the snapshot and throwing it away is
// two scans per tab that was already streaming, against the database the sync
// service was shielding — and every subscriber falls back at the same moment.
//
// OFFSET past the bound asks exactly what [Proxy.read]'s LIMIT asks, without
// materializing anything: a row here is a shape too large to send, no row is a
// shape that will send, and an error either way is a read that would have failed
// the same way when it was made for real.
func (p *Proxy) exists(ctx context.Context, s Shape) error {
	ctx, cancel := p.readContext(ctx)
	defer cancel()

	rows, err := p.db.Query(ctx, selectExists(s, p.maxRows), p.args(s)...)
	if err != nil {
		return err
	}
	defer rows.Close()

	// With no bound there is nothing a row could mean, and the question was only
	// ever whether the read works. Postgres still plans and runs the filter to
	// answer it.
	over := rows.Next()
	if err := rows.Err(); err != nil {
		return err
	}
	if over && p.maxRows > 0 {
		return fmt.Errorf("it holds more than the %d rows a snapshot may send", p.maxRows)
	}
	return nil
}

// readContext bounds one read. See [Config.SnapshotTimeout].
func (p *Proxy) readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.snapTTL <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, p.snapTTL)
}

// readLimit is how many rows the read asks for: one past the bound, so that a
// shape too large to send is known to be too large without the rows past it
// being read. Zero is no limit, which is [Config.MaxSnapshotRows] set negative.
func (p *Proxy) readLimit() int {
	if p.maxRows <= 0 {
		return 0
	}
	return p.maxRows + 1
}

// args are the arguments to a read: the result formats, then the filter's bound
// values.
//
// The values go through as Go strings, which is what makes one filter work
// against a column of any type. pgx picks the text format for a string argument
// before it consults the parameter's type, and then sends the characters
// unchanged — so Postgres's own input function is what parses them, uuid_in for
// a uuid and int4in for an integer. That is what the sync service does with the
// same parameters, which is why the same filter can be sent to both.
func (p *Proxy) args(s Shape) []any {
	out := make([]any, 0, len(s.Params)+1)
	out = append(out, binaryFormats)
	for _, v := range s.Params {
		out = append(out, v)
	}
	return out
}

// selectRows is the statement that reads a shape.
func selectRows(s Shape, limit int) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if cols := selectList(s); len(cols) == 0 {
		// Every column, which is what an empty [Shape.Columns] asks the sync
		// service for too.
		b.WriteString("*")
	} else {
		for i, c := range cols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quote(c))
		}
	}
	b.WriteString(" FROM ")
	b.WriteString(qualified(s.Table))
	writeWhere(&b, s)
	if limit > 0 {
		b.WriteString(" LIMIT ")
		b.WriteString(strconv.Itoa(limit))
	}
	return b.String()
}

// selectExists is the statement behind [Proxy.exists]: whether there is a row
// past offset.
func selectExists(s Shape, offset int) string {
	var b strings.Builder
	b.WriteString("SELECT 1 FROM ")
	b.WriteString(qualified(s.Table))
	writeWhere(&b, s)
	if offset > 0 {
		b.WriteString(" OFFSET ")
		b.WriteString(strconv.Itoa(offset))
	}
	b.WriteString(" LIMIT 1")
	return b.String()
}

// writeWhere adds the filter, or nothing where there is none.
//
// A shape can legitimately have none — a table with no tenant column, no owner,
// no soft-delete and no scope — and WHERE with nothing after it does not parse.
// [Proxy.request] makes the same check for the same reason.
func writeWhere(b *strings.Builder, s Shape) {
	if s.Where == "" {
		return
	}
	b.WriteString(" WHERE ")
	b.WriteString(s.Where)
}

// qualified is a table name as an identifier: schema-qualified and quoted.
//
// Qualified even where the caller wrote it bare, because bare would be resolved
// by the connection's search_path, and the sync service resolves it to public.
// A shape that read one table and streamed another would be the same shape
// answering two different questions.
func qualified(table string) string {
	schema, name := splitTable(table)
	return quote(schema) + "." + quote(name)
}

// selectList is the columns to read: the projection, plus any key column the
// projection leaves out.
//
// A key column is read whether or not it is sent, because [RowKey] is built from
// it and a row with no key is a row a subscriber cannot address. It is still not
// sent — see [layout]. Empty means every column.
func selectList(s Shape) []string {
	if len(s.Columns) == 0 {
		return nil
	}
	out := slices.Clone(s.Columns)
	for _, k := range s.Key {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// layout resolves the shape's columns against the result: which of them are the
// row's key, and which belong in the row.
//
// The two are separate because they answer different questions. The key is
// [Shape.Key], in the order the table declares it, so a composite key is built
// the way the sync service builds it. The row is [Shape.Columns] and nothing
// else — a key column outside the projection was read to build the key and has
// no business in the value, because the projection is the promise on this path
// as much as on the other one.
func layout(s Shape, fields []pgconn.FieldDescription) (key, send []int, err error) {
	at := make(map[string]int, len(fields))
	for i, f := range fields {
		at[f.Name] = i
	}

	// Refused for the reason a null key part is, one level up: a shape with no
	// key names every row the same — [RowKey] with no parts is the table — and a
	// snapshot where every row is called "public"."todo" is one row as far as a
	// subscriber is concerned. A generated shape always carries its table's
	// primary key; one built by hand can leave it out, and this is where that is
	// still visible.
	if len(s.Key) == 0 {
		return nil, nil, fmt.Errorf("%s names no key columns, so its rows cannot be named", s.Table)
	}

	for _, k := range s.Key {
		i, ok := at[k]
		if !ok {
			return nil, nil, fmt.Errorf("%s has no column %q to key a row by", s.Table, k)
		}
		key = append(key, i)
	}

	if len(s.Columns) == 0 {
		// Every column read is a column sent, which is what an empty
		// [Shape.Columns] means.
		for i := range fields {
			send = append(send, i)
		}
		return key, send, nil
	}
	for _, c := range s.Columns {
		i, ok := at[c]
		if !ok {
			return nil, nil, fmt.Errorf("%s has no column %q", s.Table, c)
		}
		send = append(send, i)
	}
	return key, send, nil
}

// column renders one value the way the sync service renders it.
//
// Most columns arrive as the text Postgres prints, which is that form already: a
// numeric keeps its scale, an array keeps Postgres's quoting, a jsonb is the
// document, a bytea is \x and hex, and an enum is its label. The four asked for
// in binary are the ones [binaryFormats] explains, and they go through the same
// renderers a hand-written [Fallback] uses.
func column(f pgconn.FieldDescription, raw []byte) (any, error) {
	if raw == nil {
		return nil, nil
	}
	if binaryFormats[f.DataTypeOID] == pgx.BinaryFormatCode {
		return decoded(f, raw)
	}
	if f.Format != pgx.TextFormatCode {
		// Unreachable through [Proxy.read], which asks for text on every column
		// it does not decode. Refusing rather than guessing: a binary value
		// rendered as though it were text is a row that still looks like a row.
		return nil, fmt.Errorf("read in binary and cannot be rendered as text")
	}
	// A copy. RawValues borrows from the row buffer, which the next Next reuses.
	return string(raw), nil
}

// keyPart is one key column as the text a row is named by.
//
// The same rendering the column's value gets, rather than the bytes it arrived
// as, and [column] is asked so that the two cannot answer differently. For most
// columns the two are the same thing: the text Postgres printed. The four in
// [binaryFormats] arrive as bytes that are not a name at all — an eight-byte
// timestamp is mostly not valid UTF-8, and encoding/json writes one U+FFFD per
// byte it cannot read, so two rows whose keys differ only above 0x7F would go out
// under the same key.
//
// Which is reachable on any table whose primary key holds one of those four
// types — a daily metric keyed by the day, most plainly. A key of uuids,
// integers or text was never in danger of it.
func keyPart(f pgconn.FieldDescription, raw []byte) (string, error) {
	v, err := column(f, raw)
	if err != nil {
		return "", err
	}
	text, ok := v.(string)
	if !ok {
		// Unreachable: every renderer behind [column] answers with a string for a
		// value that is not null, and a null key part was refused before this was
		// asked.
		return "", fmt.Errorf("rendered as %T rather than as text", v)
	}
	return text, nil
}

// decoded renders one of [binaryFormats]'s columns.
func decoded(f pgconn.FieldDescription, raw []byte) (any, error) {
	if f.DataTypeOID == pgtype.BoolOID {
		var v bool
		if err := codecs.Scan(f.DataTypeOID, f.Format, raw, &v); err != nil {
			return nil, err
		}
		return Value(v), nil
	}

	var v time.Time
	if err := codecs.Scan(f.DataTypeOID, f.Format, raw, &v); err != nil {
		return nil, err
	}
	if f.DataTypeOID == pgtype.DateOID {
		return DateOnly(v), nil
	}
	return Value(v), nil
}
