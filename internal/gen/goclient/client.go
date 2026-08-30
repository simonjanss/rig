package goclient

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// clientFile emits the client itself: what a caller builds, what it is made of,
// and the vocabulary every call shares.
func (e *emitter) clientFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)
	api := e.doc.API

	b.Doc("Package " + e.cfg.Package + " is the generated Go client for the " +
		api.Name + " API, version " + api.Version + ".\n\n" +
		genutil.Describe(api.Description, "") + "\n\n" +
		"One method per endpoint, grouped by resource: c.New(...) then " +
		"c.<Resource>.<Operation>(ctx, ...). The types are this package's; " +
		"everything underneath a method — the request, the credential, the " +
		"failure — is rigclient's, and a failure arrives as *rigclient.Error " +
		"whose Code says which of them it was.")

	rig := e.client(b)

	b.Comment("BasePath is the prefix every route sits under. It is the " +
		"document's, not a setting: a client that could be pointed at a " +
		"different one would be a client for a different API.")
	b.L("const BasePath = %s", gobuf.Quote(api.BasePath))
	b.NL()

	b.Comment("Revision is the date the API surface this client was generated " +
		"from last changed.\n\n" +
		"Every request says it, so the server's logs can answer the question you " +
		"have to answer before removing anything: how old is the oldest client " +
		"still calling. Regenerating against an unchanged API leaves it alone — " +
		"it is not a build stamp.")
	b.L("const Revision = %s", gobuf.Quote(api.Revision))
	b.NL()

	b.Comment("RevisionHeader is where [Revision] is sent: the same header the " +
		"server generated from this document reads.")
	b.L("const RevisionHeader = %s", gobuf.Quote(genutil.RevisionHeader(e.doc)))
	b.NL()

	e.serverConstants(b)

	e.clientStruct(b, rig)
	e.errorVocabulary(b, rig)

	if obj := e.object("Pagination"); obj != nil {
		e.objectStruct(b, obj)
	}

	return e.artifact("client.gen.go", b)
}

// exposed are the resources with endpoints to call.
func (e *emitter) exposed() []*ir.Resource { return genutil.Exposed(e.doc) }

// clientStruct emits the client and its constructor.
func (e *emitter) clientStruct(b *gobuf.Buf, rig string) {
	resources := e.exposed()

	b.Comment("Client is the API. One field per resource, so what a client can " +
		"do is what the schema says it can, and reaching for a resource that " +
		"does not exist is a compile error rather than a 404.")
	b.L("type Client struct {")
	b.L("rt *%s.Runtime", rig)
	b.NL()
	for _, res := range resources {
		b.Comment(genutil.Describe(res.Description, res.Plural+" of this API."))
		b.L("%s *%sClient", res.Plural, res.Name)
	}
	if e.doc.API.Auth != nil {
		b.NL()
		b.Comment("Auth is rig's own authentication: signing in, refreshing, " +
			"tenants, sessions and API keys. It is not generated — the routes are " +
			"the same in every project — but what it knows about this one is, in " +
			"[AuthProfile].")
		b.L("Auth *%s.Auth", rig)
	}
	b.L("}")
	b.NL()

	_, hasDefault := genutil.DefaultServer(e.servers())
	if hasDefault {
		b.Comment("New builds a client.\n\n" +
			"Config.BaseURL is optional: left empty it is DefaultBaseURL, the " +
			"deployment rig.yaml marks as the default. A caller pointing somewhere " +
			"else — a mock server, a local build — sets it and gets exactly what " +
			"they set. A credential can be given here or installed later by " +
			"signing in.")
	} else {
		b.Comment("New builds a client.\n\n" +
			"The only required setting is Config.BaseURL. A credential can be given " +
			"here or installed later by signing in.")
	}
	b.L("func New(cfg %s.Config) (*Client, error) {", rig)
	if hasDefault {
		b.L("if cfg.BaseURL == \"\" {")
		b.L("cfg.BaseURL = DefaultBaseURL")
		b.L("}")
	}
	b.L("rt, err := %s.New(cfg, %s.API{", rig, rig)
	b.L("BasePath: BasePath,")
	b.L("Revision: Revision,")
	b.L("RevisionHeader: RevisionHeader,")
	if e.doc.API.Auth != nil {
		b.L("Auth: &AuthProfile,")
	}
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.NL()
	b.L("c := &Client{rt: rt}")
	for _, res := range resources {
		b.L("c.%s = &%sClient{rt: rt}", res.Plural, res.Name)
	}
	if e.doc.API.Auth != nil {
		b.L("c.Auth = rt.Auth()")
	}
	b.L("return c, nil")
	b.L("}")
	b.NL()

	b.Comment("Runtime is the transport underneath, for the rare call that has " +
		"no generated method: an endpoint added by hand to the same server, say.")
	b.L("func (c *Client) Runtime() *%s.Runtime { return c.rt }", rig)
	b.NL()
}

