// Package servergo generates the HTTP layer: routing, decoding, and the
// registration struct.
//
// Routes come straight from the document's precomputed patterns, so the router,
// the specification, and the client cannot disagree about a path. Registration
// goes through a struct with one field per resource, which makes forgetting to
// wire a new table a compile error rather than a 404 nobody notices until a
// client reports it.
//
// A project with an `auth:` block gets one more file in the same package: the
// call that hands rig/auth exactly what that block says, so how long a token
// lives is a line in rig.yaml rather than a literal in a main function nobody
// diffs. The foundation itself is not generated — its endpoints, stores and
// hashing all live in the rig/auth module — and what is left in Go is what a
// configuration file cannot hold: a function, and a secret.
//
// It is written here rather than by a generator of its own because it belongs
// to the same package: the error mapper it reaches for is this one's, so an
// authentication failure is shaped like every other failure without an import
// path in a configuration file to say where to find it. A project with no auth
// block gets no file at all, which is what keeps its API package — and its
// module — free of rig/auth.
package servergo

import (
	"context"
	"fmt"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func init() { gen.Register(New()) }

const runtimeModule = "github.com/simonjanss/rig/runtime"

// Options configure the generator.
type Options struct {
	// Package is the Go package the generated files declare. It is normally the
	// same package service-go writes into.
	Package string `json:"package"`
	// ModelImport is the import path of the generated model package, whose
	// input types the create and update handlers decode into.
	ModelImport string `json:"model_import"`

	// RequestIDHeader is the header the generated authentication error mapper
	// reads a request identifier from, so a failure from the sign-in endpoint
	// carries the same identifier as a failure from anywhere else.
	//
	// It only matters for a project with an auth block. Empty means
	// [DefaultRequestIDHeader].
	RequestIDHeader string `json:"request_id_header"`

	// ElectricURL is the sync service the shape endpoints proxy to. It becomes
	// the DefaultElectricURL a wiring can override, so a deployment can point at
	// a different one without regenerating.
	//
	// It only matters for a project where some table asked for live sync.
	ElectricURL string `json:"electric_url"`

	// ElectricRequired makes a sync service that is not answering a refusal to
	// start rather than a line in the log.
	//
	// False, the default, is a server that boots, says once that live sync is
	// not there, and serves — because that is what most projects want: a shape
	// with a fallback answers from the database, and every route that is not a
	// shape never touched the sync service at all.
	//
	// True is for the application whose pages are shapes, where starting is a
	// server that answers 502 to everything that matters. It also makes the sync
	// service part of the readiness check, so an instance that loses it is taken
	// out of the load balancer rather than left in it answering nothing.
	//
	// It only matters for a project where some table asked for live sync.
	ElectricRequired bool `json:"electric_required"`

	// StubDir is where the hand-owned shape scoping files go, relative to the
	// project root. {table} and {Table} are substituted. Empty writes no stubs.
	//
	// The same directory service-go writes its service stub into: everything
	// about one table in one place.
	StubDir string `json:"stub_dir"`
	// StubPackage names the package a shape stub declares. Empty uses the table
	// name.
	StubPackage string `json:"stub_package"`
	// APIImport is the import path of this generated package, so a shape stub in
	// another directory can name the scope type it satisfies.
	APIImport string `json:"api_import"`
}

// Generator emits the HTTP layer.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "server-go" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "net/http routing, request decoding, and the handler registration struct"
}

// Version implements [gen.Generator].
func (*Generator) Version() string { return "4" }

