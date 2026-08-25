package electric

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// Fallback answers a shape from somewhere other than the sync service.
//
// It is called when the sync service cannot be reached and a subscriber is
// asking for the shape from the beginning — never mid-subscription, and never
// on a live poll. What it returns is sent in the sync protocol's own format, so
// a client that already speaks the protocol needs to know nothing about it.
//
// A shape with no Fallback answers a sync outage the way this package always
// did: 502, and a subscriber with no rows.
type Fallback func(ctx context.Context) (Snapshot, error)

// Snapshot is a shape's rows at one moment, and enough type information for a
// client to decode them.
type Snapshot struct {
	// Rows are the rows the shape's filter admits. Order does not matter.
	Rows []Row

	// Schema is the electric-schema header: a JSON object mapping each column to
	// its Postgres type, which is how a client picks a parser per column.
	//
	// Sending it is not optional in practice. A subscriber that does not get it
	// leaves every value as the string it arrived as, so a timestamp stays text
	// and a count stays text — decoded differently from the same column read
	// over the API, which is the one thing a generated client exists to prevent.
	Schema string
}

// Row is one row of a snapshot.
type Row struct {
	// Key identifies the row within the shape. [RowKey] builds one.
	Key string

	// Value is the row: the shape's columns, each in the text form Postgres
	// prints, or nil for NULL. A column the shape does not carry does not belong
	// here — the projection is the same promise on this path as on the other one.
	Value map[string]any
}

// RowKey builds the key the sync service gives a row: the schema-qualified
// table, then the primary key, each part quoted.
//
// The table may arrive qualified or not, the way [Shape.Table] may; an
// unqualified one is public, which is what the sync service resolves it to.
// More than one key part is a composite primary key, in the order the table
// declares them.
func RowKey(table string, key ...string) string {
	schema, name := "public", table
	if i := strings.IndexByte(table, '.'); i >= 0 {
		schema, name = table[:i], table[i+1:]
	}

	var b strings.Builder
	b.WriteString(quote(schema))
	b.WriteByte('.')
	b.WriteString(quote(name))
	for _, k := range key {
		b.WriteByte('/')
		b.WriteString(quote(k))
	}
	return b.String()
}

// fallbackHandlePrefix marks a handle this package invented rather than one the
// sync service issued.
//
// It has to be recognisable, because the two are told apart in both directions:
// this proxy answers a poll carrying one with a 503 rather than a 502 while the
// outage lasts, and the sync service refuses one with a 409 and a must-refetch
// the moment it is reachable again. That refusal is the whole recovery path, and
// it is the protocol's own — a shape handle that does not match is exactly what
// must-refetch is for, so a subscriber resets itself and re-reads from the sync
// service without this package arranging anything.
const fallbackHandlePrefix = "rig-fallback-"

// fallbackHandles numbers the handles this process hands out, so two
// subscriptions served during one outage are not told they are the same shape.
var fallbackHandles atomic.Uint64

// isFallbackHandle reports whether a handle came from here.
func isFallbackHandle(handle string) bool {
	return strings.HasPrefix(handle, fallbackHandlePrefix)
}

// message is one entry of a shape response.
//
// The field order is the order the sync service writes them in. Nothing parses
// positionally, so this is for the benefit of somebody comparing two responses
// in a terminal.
type message struct {
	Key     string         `json:"key,omitempty"`
	Value   map[string]any `json:"value,omitempty"`
	Headers map[string]any `json:"headers"`
}

// writeSnapshot answers with a snapshot in the sync protocol's format.
//
// The response ends with an up-to-date control message, which is what makes a
// subscriber ready rather than still loading. A real initial response ends with
// snapshot-end and takes a second request to reach up-to-date, because the sync
// service has a log to catch up on between the two; there is no log here, so
// there is nothing to catch up on and a second round trip would only answer the
// same thing again.
func writeSnapshot(w http.ResponseWriter, s Shape, snap Snapshot) {
	schema, table := "public", s.Table
	if i := strings.IndexByte(s.Table, '.'); i >= 0 {
		schema, table = s.Table[:i], s.Table[i+1:]
	}
	relation := []any{schema, table}

	out := make([]message, 0, len(snap.Rows)+1)
	for _, row := range snap.Rows {
		out = append(out, message{
			Key:   row.Key,
			Value: row.Value,
			// insert, for every row. A snapshot is what the shape holds now, not a
			// description of how it got there, and it is read by a subscriber that
			// has nothing yet.
			Headers: map[string]any{"relation": relation, "operation": "insert"},
		})
	}
	out = append(out, message{Headers: map[string]any{"control": "up-to-date"}})

	body, err := json.Marshal(out)
	if err != nil {
		// A column whose value will not marshal. Answering 502 rather than a
		// half-written body: the status has not been sent yet, so this is still a
		// failure to reach the sync service as far as a subscriber can tell.
		http.Error(w, "the sync service is unavailable", http.StatusBadGateway)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	if snap.Schema != "" {
		h.Set("electric-schema", snap.Schema)
	}
	h.Set("electric-handle", fallbackHandlePrefix+strconv.FormatUint(fallbackHandles.Add(1), 10))
	// The offset a subscriber holds when it is caught up. It is sent back on the
	// next poll, and that poll is the one the sync service refuses.
	h.Set("electric-offset", "0_inf")
	// And that it *is* caught up, which the offset does not say on its own. A
	// handle and an offset without this is what a client's chunk buffer reads as
	// a response with another chunk behind it, so it asks for the next one
	// immediately — a chunk that does not exist, and a request this proxy can
	// only refuse. Empty, because the sync service sends it empty: the header's
	// presence is the whole signal.
	h.Set("electric-up-to-date", "")
	h.Set("electric-has-data", strconv.FormatBool(len(snap.Rows) > 0))
	// This is not a stream and it is not cacheable: the sync service's own
	// responses are immutable for a given offset, and this one is a moment.
	h.Set("Cache-Control", "no-store")
	// So that a browser's network tab, a request log and a person reading either
	// can tell a snapshot from a subscription.
	h.Set("X-Rig-Sync-Fallback", "snapshot")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	_ = http.NewResponseController(w).Flush()
}
