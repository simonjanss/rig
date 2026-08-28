// Package tsclient generates a TypeScript SDK for the API: the types a front
// end holds, one method per endpoint, and a factory per live-sync stream.
//
// It exists for the reason the Go client generator does, and the reason bites
// harder here. A hand-written TypeScript client is a second description of the
// API kept in step by whoever remembers, and the failure is quiet: a client that
// stopped sending a field compiles perfectly, and a type that says `string`
// about a column the server now sends as null is a runtime error in somebody's
// browser rather than a build failure. This one is written from the document the
// router was built from, so the two cannot disagree about a path, a status, or
// the name of a key.
//
// What it does not generate is everything underneath the methods: the request,
// the credential, the failure, the pagination. That is the same in every project
// and lives, hand-written, in the `@rig/client` package — and the streaming half
// lives in `@rig/electric`, which is a separate package because the sync client
// and TanStack DB come with it and a project that streams nothing should install
// nothing.
//
// # Two shapes for one row
//
// A row reaches a front end two ways, and they are not the same shape. Over REST
// it arrives as `encoding/json` wrote it, under the keys the document's
// `json_case` produced — `createdAt` by default. Over a stream it arrives as
// Postgres printed it, under column names — `created_at`. rig's shape endpoint
// is a proxy in front of the sync service and does not rewrite the rows, so
// there is nothing in the middle to reconcile them.
//
// So a streamed resource gets a second type, `<Resource>Row`, keyed by column.
// Declaring one type and using it for both is the bug this generator exists to
// prevent, only with the compiler agreeing.
package tsclient

import (
	"context"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/tsbuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func init() { gen.Register(New()) }

// DefaultClientModule is the hand-written half of the SDK, which the generated
// half calls into.
const DefaultClientModule = "@rig/client"

// DefaultElectricModule is the hand-written half of the streaming layer. It is
// only ever imported by a project that has a table with `electric: {enabled:
// true}`, so a project without one never has to install it.
const DefaultElectricModule = "@rig/electric"

// defaultRevisionHeader matches the `@rig/client` default and rig.yaml's
// api.revision_header, for a document compiled before the setting existed.
const defaultRevisionHeader = "API-Revision"

// Options configure the generator.
type Options struct {
	// ClientImport is the module specifier of the SDK runtime. It is here for a
	// fork, a vendored copy, or a registry of your own; a project has no reason
	// to set it.
	ClientImport string `json:"client_import"`

	// ElectricImport is the same, for the streaming runtime.
	ElectricImport string `json:"electric_import"`

	// DefaultBaseURL is emitted as a constant when it is set, so a development
	// tool can build a client without naming the server. Leaving it out is right
	// for anything that runs in more than one place — and in a browser the
	// ordinary answer is the empty string, which resolves against the page.
	DefaultBaseURL string `json:"default_base_url"`
}

// Generator emits the TypeScript client.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "ts-client" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "a typed TypeScript client: the wire types, one method per endpoint, and the live-sync collections"
}

// Version implements [gen.Generator].
func (*Generator) Version() string { return "1" }

// Generate implements [gen.Generator].
func (g *Generator) Generate(
	_ context.Context, doc *ir.Document, opts gen.Options,
) ([]gen.Artifact, error) {
	cfg, err := gen.Decode[Options](opts)
	if err != nil {
		return nil, err
	}
	if cfg.ClientImport == "" {
		cfg.ClientImport = DefaultClientModule
	}
	if cfg.ElectricImport == "" {
		cfg.ElectricImport = DefaultElectricModule
	}

	e := &emitter{doc: doc, cfg: cfg}
	e.reachable = e.reach()
	e.home = e.placements()

	base, err := e.clientFile()
	if err != nil {
		return nil, err
	}
	artifacts := []gen.Artifact{base}

	for _, enum := range doc.API.Enums {
		// ErrorCode is the runtime's, and is re-exported from the base file
		// rather than declared here. Everything else is a column's type — and
		// only the ones a caller can actually receive.
		if enum.PgType == "" || !e.reachable[enum.Name] {
			continue
		}
		art, err := e.enumFile(enum)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, art)
	}

	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Unexposed || len(res.Endpoints) == 0 {
			continue
		}

		types, err := e.typesFile(res)
		if err != nil {
			return nil, err
		}
		inputs, err := e.inputFile(res)
		if err != nil {
			return nil, err
		}
		methods, err := e.methodFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, types, inputs, methods)

		if filters := e.filterObjects(res); len(filters) > 0 {
			art, err := e.filterFile(res, filters)
			if err != nil {
				return nil, err
			}
			artifacts = append(artifacts, art)
		}
	}

	if rest := e.unclaimedObjects(); len(rest) > 0 {
		art, err := e.objectFile(rest)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, art)
	}

	// Nothing at all until a table opts in, which is the same promise the
	// electric generator makes: leaving the block configured costs a project
	// that streams nothing neither a file nor a dependency.
	if streams := e.streamed(); len(streams) > 0 {
		art, err := e.electricFile(streams)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, art)
	}

	index, err := e.indexFile(artifacts)
	if err != nil {
		return nil, err
	}
	return append(artifacts, index), nil
}

