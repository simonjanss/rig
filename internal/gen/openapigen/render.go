package openapigen

import (
	"bytes"
	"fmt"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// document assembles the whole model.
//
// Paths are built before components on purpose: walking the operations is what
// records which error statuses are actually reachable and which body shapes had
// to be named, and components is where both of those land. An unused component
// is a lint finding and, worse, a document describing a failure that cannot
// happen.
func (e *emitter) document() *v3.Document {
	paths := e.paths()

	doc := &v3.Document{
		Version: specVersionFull,
		Info:    e.info(),
		Servers: e.servers(),
		Tags:    e.tags(),
		Paths:   paths,
		Components: &v3.Components{
			Schemas:    e.schemas(),
			Responses:  e.errorResponses(),
			Parameters: e.sharedParameters(),
			Headers:    e.sharedHeaders(),
		},
	}

	if schemes := e.securitySchemes(); schemes != nil {
		doc.Components.SecuritySchemes = schemes
		doc.Security = requireCredential()
	}
	return doc
}

// info is the document's own identity.
func (e *emitter) info() *base.Info {
	api := e.doc.API

	version := api.Revision
	if version == "" {
		// A project that has never generated with a revision recorded still
		// needs a version here — it is required — and the path segment is the
		// only other thing that identifies this surface.
		version = api.Version
	}

	// A description is not optional in practice: it is the first thing a
	// reader sees, and a linter treats its absence as an error. A project that
	// wrote none still gets a true sentence rather than an empty key.
	desc := genutil.Describe(api.Description,
		"The "+genutil.Describe(api.Name, "rig")+" HTTP API.")
	if extra := e.undescribed(); extra != "" {
		desc = join(desc, extra)
	}

	return &base.Info{
		Title:       genutil.Describe(api.Name, "API"),
		Description: desc,
		Version:     version,
	}
}

// undescribed says what this document leaves out, so an omission is something a
// reader can see rather than something they discover.
//
// The routes the authentication module and the notification inbox mount are
// hand-written: they reach no IR endpoint, so nothing here can describe them
// without inventing the description. Saying so costs a sentence.
func (e *emitter) undescribed() string {
	var out string
	if e.doc.API.Auth != nil {
		out = join(out, "The authentication endpoints under `"+e.doc.API.Auth.BasePath+
			"` are served by a hand-written module and are not described here.")
	}
	if n := e.doc.API.Notifications; n != nil && n.Enabled {
		out = join(out, "The notification inbox endpoints are served by a hand-written "+
			"module and are not described here.")
	}
	return out
}

// servers are the origins the API answers on.
//
// The project's `servers:` block first: the document and every generated SDK
// have to agree about where the API is, and that block is the one place that
// says so. The deprecated openapi.servers option is read only when the project
// named none, and the relative fallback only when neither did.
//
// The default deployment is emitted first, whatever its position in the file. A
// viewer sends its trial request to the first entry, and a document whose "try
// it" went to staging while the SDK beside it defaulted to production would be
// exactly the disagreement the block exists to prevent.
func (e *emitter) servers() []*v3.Server {
	named := genutil.Servers(e.doc)
	if len(named) == 0 {
		out := make([]*v3.Server, 0, len(e.cfg.Servers))
		for _, s := range e.cfg.Servers {
			out = append(out, &v3.Server{URL: s.URL, Description: s.Description})
		}
		return out
	}

	ordered := make([]ir.Server, 0, len(named))
	for _, s := range named {
		if s.Default {
			ordered = append(ordered, s)
		}
	}
	for _, s := range named {
		if !s.Default {
			ordered = append(ordered, s)
		}
	}

	out := make([]*v3.Server, 0, len(ordered))
	for _, s := range ordered {
		out = append(out, &v3.Server{URL: s.URL, Description: serverDescription(s)})
	}
	return out
}

// serverDescription is what the document says about a deployment.
//
// It falls back to the name, so `- name: staging` with no prose still reads as
// something in a viewer rather than as a bare URL.
func serverDescription(s ir.Server) string {
	if s.Description != "" {
		return s.Description
	}
	if s.Name == "" {
		return ""
	}
	return "The " + s.Name + " deployment of this API."
}

// tags group the operations, one per resource that produced any.
//
// Built from what was actually emitted rather than from the resource list: a
// resource whose only endpoint was a QUERY with no alias contributes no
// operation, and a tag nothing references is an unused component.
func (e *emitter) tags() []*base.Tag {
	var out []*base.Tag
	for _, res := range e.exposed() {
		described := e.syncs(res)
		var skipped []string
		for i := range res.Endpoints {
			ep := &res.Endpoints[i]
			if len(routesOf(ep)) > 0 {
				described = true
				continue
			}
			skipped = append(skipped, ep.Name)
		}
		if !described {
			continue
		}

		desc := genutil.Describe(res.Description, "Operations on "+res.Plural+".")
		for _, name := range skipped {
			desc = join(desc, name+" on "+res.Plural+" is served as `"+methodQuery+" "+
				res.Name+"` only. OpenAPI 3.1 cannot describe an operation on the QUERY "+
				"method, and this project did not ask for the POST alias, so the operation "+
				"is absent from this document rather than misdescribed.")
		}
		out = append(out, &base.Tag{Name: res.Plural, Description: desc})
	}
	return out
}

// render writes the document in each configured format.
//
// Both come from one model. Building it twice would be two chances to differ,
// and the difference would be invisible until somebody compared the files.
func (e *emitter) render(model *v3.Document) ([]gen.Artifact, error) {
	var out []gen.Artifact
	for _, format := range e.cfg.Formats {
		switch format {
		case "yaml":
			b, err := model.Render()
			if err != nil {
				return nil, fmt.Errorf("openapi: render yaml: %w", err)
			}
			out = append(out, gen.Artifact{
				Path: "openapi.gen.yaml", Content: newlineTerminated(b), Mode: gen.Overwrite,
			})
		case "json":
			b, err := model.RenderJSON("  ")
			if err != nil {
				return nil, fmt.Errorf("openapi: render json: %w", err)
			}
			out = append(out, gen.Artifact{
				Path: "openapi.gen.json", Content: newlineTerminated(b), Mode: gen.Overwrite,
			})
		}
	}
	return out, nil
}

// newlineTerminated ends a file the way every other file in the tree ends.
func newlineTerminated(b []byte) []byte {
	if len(b) > 0 && bytes.HasSuffix(b, []byte("\n")) {
		return b
	}
	return append(b, '\n')
}
