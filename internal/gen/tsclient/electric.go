package tsclient

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/tsbuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// electricFile emits one factory per live-sync stream.
//
// Three routes per streamed resource at most, and the columns decide which: the
// live shape always, the trash when the table has a `deleted_at`, and one row's
// history when it has the snapshot columns. That is the rule the shape endpoints
// already follow, read off the same document — asking a second time here would
// create a way for the two answers to disagree.
//
// This file is not emitted at all when no table opts in, which is the promise
// server-go makes on the server side. It is what keeps `@rig/electric` — and
// TanStack DB, and the sync client — out of a project that streams nothing.
func (e *emitter) electricFile(streams []*ir.Resource) (gen.Artifact, error) {
	b := e.open("electric.gen")

	b.Doc("The live-sync collections.\n\n" +
		"One factory per shape the API serves. Each takes the client's runtime and " +
		"the params its endpoint declares, and hands back a TanStack DB collection " +
		"that syncs while something is subscribed to it and pauses when nothing " +
		"is.\n\n" +
		"The same runtime and params always give back the same collection, so a " +
		"stream survives a navigation and two callers share one subscription. That " +
		"is what makes these safe to call during render, with no memoization.\n\n" +
		"A row here is the `<Resource>Row` type, not the `<Resource>` the API " +
		"sends: a stream carries what Postgres printed, under column names. See " +
		"the row type's own documentation.")

	for _, res := range streams {
		// A table with no API surface has no types file, so its row type is
		// emitted here — beside the only thing that mentions it.
		if _, exposed := e.home[res.Name]; !exposed {
			e.rowType(b, res)
		}

		if len(res.Electric.Params) > 0 {
			e.streamParams(b, res)
		}

		e.streamFactory(b, res, streamLive)
		if res.Storage.IsSoftDeletable() {
			e.streamFactory(b, res, streamDeleted)
		}
		if res.Storage.IsSnapshotable() {
			e.streamFactory(b, res, streamVersions)
		}
	}

	return e.close(b)
}

// The three shapes a table can have, which are the three the server mounts.
type streamKind int

const (
	streamLive streamKind = iota
	streamDeleted
	streamVersions
)

// streamParams emits the params a resource's shapes accept.
//
// One type for all three routes, because the endpoints take the same declared
// params: the trash and the history are the same table's rows under a different
// lifecycle filter, and the filter is the route rather than a parameter.
func (e *emitter) streamParams(b *tsbuf.Buf, res *ir.Resource) {
	b.Comment("The params the " + res.Name + " streams accept.\n\n" +
		"They are handed to the scoping function the application wrote, which can " +
		"only narrow what a subscriber sees. Nothing here can widen a shape — the " +
		"tenant and lifecycle filters are the server's and are not a client's to " +
		"send.")
	b.L("export type %sStreamParams = {", res.Name)
	b.Indent()
	for _, p := range res.Electric.Params {
		if p.Description != "" {
			b.Comment(p.Description)
		}
		optional := ""
		if p.Optional {
			optional = "?"
		}
		b.L("%s%s: %s;", tsbuf.Key(p.Name), optional, streamParamType(p))
	}
	b.Outdent()
	b.L("};")
	b.NL()
}

// streamParamType is the TypeScript type of one declared param.
//
// Every one of them travels on a query string and is parsed there, so the set is
// narrower than a field's: a number, a boolean, or a string — and a UUID, a Date
// and a Timestamp are strings, which is what they are in a URL.
func streamParamType(p ir.ElectricParam) string {
	switch p.Type {
	case ir.TypeBool:
		return "boolean"
	case ir.TypeInt, ir.TypeInt64, ir.TypeFloat64:
		return "number"
	default:
		return "string"
	}
}