// emitter carries what every file needs.
type emitter struct {
	doc *ir.Document
	cfg Options
	// reachable names every type a caller can actually send or receive. See
	// [emitter.reach].
	reachable map[string]bool
	// home says which module declares each type, so a file that mentions a
	// sibling's type can import it. Go needed nothing here — one package, no
	// imports — and this is the whole of what a second language costs.
	home map[string]string
	// cur is the module being emitted, so [emitter.ref] can tell a local name
	// from one that has to be imported. Set by each file method; the generator
	// emits one file at a time.
	cur string
}

// ref names a type, importing it when it lives in another module.
//
// Always `import type`, because everything it can name is a type: a value
// import would be a runtime dependency between two files that have no runtime
// between them, and a bundler could not drop either.
func (e *emitter) ref(b *tsbuf.Buf, name string) string {
	module, known := e.home[name]
	if known && module != e.cur {
		b.ImportType(module, name)
	}
	return name
}

// refValue names something that is called rather than only mentioned — a
// resource client's factory, which the entry point invokes to build the half of
// the client that resource is.
//
// A separate method rather than a flag, because getting it wrong is a specific
// and confusing error: TypeScript reports that the name "cannot be used as a
// value because it was imported using import type", which reads as a problem
// with the call rather than with the import above it.
func (e *emitter) refValue(b *tsbuf.Buf, name string) string {
	module, known := e.home[name]
	if known && module != e.cur {
		b.Import(module, name)
	}
	return name
}

// placements decides which module declares each type, before anything is
// emitted.
//
// It is a pass of its own because an import needs the answer for a name the
// emitter has not reached yet — the resource file mentions an enum, and the
// enum's file is written later. Deriving it twice, once here and once where the
// file is named, is how the two would come to disagree; [emitter.moduleFor] is
// the single answer both use.
func (e *emitter) placements() map[string]string {
	home := map[string]string{}

	for _, enum := range e.doc.API.Enums {
		if enum.PgType == "" || !e.reachable[enum.Name] {
			continue
		}
		home[enum.Name] = moduleFor(snake(enum.Name) + ".gen")
	}

	for _, res := range e.exposed() {
		self := moduleFor(snake(res.Name) + ".gen")
		home[res.Name] = self
		home[res.Name+"ListResponse"] = self

		query := moduleFor(snake(res.Name) + "_query.gen")
		for _, obj := range e.filterObjects(res) {
			home[obj.Name] = query
		}

		client := moduleFor(snake(res.Name) + "_client.gen")
		home[res.Name+"Client"] = client
		home["create"+res.Name+"Client"] = client

		input := moduleFor(snake(res.Name) + "_input.gen")
		if createWithFiles(res) != nil {
			home[createFilesTypeName(res)] = input
		}
		for i := range res.Endpoints {
			ep := &res.Endpoints[i]
			switch {
			case genutil.ModelInputName(ep) == ir.OpCreate:
				home[res.Name+"CreateInput"] = input
			case genutil.ModelInputName(ep) == ir.OpUpdate:
				home[res.Name+"UpdateInput"] = input
			case len(ep.Request.BodyParams) > 0 && ep.Request.BodyObject == "":
				home[bodyTypeName(res, ep)] = input
			}
			if name := genutil.FieldsTypeName(res, ep); name != "" {
				home[name] = input
			}
			if len(ep.Request.QueryParams) > 0 {
				home[genutil.QueryTypeName(res, ep)] = input
			}
		}
	}

	for _, obj := range e.unclaimedObjects() {
		home[obj.Name] = moduleFor("objects.gen")
	}

	// A streamed resource's row type sits with the resource, or — for a table
	// with no API surface at all — with the stream factories that are the only
	// thing that mentions it.
	for _, res := range e.streamed() {
		if _, exposed := home[res.Name]; exposed {
			home[res.Name+"Row"] = home[res.Name]
			continue
		}
		home[res.Name+"Row"] = moduleFor("electric.gen")
	}

	return home
}

// moduleFor is the specifier a sibling module is imported by.
//
// The `.js` extension on a `.ts` file is not a mistake: under
// `moduleResolution: bundler` it is optional and under `node16` it is required,
// and the form that works under both is the one to emit.
func moduleFor(stem string) string { return "./" + stem + ".js" }

// snake is the file-name form of a type name.
func snake(name string) string { return naming.Snake(name) }

// reach names every type a front end can send or receive: what the client's own
// methods mention, what a stream's rows carry, and what those mention in turn.
//
// The walk is [genutil.Walk]; what is here is the seeding. This generator's
// differs from the Go client's by the streamed half below.
func (e *emitter) reach() map[string]bool {
	w := genutil.NewWalk(e.doc)

	w.Follow("Error")
	w.Follow("Pagination")

	for _, res := range e.exposed() {
		w.Follow(res.Name)
		w.Follow(res.Name + "ListResponse")

		for i := range res.Endpoints {
			w.Endpoint(&res.Endpoints[i])
		}
	}

	// A streamed table need not be exposed at all — rig's own are not — and its
	// row type is emitted from its columns rather than from an object, so
	// nothing above reaches what those columns carry. Without this an enum
	// column on an unexposed streamed table is named by `electric.gen.ts` and
	// declared by nothing: no enum file, no import, and a client that does not
	// compile.
	for _, res := range e.streamed() {
		w.Fields(genutil.PlainFields(streamFields(res)))
	}
	return w.Seen()
}