// errorVocabulary emits what a caller switches on when a call fails.
func (e *emitter) errorVocabulary(b *gobuf.Buf, rig string) {
	rigerr := b.Import(genutil.RuntimeModule + "/rigerr")

	if obj := e.object("Error"); obj != nil {
		b.Comment(genutil.Describe(obj.Description, "Returned whenever a request fails.") +
			"\n\nIt is rigclient's type rather than one of this package's, so that " +
			"a program talking to two rig APIs handles one kind of failure. " +
			"errors.As reaches it, and Code below is what to branch on.")
		b.L("type Error = %s.Error", rig)
		b.NL()
	}

	b.Comment("ErrorCode is the machine-readable reason a request failed. " +
		"Clients switch on this rather than on the status, because three " +
		"unrelated failures share a status and none of them share a code.")
	b.L("type ErrorCode = %s.Code", rigerr)
	b.NL()

	if enum := e.errorCodeEnum(); enum != nil {
		b.L("// The codes a client may see.")
		b.L("const (")
		for _, v := range enum.Values {
			if v.Description != "" {
				b.Comment(v.Description)
			}
			b.L("ErrorCode%s ErrorCode = %s.Code%s", v.Name, rigerr, v.Name)
		}
		b.L(")")
		b.NL()
	}
}

// errorCodeEnum is the closed set of failure reasons, or nil for a document that
// somehow has none.
func (e *emitter) errorCodeEnum() *ir.Enum { return e.doc.Enum("ErrorCode") }

// servers are the deployments this client knows about.
//
// The project's list first. The deprecated default_base_url option is read only
// when the project named none, and it arrives here as the nameless one-entry
// case it always was: the constant it produces is the one it has always
// produced. What it gains is the fallback in New — the comment beside
// DefaultBaseURL has always said a caller can leave Config.BaseURL empty, and
// now that is true wherever the default came from.
func (e *emitter) servers() []ir.Server {
	if named := genutil.Servers(e.doc); len(named) > 0 {
		return named
	}
	if e.cfg.DefaultBaseURL == "" {
		return nil
	}
	return []ir.Server{{URL: e.cfg.DefaultBaseURL, Default: true}}
}

// serverConstants emits the deployments and the default among them.
//
// One comment per constant rather than one over the block: a doc comment above
// several one-line declarations documents only the first, and the rest arrive on
// pkg.go.dev undocumented.
func (e *emitter) serverConstants(b *gobuf.Buf) {
	servers := e.servers()
	if len(servers) == 0 {
		return
	}

	named := make([]ir.Server, 0, len(servers))
	for _, s := range servers {
		if s.Name != "" {
			named = append(named, s)
		}
	}

	if len(named) > 0 {
		b.Comment("The deployments this API is served on, as rig.yaml names them.\n\n" +
			"A caller that talks to one of them names it here rather than writing " +
			"the URL down: the string is the project's, and it moves when the " +
			"project does. A caller pointing at a mock server or a local build " +
			"passes their own Config.BaseURL, and nothing here argues.")
		b.L("const (")
		for i, s := range named {
			if i > 0 {
				b.NL()
			}
			b.Comment(serverDescription(s))
			b.L("%s = %s", serverConstName(s.Name), gobuf.Quote(s.URL))
		}
		b.L(")")
		b.NL()
	}

	def, ok := genutil.DefaultServer(servers)
	if !ok {
		return
	}
	b.Comment("DefaultBaseURL is the deployment rig.yaml marks as the default, so " +
		"a tool that only ever talks to that one can leave Config.BaseURL empty.")
	if def.Name != "" {
		b.L("const DefaultBaseURL = %s", serverConstName(def.Name))
	} else {
		b.L("const DefaultBaseURL = %s", gobuf.Quote(def.URL))
	}
	b.NL()
}

// serverConstName is what a deployment is called in Go.
//
// The Server prefix groups the set in godoc and in an editor's completion list,
// and keeps a deployment named after a resource from colliding with the
// generated type of the same name.
func serverConstName(name string) string {
	var out strings.Builder
	out.WriteString("Server")
	for _, word := range strings.Split(name, "_") {
		if word == "" {
			continue
		}
		out.WriteString(strings.ToUpper(word[:1]))
		out.WriteString(word[1:])
	}
	return out.String()
}

// serverDescription is the sentence a deployment's constant carries.
//
// The project's own prose when it wrote any, standing on its own rather than
// spliced after the constant's name — the same way a resource's comment is used,
// and the reason ST1020 is off in .golangci.yml. A derived sentence otherwise,
// so an entry with no prose still documents itself.
func serverDescription(s ir.Server) string {
	if s.Description != "" {
		return s.Description
	}
	return "The " + s.Name + " deployment of this API."
}
