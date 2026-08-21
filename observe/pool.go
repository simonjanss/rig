package observe

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Pool puts a span around every statement. It has the shape
// serve.Config.Pool takes, so wiring the database up is one line:
//
//	serve.Main(serve.Config{Pool: observe.Pool, …})
//
// This is where the SQL comes from rather than the generated repositories,
// because a tracer on the connection sees every statement — the ones a hook
// runs, the ones rig's own foundation runs, the ones a service issues directly
// — and a generator only ever sees what it wrote.
//
// It replaces any tracer already on the connection configuration, because pgx
// has room for exactly one. A project that has its own wraps this rather than
// setting both.
func Pool(cfg *pgxpool.Config) error {
	cfg.ConnConfig.Tracer = queryTracer{}
	return nil
}

// queryTracer is the pgx side of [Pool].
type queryTracer struct{}

var _ pgx.QueryTracer = queryTracer{}

// TraceQueryStart opens the statement's span.
//
// The span goes on the returned context, which pgx hands back to TraceQueryEnd
// — the parent is whatever the caller was already in, so a statement issued
// inside a repository's Before hook lands under that hook rather than beside
// it.
func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx, _ = Tracer().Start(ctx, queryName(data.SQL),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBQueryText(data.SQL),
			semconv.DBOperationName(operation(data.SQL)),
		),
	)
	return ctx
}

// TraceQueryEnd closes it.
//
// Not deferred, because pgx decides when a statement is over and this is the
// call that says so. It is the one span in this package whose end is not a
// defer, and it is why it is the only one: there is no function here whose
// lifetime is the statement's.
func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)

	// No rows is an answer. Marking it as a failure would make every optional
	// read red, and the caller above has already decided whether it meant
	// anything.
	if data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows) {
		record(span, data.Err)
	}
	if data.Err == nil {
		span.SetAttributes(semconv.DBResponseReturnedRows(int(data.CommandTag.RowsAffected())))
	}
	span.End()
}

// queryName is what the statement is called in a trace: the verb and the table,
// "INSERT todo".
//
// Not the statement itself, which is what several pgx instrumentations use. A
// generated INSERT names every column it writes, and a trace listing it in full
// as a span name is unreadable at the width anybody views one. The whole
// statement is on the span as db.query.text, where it can be read once by
// somebody who wants it.
func queryName(sql string) string {
	verb := operation(sql)
	if verb == "" {
		return "query"
	}
	if table := queryTable(sql, verb); table != "" {
		return verb + " " + table
	}
	return verb
}

// operation is the leading keyword, upper-cased.
func operation(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

// queryTable is the table the statement names, when it says so in the one place
// this looks.
//
// Deliberately not a SQL parser. It reads the token after FROM, INTO or UPDATE
// and gives up on anything else — a CTE, a join, a subselect — because a span
// name that is right for the ordinary statement and absent for the unusual one
// is more useful than one that is confidently wrong about both.
func queryTable(sql, verb string) string {
	var keyword string
	switch verb {
	case "SELECT", "DELETE":
		keyword = "FROM"
	case "INSERT":
		keyword = "INTO"
	case "UPDATE":
		keyword = "UPDATE"
	default:
		return ""
	}

	fields := strings.Fields(sql)
	for i, f := range fields {
		if !strings.EqualFold(f, keyword) || i+1 >= len(fields) {
			continue
		}
		table := strings.Trim(fields[i+1], `"(),;`)
		if table == "" || strings.ContainsAny(table, "$*") {
			return ""
		}
		return table
	}
	return ""
}