// Generate implements [gen.Generator].
func (g *Generator) Generate(_ context.Context, doc *ir.Document, opts gen.Options) ([]gen.Artifact, error) {
	cfg, err := gen.Decode[Options](opts)
	if err != nil {
		return nil, err
	}
	if cfg.Package == "" {
		cfg.Package = "api"
	}
	if cfg.ModelImport == "" {
		return nil, fmt.Errorf("model_import is required: create and update decode into the model's inputs")
	}
	if cfg.RequestIDHeader == "" {
		cfg.RequestIDHeader = DefaultRequestIDHeader
	}
	if cfg.StubDir != "" && cfg.APIImport == "" {
		// Before anything is written rather than after. A scoping stub names the
		// scope type it satisfies, which lives in this package, and one emitted
		// without a path to it is a file that does not build — and CreateOnce means
		// the next run leaves it exactly as it is.
		return nil, fmt.Errorf("api_import is required alongside stub_dir: a shape's scoping stub names the scope type this package declares")
	}

	e := &emitter{doc: doc, cfg: cfg, root: opts.Root, namer: naming.New(naming.Config{})}

	server, err := e.serverFile()
	if err != nil {
		return nil, err
	}
	artifacts := []gen.Artifact{server}

	if e.hasPermissions() {
		permissions, err := e.permissionsFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, permissions)
	}

	for _, res := range e.resources() {
		handlers, err := e.handlerFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, handlers)
	}

	// The authentication wiring, when there is any to write. A project with no
	// auth block gets no file, which is also what keeps its API package — and so
	// its module — free of rig/auth: examples/todo serves a list of chores
	// without depending on argon2.
	if doc.API.Auth != nil {
		auth, err := (&authEmitter{doc: doc, auth: doc.API.Auth, cfg: cfg}).authFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, auth)
	}

	// The migration wiring, for a project whose modules carry their own schema.
	// A project that vendored the foundation gets no file: its migrations are
	// already files in its own directory, and this is the only thing in the API
	// package that would name rig/migrate, so not writing it is what keeps goose
	// out of the module.
	if doc.API.EmbeddedFoundation {
		foundation, err := (&foundationEmitter{doc: doc, cfg: cfg}).foundationFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, foundation)
	}

	// The file wiring, when there is any to write. A project with no `files:`
	// block gets no file, which is what keeps its API package — and so its
	// module — free of a blob store and a multipart reader.
	if e.hasFiles() {
		files, err := e.filesFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, files)
	}

	// The tracing configuration, for a project that asked for spans. Same rule
	// again: without the block there is no file, no import of rig/observe, and
	// no OpenTelemetry anywhere in the application's dependency graph.
	if e.tracing() {
		tracing, err := e.tracingFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, tracing)
	}

	// The monitoring page's configuration, for a project that asked for the
	// page. It cannot be reached without the block above, which is what the
	// compiler refuses: the page reads the span file tracing writes.
	if e.monitoring() {
		monitoring, err := e.monitoringFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, monitoring)
	}

	// The inbox wiring, when there is an inbox. A project with no
	// `notifications:` block gets no dispatcher and no routes.
	if e.hasNotifications() {
		notifications, err := e.notificationsFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, notifications)
	}

	// The presence wiring, when the project tracks it. Same rule again: without
	// the block there is no file, no import of rig/presence, and no sweeper.
	if e.hasPresence() {
		presence, err := e.presenceFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, presence)
	}

	// The live-sync shapes, when any table asked for one: the struct an
	// application fills in, one file of endpoints per table, and a scoping stub
	// beside each table's service.
	//
	// These were a generator of their own writing a package of their own, on the
	// grounds that a project might want live sync without an HTTP layer. No
	// project ever did, and the two were already coupled in both directions —
	// the drain's limit is a term of ShutdownBudget, which lives here. What the
	// separation actually bought was a second Server with its own claims lookup
	// and its own error writer, which is to say a second place for either to be
	// wrong.
	if e.hasShapes() {
		shapes, err := e.electricFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, shapes)

		for _, res := range e.shapes() {
			file, err := e.shapeFile(res)
			if err != nil {
				return nil, err
			}
			artifacts = append(artifacts, file)

			// No scope stub for a table rig created, for the reason the service
			// generator gives: it would be a file about somebody else's table, and
			// the three in this repository's own examples were byte-identical
			// `return nil`s. A project that does want to narrow one — presence is
			// the case that earns it, where every heartbeat reaches every
			// subscriber — writes the file, and CreateOnce then leaves it alone.
			if cfg.StubDir == "" || res.Foundation {
				continue
			}
			for _, sh := range e.shapesFor(res) {
				stub, err := e.shapeStubFile(res, sh)
				if err != nil {
					return nil, err
				}
				artifacts = append(artifacts, stub)
			}
		}
	}

	// The process around the server, as far as this project's configuration
	// decided it: the subcommands it can name itself, the shutdown its own steps
	// add up to, and — for a project that traces — the log sink, the provider and
	// the page as one object with the order between them settled.
	//
	// Written for every project, unlike the six conditional files above, because
	// every project has at least one task to merge into. Everything in it that
	// names rig/observe is behind the same predicate tracing.gen.go is, so a
	// project that asked for no spans gets a file with no import of one.
	process, err := e.processFile()
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, process)

	// The order those parts come to exist in, which is the one thing above that
	// was still a main function's to write. It is generated last because it is
	// generated out of the others: the struct it names has a field per lifetime
	// the six conditional files register a shutdown for.
	run, err := e.runFile()
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, run)

	return artifacts, nil
}

type emitter struct {
	doc   *ir.Document
	cfg   Options
	root  string
	namer *naming.Namer
}

// model imports the model package and returns its qualifier.
func (e *emitter) model(b *gobuf.Buf) string { return b.Import(e.cfg.ModelImport) }

func artifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	return buffered(path, b, gen.Overwrite)
}

// stubArtifact is a file rig writes once and then leaves alone. Everything else
// this generator emits carries .gen. in its name and is rewritten every run;
// these do not, and are the developer's from the moment they exist.
func stubArtifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	return buffered(path, b, gen.CreateOnce)
}

func buffered(path string, b *gobuf.Buf, mode gen.WriteMode) (gen.Artifact, error) {
	content, err := b.Bytes()
	if err != nil {
		return gen.Artifact{}, fmt.Errorf("%s: %w", path, err)
	}
	return gen.Artifact{Path: path, Content: content, Mode: mode}, nil
}
