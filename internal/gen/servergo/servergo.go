// Package servergo generates the HTTP layer: routing, decoding, and the
// registration struct.
//
// Routes come straight from the document's precomputed patterns, so the router,
// the specification, and the client cannot disagree about a path. Registration
// goes through a struct with one field per resource, which makes forgetting to
// wire a new table a compile error rather than a 404 nobody notices until a
// client reports it.
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