// streamFields are the columns a stream's row type carries: the shape's
// projection, which is the resource's readable columns.
//
// One answer for both the type and [emitter.reach], because the two have to
// agree exactly. A column the type names and the reach misses is a name nothing
// declares; one the reach follows and the type omits is a file emitted for
// nobody.
func streamFields(res *ir.Resource) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range res.Fields {
		if f.Column == nil || !f.In(ir.FieldOpRead) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// exposed is every resource with an API surface.
func (e *emitter) exposed() []*ir.Resource { return genutil.Exposed(e.doc) }

// streamed is every resource with a live-sync shape.
//
// It reads Electric rather than Operations, because live sync is its own read
// surface: a table with no operations at all still gets shapes, which is how
// rig's own unexposed tables are subscribed to. Such a table has no REST types
// here either, so the row type the stream needs is emitted beside the factory.
func (e *emitter) streamed() []*ir.Resource {
	var out []*ir.Resource
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if res.Electric == nil {
			continue
		}
		out = append(out, res)
	}
	return out
}

// object finds a declared object by name.
func (e *emitter) object(name string) *ir.Object { return e.doc.Object(name) }

// tsType renders a field's TypeScript type, without its nullability.
//
// Every primitive that is not a number or a boolean is a string, and each one is
// a string for a reason worth keeping straight. A UUID, a Date, a Time and a
// Timestamp are strings because that is what JSON has; a Decimal is a string
// because a `number` would silently round the value the column exists to keep
// exact; and Bytes is base64, which is what `encoding/json` does with a byte
// slice. JSON is `unknown` rather than `any`, so a caller has to narrow it
// before use — `any` here would switch off the compiler for every field that
// touched it.
func (e *emitter) tsType(b *tsbuf.Buf, f ir.Field) string {
	base := "unknown"
	switch f.TypeKind {
	case ir.TypeKindEnum, ir.TypeKindObject, ir.TypeKindResource:
		base = e.ref(b, f.Type)
	case ir.TypeKindPrimitive:
		base = primitive(f.Type)
	}
	if f.IsArray() {
		return base + "[]"
	}
	return base
}

func primitive(name string) string {
	switch name {
	case ir.TypeBool:
		return "boolean"
	case ir.TypeInt, ir.TypeInt64, ir.TypeFloat64:
		return "number"
	case ir.TypeJSON:
		return "unknown"
	case ir.TypeBytes, ir.TypeDate, ir.TypeDecimal, ir.TypeString,
		ir.TypeTime, ir.TypeTimestamp, ir.TypeUUID:
		return "string"
	default:
		return "unknown"
	}
}

// member renders one member of a read type: the key, the optionality, the type.
//
// A nullable field is `?: T | null` rather than `: T | null`. The server marks
// those `omitempty`, so a null column arrives as an absent key and not as a null
// — and the `| null` is there anyway, because a type that forbids the value the
// column can hold would refuse a server that stopped omitting it.
func (e *emitter) member(b *tsbuf.Buf, f ir.Field) string {
	t := e.tsType(b, f)
	if f.IsNullable() {
		return tsbuf.Key(f.Wire) + "?: " + t + " | null;"
	}
	return tsbuf.Key(f.Wire) + ": " + t + ";"
}

// namer builds identifiers the way the document does, so a plural overridden in
// rig.yaml is the plural here too.
func namer() *naming.Namer { return naming.New(naming.Config{}) }

// methodName is what one endpoint is called on its resource client.
//
// From the operation identifier rather than from the endpoint's name, so the
// method a caller writes is the operation the document, the OpenAPI file and the
// server's logs all call it — minus the resource, which is the property it hangs
// off. `listTodos` on `client.todos` is `list`.
func methodName(ep *ir.Endpoint) string {
	name := ep.Name
	if name == "" {
		name = ep.OperationID
	}
	return lowerFirst(name)
}

// clientProperty is what a resource is reached as on the client: `todos`,
// `todoAttachments`.
func clientProperty(res *ir.Resource) string {
	return lowerFirst(namer().Plural(res.Name))
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// artifact renders a buffer into a file.
func (e *emitter) artifact(path string, b *tsbuf.Buf) (gen.Artifact, error) {
	content, err := b.Bytes()
	if err != nil {
		return gen.Artifact{}, err
	}
	return gen.Artifact{Path: path, Content: content, Mode: gen.Overwrite}, nil
}

// describe is the document's description, or the fallback when it has none.
func describe(description, fallback string) string {
	return genutil.Describe(description, fallback)
}