// streamFactory emits one route's factory.
func (e *emitter) streamFactory(b *tsbuf.Buf, res *ir.Resource, kind streamKind) {
	var (
		name  string
		path  string
		about string
	)
	switch kind {
	case streamLive:
		name = "create" + res.Name + "Stream"
		path = res.Electric.Path
		about = "the rows an ordinary read returns: not deleted, and not a snapshot"
	case streamDeleted:
		name = "create" + res.Name + "DeletedStream"
		path = res.Electric.DeletedPath()
		about = "the retired rows — the trash.\n\nUnlike `GET /_deleted`, this has " +
			"no restore window: it carries every retired row, however old. A window " +
			"is a moving predicate and the sync service evaluates a shape's filter " +
			"when a row changes, so a row that aged out would emit nothing and sit " +
			"in your copy forever — filtered in appearance and not in fact"
	case streamVersions:
		name = "create" + res.Name + "VersionsStream"
		path = res.Electric.VersionsPath()
		about = "one row's previous versions.\n\nAn unknown identifier is an empty " +
			"stream rather than a 404: a shape endpoint is a filter in front of the " +
			"sync service and has no database handle, so an id from another tenant " +
			"matches nothing rather than being refused. Read the row over the API " +
			"first if that distinction matters"
	}

	row := e.ref(b, res.Name+"Row")
	runtime := b.ImportType(e.cfg.ClientImport, "Runtime")
	createCollection := b.Import(e.cfg.ElectricImport, "createRigCollection")
	cache := b.Import(e.cfg.ElectricImport, "createCollectionCache")

	params, args := e.streamArgs(res, kind)

	b.Comment("Streams " + about + ".\n\n" +
		"GET " + path + "\n\n" +
		"The same runtime and params always give back the same collection, so the " +
		"stream survives a navigation and two callers share one subscription. Safe " +
		"to call during render — no memoization needed.")
	b.L("export const %s = %s(", name, cache)
	b.Indent()
	b.L("(runtime: %s, params: %s) =>", runtime, params)
	b.Indent()
	b.L("%s<%s>({", createCollection, row)
	b.Indent()
	b.L("runtime,")
	b.L("path: %s,", e.streamPath(b, res, kind))
	b.L("params: %s,", args)
	b.L("getKey: (row) => row.%s,", tsbuf.Key(e.keyColumn(res)))
	b.Outdent()
	b.L("})")
	b.Outdent()
	b.Outdent()
	b.L(");")
	b.NL()
}

// streamArgs is the params type a factory takes and the expression it sends.
//
// The history route takes the row's identifier on top of whatever the endpoint
// declared, because the identifier is a path segment rather than a query
// parameter. So it is in the same object for the caller — one argument, not two
// — and is spliced into the route rather than sent.
//
// The sent expression names the declared params one by one rather than
// forwarding the object. Forwarding it would put `id` on the query string of
// every history subscription, where the proxy drops it as a parameter it does
// not recognise: harmless, and also the kind of thing nobody notices for a year.
func (e *emitter) streamArgs(res *ir.Resource, kind streamKind) (string, string) {
	declared := ""
	if len(res.Electric.Params) > 0 {
		declared = res.Name + "StreamParams"
	}

	sent := "{}"
	if declared != "" {
		names := make([]string, 0, len(res.Electric.Params))
		for _, p := range res.Electric.Params {
			names = append(names, tsbuf.Key(p.Name)+": params."+tsbuf.Key(p.Name))
		}
		sent = "{ " + strings.Join(names, ", ") + " }"
	}

	switch {
	case kind == streamVersions && declared == "":
		return "{ id: string }", sent
	case kind == streamVersions:
		return declared + " & { id: string }", sent
	case declared == "":
		return "Record<string, never>", sent
	default:
		return declared, sent
	}
}

// streamPath is the route expression, with the identifier spliced in for the
// history shape.
func (e *emitter) streamPath(b *tsbuf.Buf, res *ir.Resource, kind streamKind) string {
	switch kind {
	case streamDeleted:
		return tsbuf.Quote(res.Electric.DeletedPath())
	case streamVersions:
		pathValue := b.Import(e.cfg.ClientImport, "pathValue")
		path := strings.Replace(res.Electric.VersionsPath(), "{id}",
			"${"+pathValue+"(params.id)}", 1)
		return "`" + path + "`"
	default:
		return tsbuf.Quote(res.Electric.Path)
	}
}

// keyColumn is the column a collection is keyed by.
//
// The primary key, which rig's convention makes `id` on every table it manages.
// Taken from the storage rather than assumed, so a table that names it something
// else keys correctly instead of keying every row to undefined.
func (e *emitter) keyColumn(res *ir.Resource) string {
	if res.Storage != nil && len(res.Storage.PrimaryKey) == 1 {
		return res.Storage.PrimaryKey[0]
	}
	// A composite key has no single column to key a collection by, and rig's
	// own convention never produces one. Falling back to `id` keeps the emitted
	// file compiling and wrong in one visible place rather than in every row.
	return "id"
}
