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
func (*Generator) Version() string { return "1" }

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

	e := &emitter{doc: doc, cfg: cfg, namer: naming.New(naming.Config{})}

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

	// The file wiring, when there is any to write. A project with no `files:`
	// block gets no file, which is what keeps its API package — and so its
	// module — free of a blob store and a multipart reader.
	if doc.API.Files != nil && doc.API.Files.Enabled {
		files, err := e.filesFile()
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, files)
	}

	return artifacts, nil
}

type emitter struct {
	doc   *ir.Document
	cfg   Options
	namer *naming.Namer
}

// model imports the model package and returns its qualifier.
func (e *emitter) model(b *gobuf.Buf) string { return b.Import(e.cfg.ModelImport) }

func artifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	content, err := b.Bytes()
	if err != nil {
		return gen.Artifact{}, fmt.Errorf("%s: %w", path, err)
	}
	return gen.Artifact{Path: path, Content: content, Mode: gen.Overwrite}, nil
}
